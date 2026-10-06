package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// helpGroups is the order groups appear, with the short label shown once per
// group instead of a heading line per section.
var helpGroups = []struct{ id, label string }{
	{groupSandbox, "define"},
	{groupRun, "run"},
	{groupState, "state"},
	{groupInspect, "inspect"},
	{groupRemove, "remove"},
}

// rootHelp prints a compact overview: one usage line, then one line per
// command. Detail lives behind `box <command> -h`.
func rootHelp(c *cobra.Command) {
	var b strings.Builder
	b.WriteString("\n   usage: box <command> [flags]\n\n")

	for _, g := range helpGroups {
		label := g.label
		for _, sub := range c.Commands() {
			if sub.GroupID != g.id || sub.Hidden || !sub.IsAvailableCommand() {
				continue
			}
			fmt.Fprintf(&b, "   %-8s %-9s %s\n", label, sub.Name(), sub.Short)
			label = "" // the group is named once, then indented under it
		}
	}
	b.WriteString("\n   box <command> -h   flags and detail\n")
	fmt.Print(b.String())
}

// installHelp gives the root a compact overview while leaving subcommands with
// cobra's standard layout, which is already the right shape for one command.
func installHelp(root *cobra.Command) {
	root.SetHelpFunc(func(c *cobra.Command, _ []string) {
		if !c.HasParent() {
			rootHelp(c)
			return
		}
		if c.Long != "" {
			fmt.Println(c.Long)
		} else if c.Short != "" {
			fmt.Println(c.Short)
		}
		fmt.Print("\n", c.UsageString())
	})
}
