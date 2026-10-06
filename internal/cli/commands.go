package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"box/examples"
	"box/internal/boxfile"
	"box/internal/runtime"
	"box/internal/sandbox"
)

func newCmd() *cobra.Command {
	var from string
	c := &cobra.Command{
		Use: "new <name>", Short: "write a Boxfile", GroupID: groupSandbox,
		Long: "Creates a sandbox with a starter Boxfile.\n\n" +
			"--from starts from a shipped example instead; any files that come\n" +
			"with it (a sample main.tf, say) are placed in its /data.\n" +
			"Examples: " + strings.Join(examples.Names(), ", "),
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeNothing,
		Example: "  box new devbox\n" +
			"  box new agent --from tiny-python",
		RunE: func(_ *cobra.Command, args []string) error {
			content := []byte(boxfile.Template)
			var extras map[string][]byte
			if from != "" {
				var err error
				if content, extras, err = examples.Load(from); err != nil {
					return err
				}
			}
			if _, err := sandbox.Create(args[0]); err != nil {
				return err
			}
			path, err := sandbox.BoxfilePath(args[0])
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, content, 0o644); err != nil {
				return err
			}
			data, err := sandbox.DataDir(args[0])
			if err != nil {
				return err
			}
			for name, b := range extras {
				if err := os.WriteFile(filepath.Join(data, name), b, 0o644); err != nil {
					return err
				}
			}
			fmt.Println(path)
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "start from a shipped example")
	c.RegisterFlagCompletionFunc("from", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return matching(examples.Names(), toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	return c
}

func buildCmd() *cobra.Command {
	return &cobra.Command{
		Use: "build <name>", Short: "build image, verify isolation", GroupID: groupSandbox,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := loadSpec(args[0])
			if err != nil {
				return err
			}
			if err := runtime.PreflightFor(s); err != nil {
				return err
			}
			// Waiting VMs were booted from the image being replaced.
			if err := runtime.Drain(args[0]); err != nil {
				return err
			}
			if err := runtime.Build(args[0], s); err != nil {
				return err
			}
			// Verify at build so a sandbox that is not really a VM never
			// reaches first use.
			if err := verify(args[0], s); err != nil {
				return err
			}
			if s.Warm > 0 {
				runtime.SpawnTender(args[0])
				fmt.Printf("warming %d VMs in the background\n", s.Warm)
			}
			return nil
		},
	}
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use: "verify <name>", Short: "re-check the kernel boundary", GroupID: groupInspect,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := loadSpec(args[0])
			if err != nil {
				return err
			}
			if err := runtime.PreflightFor(s); err != nil {
				return err
			}
			return verify(args[0], s)
		},
	}
}

// verify proves the sandbox got its own kernel, by comparing it against the
// kernel a plain container sees. Comparing against the host's own uname would
// be wrong on macOS, where the host runs Darwin and any Linux container kernel
// differs from it whether or not a microVM is involved.
func verify(name string, s boxfile.Spec) error {
	guest, baseline, err := runtime.CheckIsolation(name, s)
	if err != nil {
		return err
	}
	// Prime the cache so runs on this same runtime skip the double boot.
	runtime.MarkVerified(guest, baseline)
	fmt.Printf("isolated: sandbox %s, outside %s\n", guest, baseline)
	return nil
}

func runCmd() *cobra.Command {
	return &cobra.Command{
		Use: "run <name> [command...]", Short: "one command, fresh microVM", GroupID: groupRun,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: completeRunArgs,
		Example:           "  box run devbox -- python3 script.py",
		RunE: func(_ *cobra.Command, args []string) error {
			name, argv := args[0], args[1:]
			s, err := loadSpec(name)
			if err != nil {
				return err
			}
			err = runtime.RunFresh(name, s, argv, runtime.Terminal(), notice)
			switch {
			case err == nil:
				return nil
			case err == runtime.ErrTimeout:
				exitCode = runtime.ExitTimeout
				return fmt.Errorf("killed after %ds (timeout_seconds)", s.TimeoutSeconds)
			default:
				// A non-zero exit from the sandbox is its result, not our error.
				if code := runtime.ExitCode(err); code >= 0 {
					exitCode = code
					return nil
				}
				return err
			}
		},
	}
}

func shellCmd() *cobra.Command {
	return &cobra.Command{
		Use: "shell <name>", Short: "interactive session", GroupID: groupRun,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := loadSpec(args[0])
			if err != nil {
				return err
			}
			if err := runtime.PreflightFor(s); err != nil {
				return err
			}
			if fresh, err := runtime.EnsureIsolated(args[0], s); err != nil {
				return err
			} else if fresh {
				fmt.Fprintln(os.Stderr, "box: re-verified isolation (runtime changed since last check)")
			}
			if err := runtime.Shell(args[0], s); err != nil {
				if code := runtime.ExitCode(err); code >= 0 {
					exitCode = code
					return nil
				}
				return err
			}
			return nil
		},
	}
}

func resetCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "reset <name>", Short: "empty /data", GroupID: groupState,
		Long: "Empties /data, returning the sandbox to a clean state.\n" +
			"The sandbox and its image are untouched.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.Exists(name) {
				return fmt.Errorf("no sandbox %q", name)
			}
			if err := refuseWhileUp(name, "reset it"); err != nil {
				return err
			}
			if sandbox.IsFork(name) {
				return fmt.Errorf("%s is a fork; drop its changes with: box discard %s", name, name)
			}
			if err := refuseWithForks(name, "reset it"); err != nil {
				return err
			}
			// Only ask when there is actually something to lose.
			empty, err := sandbox.DataEmpty(name)
			if err != nil {
				return err
			}
			if !empty {
				if err := confirm(fmt.Sprintf("Delete everything in %s's /data?", name), yes); err != nil {
					return err
				}
			}
			// Waiting VMs hold the old /data mounted.
			if err := runtime.Drain(name); err != nil {
				return err
			}
			if err := sandbox.ResetData(name); err != nil {
				return err
			}
			runtime.SpawnTender(name)
			fmt.Printf("reset %s\n", name)
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func snapshotCmd() *cobra.Command {
	var list, yes bool
	c := &cobra.Command{
		Use: "snapshot <name> [label]", Short: "archive /data", GroupID: groupState,
		Long: "Archives /data to ~/.box/snapshots/<name>/.\n\n" +
			"With a label the archive is named for it instead of the time, so it\n" +
			"can be restored by that name rather than by a timestamp you would\n" +
			"have to look up. Reusing a label replaces that snapshot.\n\n" +
			"Restore one with: box restore <name> [snapshot]",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeName, // the label is invented, not chosen
		Example: "  box snapshot devbox\n" +
			"  box snapshot devbox before-upgrade\n" +
			"  box restore devbox before-upgrade",
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.Exists(name) {
				return fmt.Errorf("no sandbox %q", name)
			}
			if sandbox.IsFork(name) {
				return fmt.Errorf("%s is a fork; apply it, then snapshot its parent", name)
			}
			if list {
				snaps, err := sandbox.Snapshots(name)
				if err != nil {
					return err
				}
				for _, s := range snaps {
					fmt.Println(s)
				}
				return nil
			}
			label := time.Now().UTC().Format("20060102T150405Z")
			if len(args) == 2 {
				// A label typed with the suffix names the archive, not
				// "before-upgrade.tar.gz.tar.gz".
				label = strings.TrimSuffix(args[1], ".tar.gz")
				if err := sandbox.ValidLabel(label); err != nil {
					return err
				}
				// Reusing a label loses what that snapshot held, so ask first.
				if prev, err := sandbox.SnapshotPath(name, label); err == nil {
					if err := confirm(fmt.Sprintf("Replace the existing snapshot %s?",
						filepath.Base(prev)), yes); err != nil {
						return err
					}
				}
			}
			path, err := sandbox.Snapshot(name, label)
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
	c.Flags().BoolVarP(&list, "list", "l", false, "list existing snapshots instead")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func restoreCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "restore <name> [snapshot]", Short: "restore /data from a snapshot", GroupID: groupState,
		Long: "Replaces /data with the contents of a snapshot, the inverse of snapshot.\n" +
			"With no snapshot named, the most recent one is used.\n\n" +
			"Archive entries are checked before anything is unpacked, and the new\n" +
			"data is swapped in only once it is complete, so a failed restore\n" +
			"leaves the existing /data untouched.",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeSnapshot,
		Example: "  box restore devbox\n" +
			"  box restore devbox 20260823T150405Z",
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.Exists(name) {
				return fmt.Errorf("no sandbox %q", name)
			}
			ref := ""
			if len(args) == 2 {
				ref = args[1]
			}
			if err := refuseWhileUp(name, "restore it"); err != nil {
				return err
			}
			if sandbox.IsFork(name) {
				return fmt.Errorf("%s is a fork; restore its parent instead", name)
			}
			if err := refuseWithForks(name, "restore it"); err != nil {
				return err
			}
			archive, err := sandbox.SnapshotPath(name, ref)
			if err != nil {
				return err
			}
			// Only ask when there is actually something to overwrite.
			empty, err := sandbox.DataEmpty(name)
			if err != nil {
				return err
			}
			if !empty {
				if err := confirm(fmt.Sprintf("Replace %s's /data with %s?",
					name, filepath.Base(archive)), yes); err != nil {
					return err
				}
			}
			if err := runtime.Drain(name); err != nil {
				return err
			}
			if err := sandbox.Restore(name, archive); err != nil {
				return err
			}
			runtime.SpawnTender(name)
			fmt.Printf("restored %s from %s\n", name, filepath.Base(archive))
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func envCmd() *cobra.Command {
	return &cobra.Command{
		Use: "env <name>", Short: "settings as KEY=VALUE", GroupID: groupInspect,
		Long: "Shell-consumable settings, for: eval \"$(box env <name>)\"\n\n" +
			"Every value is single-quoted, so evaluating the output only assigns\n" +
			"variables. The Boxfile's env entries are printed as BOX_ENV_<KEY>,\n" +
			"so a Boxfile cannot overwrite PATH or anything else in your shell.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		Example:           "  eval \"$(box env devbox)\"",
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			s, err := loadSpec(name)
			if err != nil {
				return err
			}
			data, _ := sandbox.DataDir(name)
			for _, l := range envLines(name, data, s) {
				fmt.Println(l)
			}
			return nil
		},
	}
}

// envLines renders the settings printed by `box env`. The output is
// documented for eval, and base and env come from a Boxfile that may have
// been written by someone else, so every value is quoted and the Boxfile's
// keys are namespaced: evaluating the output assigns BOX_* variables and
// nothing else.
func envLines(name, data string, s boxfile.Spec) []string {
	lines := []string{
		"BOX_NAME=" + shellQuote(name),
		"BOX_IMAGE=" + shellQuote(sandbox.ImageTag(name)),
		"BOX_DATA=" + shellQuote(data),
		"BOX_BASE=" + shellQuote(s.Base),
		fmt.Sprintf("BOX_CPUS=%d", s.CPUs),
		fmt.Sprintf("BOX_RAM_MIB=%d", s.RAMMiB),
		"BOX_NETWORK=" + shellQuote(s.Network),
		fmt.Sprintf("BOX_PASST=%t", s.Passt),
		fmt.Sprintf("BOX_READONLY=%t", s.ReadOnlyRootfs),
		fmt.Sprintf("BOX_TIMEOUT_SECONDS=%d", s.TimeoutSeconds),
	}
	// EnvKeys is sorted, and validate() has already held each key to an
	// identifier, so the prefixed name is a valid shell variable.
	for _, k := range s.EnvKeys() {
		lines = append(lines, "BOX_ENV_"+k+"="+shellQuote(s.Env[k]))
	}
	return lines
}

// shellQuote makes s a single POSIX shell word that expands to exactly s.
// Inside single quotes nothing is special, so the only character to handle is
// the single quote itself: close the quote, emit an escaped one, reopen.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func logsCmd() *cobra.Command {
	var lines int
	c := &cobra.Command{
		Use: "logs <name>", Short: "recent runs", GroupID: groupInspect,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.Exists(name) {
				return fmt.Errorf("no sandbox %q", name)
			}
			p, err := sandbox.LogPath(name)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil // nothing run yet; nothing to say
			}
			all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(all) > lines {
				all = all[len(all)-lines:]
			}
			fmt.Println(strings.Join(all, "\n"))
			return nil
		},
	}
	c.Flags().IntVarP(&lines, "lines", "n", 200, "how many lines to show")
	return c
}

func renameCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rename <old> <new>", Short: "rename, keeping data", GroupID: groupRemove,
		Long: "Moves the definition, data, logs and snapshots, and retags the\n" +
			"built image so no rebuild is needed.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			if err := refuseWhileUp(args[0], "rename it"); err != nil {
				return err
			}
			if sandbox.IsFork(args[0]) {
				return fmt.Errorf("%s is a fork; apply or discard it, destroy it, and fork again under the new name", args[0])
			}
			if err := refuseWithForks(args[0], "rename it"); err != nil {
				return err
			}
			if err := sandbox.ValidName(args[1]); err != nil {
				return err
			}
			if err := runtime.RemovePool(args[0]); err != nil {
				return err
			}
			if err := sandbox.Rename(args[0], args[1]); err != nil {
				runtime.SpawnTender(args[0])
				return err
			}
			runtime.RetagImage(args[0], args[1])
			runtime.SpawnTender(args[1])
			fmt.Printf("%s -> %s\n", args[0], args[1])
			return nil
		},
	}
}

func destroyCmd() *cobra.Command {
	var withData, yes bool
	c := &cobra.Command{
		Use: "destroy <name>", Short: "remove one sandbox", GroupID: groupRemove,
		Long: "Removes the sandbox and its image. /data is kept unless --data,\n" +
			"since it is the only part a rebuild cannot reproduce.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.Exists(name) {
				return fmt.Errorf("no sandbox %q", name)
			}
			data, _ := sandbox.DataDir(name)
			if withData {
				empty, _ := sandbox.DataEmpty(name)
				if !empty {
					if err := confirm(fmt.Sprintf("Delete %s and its data?", name), yes); err != nil {
						return err
					}
				}
			}
			if err := refuseWithForks(name, "destroy it"); err != nil {
				return err
			}
			runtime.Down(name)
			runtime.RemovePool(name)
			runtime.RemoveFirecracker(name, withData)
			runtime.RemoveImage(name)
			if sandbox.IsFork(name) {
				if err := sandbox.RemoveFork(name); err != nil {
					return err
				}
			}
			if err := sandbox.Remove(name, withData); err != nil {
				return err
			}
			if withData {
				fmt.Printf("destroyed %s\n", name)
			} else {
				fmt.Printf("destroyed %s (data kept: %s)\n", name, data)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&withData, "data", false, "also delete /data and snapshots")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func nukeCmd() *cobra.Command {
	var noData, yes bool
	c := &cobra.Command{
		Use: "nuke", Short: "remove every sandbox", GroupID: groupRemove,
		Long: "Removes every sandbox, its image and its data.\n" +
			"Pass --no-data to keep the data directories.",
		Args:              cobra.NoArgs,
		ValidArgsFunction: completeNothing,
		RunE: func(_ *cobra.Command, _ []string) error {
			names, err := sandbox.List()
			if err != nil || len(names) == 0 {
				return nil // nothing to do
			}
			what := "and their data"
			if noData {
				what = "keeping their data"
			}
			if err := confirm(fmt.Sprintf("Remove %s (%s), %s?",
				plural(len(names), "sandbox", "sandboxes"),
				strings.Join(names, ", "), what), yes); err != nil {
				return err
			}
			for _, n := range names {
				runtime.Down(n)
				runtime.RemovePool(n)
				runtime.RemoveImage(n)
				sandbox.RemoveFork(n)
				if err := sandbox.Remove(n, !noData); err != nil {
					return fmt.Errorf("%s: %w", n, err)
				}
			}
			fmt.Printf("removed %s\n", plural(len(names), "sandbox", "sandboxes"))
			return nil
		},
	}
	c.Flags().BoolVar(&noData, "no-data", false, "keep /data and snapshots")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "list sandboxes", GroupID: groupSandbox,
		Args:              cobra.NoArgs,
		ValidArgsFunction: completeNothing,
		RunE: func(_ *cobra.Command, _ []string) error {
			names, err := sandbox.List()
			if err != nil || len(names) == 0 {
				return nil
			}
			fmt.Printf("%-14s %-12s %-5s %-7s %-7s %-6s %-8s %s\n",
				"NAME", "STATE", "CPUS", "RAM", "NET", "RO", "TIMEOUT", "BASE")
			for _, name := range names {
				path, _ := sandbox.BoxfilePath(name)
				s, err := boxfile.Parse(path)
				if err != nil {
					fmt.Printf("%-14s invalid Boxfile\n", name)
					continue
				}
				timeout := "-"
				if s.TimeoutSeconds > 0 {
					timeout = strconv.Itoa(s.TimeoutSeconds) + "s"
				}
				var states []string
				if sandbox.IsUp(name) {
					states = append(states, "up")
				}
				if s.Warm > 0 {
					states = append(states, fmt.Sprintf("warm %d/%d", runtime.Warm(name), s.Warm))
				}
				state := strings.Join(states, ",")
				if state == "" {
					state = "-"
				}
				base := s.Base
				if parent, err := sandbox.Parent(name); err == nil {
					base += "  (fork of " + parent + ")"
				}
				fmt.Printf("%-14s %-12s %-5d %-7s %-7s %-6t %-8s %s\n", name, state, s.CPUs,
					strconv.Itoa(s.RAMMiB)+"M", s.Network, s.ReadOnlyRootfs, timeout, base)
			}
			return nil
		},
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// editorArgv resolves the user's editor. Fields are split so "code --wait"
// works, not just a bare command name.
func editorArgv() []string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return strings.Fields(v)
		}
	}
	return []string{"vi"}
}

func openEditor(path string) error {
	argv := editorArgv()
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}

// chooseTarget asks which file to open. Reaching EOF without a line means
// nobody is there to answer -- /dev/null is a character device, so testing for
// a terminal would wrongly accept it -- and guessing between two files that
// behave this differently is worse than refusing.
func chooseTarget() (boxfile bool, err error) {
	fmt.Println("  1  Boxfile       the spec you edit")
	fmt.Println("  2  Containerfile  generated, replaced by the next build")
	fmt.Print("  choose [1]: ")
	line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
	if readErr != nil && strings.TrimSpace(line) == "" {
		fmt.Println()
		return false, fmt.Errorf("no answer; pass -b for the Boxfile or -c for the Containerfile")
	}
	switch strings.TrimSpace(line) {
	case "", "1", "b", "boxfile":
		return true, nil
	case "2", "c", "containerfile":
		return false, nil
	default:
		return false, fmt.Errorf("choose 1 or 2")
	}
}

func editCmd() *cobra.Command {
	var wantBlue, wantContainer bool
	c := &cobra.Command{
		Use: "edit <name>", Short: "edit the Boxfile or Containerfile", GroupID: groupSandbox,
		Long: "Opens a sandbox's file in $VISUAL, $EDITOR, or vi.\n\n" +
			"The Containerfile is generated from the Boxfile, so edits to it are\n" +
			"replaced by the next build. Change the Boxfile to make them stick.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.Exists(name) {
				return fmt.Errorf("no sandbox %q", name)
			}
			if wantBlue && wantContainer {
				return fmt.Errorf("pass -b or -c, not both")
			}

			editBoxfile := wantBlue
			if !wantBlue && !wantContainer {
				chosen, err := chooseTarget()
				if err != nil {
					return err
				}
				editBoxfile = chosen
			}

			if editBoxfile {
				path, err := sandbox.BoxfilePath(name)
				if err != nil {
					return err
				}
				if err := openEditor(path); err != nil {
					return err
				}
				// Catch a broken edit now rather than at the next build.
				if _, err := boxfile.Parse(path); err != nil {
					return fmt.Errorf("saved, but it no longer parses:\n%w", err)
				}
				fmt.Printf("ok — run: box build %s\n", name)
				return nil
			}

			path, err := sandbox.ContainerfilePath(name)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("no Containerfile yet; it is written by: box build %s", name)
			}
			fmt.Println("note: generated file — the next build overwrites it")
			return openEditor(path)
		},
	}
	c.Flags().BoolVarP(&wantBlue, "boxfile", "b", false, "edit the Boxfile")
	c.Flags().BoolVarP(&wantContainer, "containerfile", "c", false, "edit the generated Containerfile")
	return c
}
