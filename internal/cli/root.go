// Package cli defines the box command line. main() only calls Execute.
package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"box/internal/boxfile"
	"box/internal/runtime"
)

// exitCode lets a command choose the process status without cobra printing an
// error: a sandbox command that exits 3 is not a box failure.
var exitCode = 0

// Version is stamped at build time:
//
//	go build -ldflags "-X box/internal/cli.Version=v1.2.3" ./cmd/box
var Version = "dev"

// Command groups, in the order they appear in help.
const (
	groupSandbox = "sandbox"
	groupRun     = "run"
	groupState   = "state"
	groupInspect = "inspect"
	groupRemove  = "remove"
	groupHelp    = "help"
)

func newRoot() *cobra.Command {
	// Keep commands in the order they are added: new before build before ls
	// reads as a workflow, where alphabetical does not.
	cobra.EnableCommandSorting = false

	root := &cobra.Command{
		Use:           "box",
		Short:         "Isolated, persistent sandboxes",
		Version:       Version,
		SilenceUsage:  true, // an error is not a reason to reprint the manual
		SilenceErrors: true, // printed by Execute, without a stack of usage text
	}
	root.AddGroup(
		&cobra.Group{ID: groupSandbox, Title: "Defining sandboxes:"},
		&cobra.Group{ID: groupRun, Title: "Running things:"},
		&cobra.Group{ID: groupState, Title: "Managing state:"},
		&cobra.Group{ID: groupInspect, Title: "Inspecting:"},
		&cobra.Group{ID: groupRemove, Title: "Removing:"},
		&cobra.Group{ID: groupHelp, Title: "Shell:"},
	)
	root.SetHelpCommandGroupID(groupHelp)
	root.SetCompletionCommandGroupID(groupHelp)
	installHelp(root)

	root.AddCommand(
		newCmd(), editCmd(), buildCmd(), lsCmd(),
		runCmd(), shellCmd(), upCmd(), execCmd(), downCmd(), serveCmd(), mcpCmd(),
		resetCmd(), snapshotCmd(), restoreCmd(),
		forkCmd(), diffCmd(), applyCmd(), discardCmd(),
		dataCmd(),
		envCmd(), logsCmd(), verifyCmd(), doctorCmd(),
		renameCmd(), destroyCmd(), nukeCmd(),
		agentCmd(), prepareCmd(), tendCmd(), overlayCmd(), krunCmd(), exportCmd(), netnsCmd(), fcCmd(), tarOutCmd(), tarInCmd(),
	)
	return root
}

// Execute runs the CLI and returns the process exit status.
func Execute() int {
	oldHomeNotice()
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		if exitCode != 0 {
			return exitCode
		}
		return 1
	}
	return exitCode
}

// oldHomeNotice points users of the project's former name, bluebox, at the
// new root. Nothing is moved automatically: the data is theirs, and images
// built under the old name need rebuilding anyway.
func oldHomeNotice() {
	if os.Getenv("BLUEBOX_HOME") != "" && os.Getenv("BOX_HOME") == "" {
		notice("BLUEBOX_HOME is set but no longer read; bluebox is now box, so set BOX_HOME instead")
		return
	}
	if os.Getenv("BOX_HOME") != "" {
		return
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(h, ".bluebox")); err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(h, ".box")); err == nil {
		return
	}
	notice("found ~/.bluebox from bluebox, which is now box. To keep your sandboxes:\n" +
		"  mv ~/.bluebox ~/.box\n" +
		"then rename each sandbox's Bluefile to Boxfile and run box build <name>.")
}

// loadSpec reads a sandbox's Boxfile.
func loadSpec(name string) (boxfile.Spec, error) { return runtime.LoadSpec(name) }

// notice reports a slow path on stderr, so it never mixes into a command's
// stdout.
func notice(msg string) { fmt.Fprintln(os.Stderr, "box: "+msg) }

// confirm asks before destroying something. Without a terminal it refuses
// rather than assuming yes, so a script cannot delete data by accident.
func confirm(prompt string, assumeYes bool) error {
	if assumeYes {
		return nil
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("refusing without a terminal; pass --yes to confirm")
	}
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		return fmt.Errorf("cancelled")
	}
	return nil
}
