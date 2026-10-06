package cli

import (
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"box/internal/sandbox"
)

// matching filters candidates by what has been typed so far. Cobra filters its
// own static ValidArgs but hands a completion function's results to the shell
// as they are, so the prefix has to be applied here.
func matching(candidates []string, prefix string) []string {
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// completeName offers the defined sandboxes. Names are the one box
// argument a shell cannot guess, and they are read from disk at completion
// time, so a sandbox created in another terminal completes immediately
// without re-sourcing anything.
func completeName(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := sandbox.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return matching(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeNothing suppresses completion where the argument is invented rather
// than chosen -- a new sandbox, a rename target. The default is file names,
// which are never valid there, so an empty list is more honest than a wrong one.
func completeNothing(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeRunArgs completes the sandbox, then stops. What follows is a command
// line for the guest, and the host's files are not the guest's: completing
// them would suggest paths that do not exist inside the microVM.
func completeRunArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completeName(cmd, args, toComplete)
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeSnapshot completes restore: the sandbox, then its snapshots by
// stamp -- the short form restore accepts, not the full path. Once what is
// typed looks like a path, restore takes an archive from anywhere, so the
// shell's own file completion is the useful answer.
func completeSnapshot(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completeName(cmd, args, toComplete)
	}
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if strings.ContainsRune(toComplete, filepath.Separator) {
		return nil, cobra.ShellCompDirectiveDefault
	}
	snaps, err := sandbox.Snapshots(args[0])
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Newest first: a restore is usually a roll back to the last snapshot.
	stamps := make([]string, 0, len(snaps))
	for i := len(snaps) - 1; i >= 0; i-- {
		stamps = append(stamps, strings.TrimSuffix(filepath.Base(snaps[i]), ".tar.gz"))
	}
	return matching(stamps, toComplete), cobra.ShellCompDirectiveNoFileComp
}
