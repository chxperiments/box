package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"box/internal/runtime"
	"box/internal/sandbox"
)

func forkCmd() *cobra.Command {
	return &cobra.Command{
		Use: "fork <name> <fork>", Short: "branch a sandbox's /data", GroupID: groupState,
		Long: "Creates <fork>: the same Boxfile and image as <name>, with /data an\n" +
			"overlay on <name>'s. Running the fork never touches <name>; its\n" +
			"changes collect in a layer of their own, which diff shows, apply\n" +
			"merges back and discard throws away. Fork several times to try\n" +
			"several approaches side by side, then keep one.\n\n" +
			"Only /data is branched. Everything else resets per run anyway.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeName,
		Example: "  box fork devbox try-b\n" +
			"  box run try-b -- make test\n" +
			"  box diff try-b\n" +
			"  box apply try-b      # or: box discard try-b",
		RunE: func(_ *cobra.Command, args []string) error {
			parent, name := args[0], args[1]
			if err := sandbox.ValidName(name); err != nil {
				return err
			}
			if err := sandbox.CreateFork(parent, name); err != nil {
				return err
			}
			// Same image, so no rebuild. The parent may never have built.
			runtime.CopyImage(parent, name)
			fmt.Printf("%s forked to %s\n", parent, name)
			return nil
		},
	}
}

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use: "diff <fork>", Short: "what a fork changed", GroupID: groupState,
		Long: "Lists a fork's changes to /data relative to its parent, one per line:\n" +
			"A added, M modified, D deleted. Read from the fork's own layer, so\n" +
			"it is exact and boots nothing.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if handled, err := asDataOwner(name, "diff", name); handled {
				return err
			}
			cs, err := sandbox.Diff(name)
			if errors.Is(err, sandbox.ErrNotFork) {
				return fmt.Errorf("%s is not a fork; make one with: box fork <name> <fork>", name)
			}
			if err != nil {
				return err
			}
			fmt.Print(sandbox.DescribeChanges(cs))
			return nil
		},
	}
}

func applyCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "apply <fork>", Short: "merge a fork into its parent", GroupID: groupState,
		Long: "Merges a fork's changes into its parent's /data, then clears the fork\n" +
			"so it carries on from the merged state. Shows the changes and asks\n" +
			"first; -y skips the prompt.\n\n" +
			"The parent's /data is guest-written and may hold symlinks to anywhere;\n" +
			"apply never follows one, so a sandbox cannot turn an apply into a\n" +
			"write elsewhere on the host. Device nodes, fifos and sockets are\n" +
			"skipped and reported; setuid and setgid bits are dropped.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			parent, err := sandbox.Parent(name)
			if errors.Is(err, sandbox.ErrNotFork) {
				return fmt.Errorf("%s is not a fork", name)
			}
			if err != nil {
				return err
			}
			// Both sides hold /data mounted while up, and a waiting VM holds
			// the parent's as its lower layer.
			for _, n := range []string{parent, name} {
				if err := refuseWhileUp(n, "apply"); err != nil {
					return err
				}
				if err := runtime.Drain(n); err != nil {
					return err
				}
			}
			if handled, err := asDataOwner(name, "apply", "-y", name); handled {
				if err == nil {
					runtime.SpawnTender(parent)
					runtime.SpawnTender(name)
				}
				return err
			}
			cs, err := sandbox.Diff(name)
			if err != nil {
				return err
			}
			if len(cs) == 0 {
				fmt.Printf("%s has no changes\n", name)
				return nil
			}
			if !yes {
				fmt.Print(sandbox.DescribeChanges(cs))
				if err := confirm(fmt.Sprintf("Apply %s to %s's /data?",
					plural(len(cs), "change", "changes"), parent), yes); err != nil {
					return err
				}
			}
			// Apply writes the parent's /data, which the fork's live overlay
			// has as its lower layer, and then clears the upper layer.
			if err := sandbox.Unmount(name); err != nil {
				return err
			}
			skipped, err := sandbox.Apply(name)
			for _, s := range skipped {
				fmt.Fprintf(os.Stderr, "box: skipped %s: not a file, directory or symlink\n", s)
			}
			if err != nil {
				return err
			}
			fmt.Printf("applied %s to %s\n", plural(len(cs), "change", "changes"), parent)
			runtime.SpawnTender(parent)
			runtime.SpawnTender(name)
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func discardCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "discard <fork>", Short: "drop a fork's changes", GroupID: groupState,
		Long:              "Throws away a fork's changes to /data, leaving it a clean fork again.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !sandbox.IsFork(name) {
				return fmt.Errorf("%s is not a fork", name)
			}
			if err := refuseWhileUp(name, "discard it"); err != nil {
				return err
			}
			if handled, err := asDataOwner(name, "discard", "-y", name); handled {
				return err
			}
			cs, err := sandbox.Diff(name)
			if err != nil {
				return err
			}
			if len(cs) == 0 {
				fmt.Printf("%s has no changes\n", name)
				return nil
			}
			if err := confirm(fmt.Sprintf("Discard %s in %s?", plural(len(cs), "change", "changes"), name), yes); err != nil {
				return err
			}
			if err := runtime.Drain(name); err != nil {
				return err
			}
			if err := sandbox.Discard(name); err != nil {
				return err
			}
			fmt.Printf("discarded %s\n", plural(len(cs), "change", "changes"))
			runtime.SpawnTender(name)
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// asDataOwner re-runs this command under podman unshare when the fork's
// files belong to a strict sandbox's subordinate UID rather than to you:
// there, the namespace's root can read and write them. The re-run is
// marked, so it cannot recurse, and its exit status is passed through.
func asDataOwner(name string, args ...string) (handled bool, err error) {
	if os.Getenv("BOX_UNSHARED") != "" {
		return false, nil
	}
	upper, err := sandbox.UpperDir(name)
	if err != nil {
		return false, nil // let the command report the bad name
	}
	if mine, err := sandbox.OwnedByMe(upper); err != nil || mine {
		return false, nil
	}
	self, err := os.Executable()
	if err != nil {
		return true, err
	}
	cmd := exec.Command("podman", append([]string{"unshare", self}, args...)...)
	cmd.Env = append(os.Environ(), "BOX_UNSHARED=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if code := runtime.ExitCode(err); code > 0 {
			exitCode = code // the child already printed its message
			return true, nil
		}
		return true, err
	}
	return true, nil
}
