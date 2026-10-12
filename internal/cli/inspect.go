package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"box/internal/runtime"
)

func inspectCmd() *cobra.Command {
	var format string
	c := &cobra.Command{
		Use: "inspect <name>", Short: "what a sandbox can and cannot touch", GroupID: groupInspect,
		Long: "A security report: the VMM's identity and confinement, the network, what\n" +
			"the sandbox can read and write on the host, and what box last proved.\n\n" +
			"It is read back out of the configuration box hands the backend, not\n" +
			"from the Boxfile, so it cannot describe a confinement box does not\n" +
			"apply. When the sandbox is up, the running VMM is read too, and any\n" +
			"difference is a failure: the command then exits 1.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeName,
		Example: "  box inspect agent --security-report\n" +
			"  box inspect agent --security-report=json | jq .findings",
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			s, err := loadSpec(name)
			if err != nil {
				return err
			}
			r, err := runtime.SecurityReport(name, s)
			if err != nil {
				return err
			}
			switch format {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(r); err != nil {
					return err
				}
			case "text", "":
				printReport(r)
			default:
				return fmt.Errorf("--security-report is text or json, not %q", format)
			}
			if r.Failed() {
				exitCode = 1
			}
			return nil
		},
	}
	c.Flags().StringVar(&format, "security-report", "text", "report format: text or json")
	c.Flags().Lookup("security-report").NoOptDefVal = "text"
	return c
}

func printReport(r runtime.Report) {
	fmt.Printf("%s  (backend %s, isolation %s)\n", r.Sandbox, r.Backend, r.Isolation)
	fmt.Printf("read from %s\n\n", r.ReadFrom)

	v := r.VMM
	who := fmt.Sprintf("host UID %d", v.HostUID)
	switch {
	case v.RunsAsYou:
		who += " (you)"
	case v.Slot != nil:
		who += fmt.Sprintf(" (strict slot %d, owns nothing of yours or of another sandbox)", *v.Slot)
	}
	caps := "none"
	if len(v.Capabilities) > 0 {
		caps = strings.Join(v.Capabilities, " ")
	}
	fmt.Println("VMM")
	row("runs as", who)
	row("capabilities", caps)
	row("no_new_privs", yesno(v.NoNewPrivileges))
	row("seccomp", v.Seccomp)
	row("namespaces", strings.Join(v.Namespaces, " "))
	row("limits", fmt.Sprintf("%d processes, %d MiB", v.PidsLimit, v.MemoryLimitMiB))

	fmt.Println("\nnetwork")
	row("mode", r.Network.Mode)
	row("own namespace", yesno(r.Network.OwnNamespace))
	row("host loopback", map[bool]string{true: "reachable", false: "not reachable"}[r.Network.HostLoopbackReachable])
	row("egress", r.Network.Egress)

	fmt.Println("\nfilesystem")
	row("root", map[bool]string{true: "read-only", false: "writable (reset every run)"}[r.Filesystem.RootReadOnly])
	d := r.Filesystem.Data
	data := d.Host + " (" + d.Kind
	if d.Owner != "" {
		data += ", owner " + d.Owner + ", mode " + d.Mode
	}
	row("/data", data+"), kept between runs")
	if len(r.Filesystem.Mounts) == 0 {
		row("host mounts", "none")
	}
	for _, m := range r.Filesystem.Mounts {
		row("mount", fmt.Sprintf("%s -> %s (%s)", m.Host, m.Guest, m.Mode))
	}

	fmt.Println("\nhost")
	row("KVM", yesno(r.Host.KVM))
	row("kernel", r.Host.Kernel)
	if r.Host.GuestKernel != "" {
		state := "current"
		if !r.Host.IsolationCurrent {
			state = "stale: the runtime changed since"
		}
		row("isolation", fmt.Sprintf("guest kernel %s, verified %s (%s)", r.Host.GuestKernel, r.Host.IsolationVerifiedAt, state))
	}

	if r.Live != nil {
		fmt.Println("\nrunning VMM")
		l := r.Live
		row("pid", fmt.Sprint(l.PID))
		row("runs as", fmt.Sprintf("host UID %d", l.HostUID))
		lcaps := "none"
		if len(l.Capabilities) > 0 {
			lcaps = strings.Join(l.Capabilities, " ")
		}
		row("capabilities", lcaps)
		row("no_new_privs", yesno(l.NoNewPrivileges))
		row("seccomp", l.Seccomp)
		if l.Matches {
			row("matches", "yes, everything above")
		} else {
			row("matches", "NO")
		}
	}

	fmt.Println("\nfindings")
	for _, f := range r.Findings {
		fmt.Printf("  %-4s  %s\n", strings.ToUpper(f.Level), f.Text)
	}
}

func row(k, v string) { fmt.Printf("  %-14s %s\n", k, v) }

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
