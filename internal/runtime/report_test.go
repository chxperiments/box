package runtime

import (
	"os"
	"slices"
	"strings"
	"testing"

	"box/internal/boxfile"
)

var sixCaps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "NET_BIND_SERVICE", "SETGID", "SETUID"}

func TestCapsFromMaskReadsTheEscapeSuitesMask(t *testing.T) {
	// 0x4cb is what the escape suite expects a podman or krun VMM to hold.
	if got := capsFromMask(0x4cb); !slices.Equal(got, sixCaps) {
		t.Errorf("0x4cb: %v", got)
	}
	if got := capsFromMask(0); len(got) != 0 {
		t.Errorf("0: %v", got)
	}
	if got := capNames(strings.Split(vmmCaps, ",")); !slices.Equal(got, sixCaps) {
		t.Errorf("vmmCaps: %v", got)
	}
	if got := capNames([]string{"CAP_SETUID", "chown", "CAP_CHOWN"}); !slices.Equal(got, []string{"CHOWN", "SETUID"}) {
		t.Errorf("normalised: %v", got)
	}
}

func TestCompareLive(t *testing.T) {
	want := VMMReport{HostUID: 524288, Capabilities: sixCaps, NoNewPrivileges: true, Seccomp: "podman's default filter"}
	yes, no := true, false
	ok := func() *Live {
		return &Live{HostUID: 524288, Capabilities: sixCaps, NoNewPrivileges: true, Seccomp: "filter", OwnNetNamespace: &yes}
	}
	l := ok()
	compareLive(l, want, true)
	if !l.Matches || len(l.Differences) != 0 {
		t.Fatalf("a matching VMM: %v", l.Differences)
	}
	for _, c := range []struct {
		what   string
		change func(*Live)
	}{
		{"another UID", func(l *Live) { l.HostUID = 1000 }},
		{"an extra capability", func(l *Live) { l.Capabilities = append(slices.Clone(sixCaps), "SYS_ADMIN") }},
		{"no_new_privs off", func(l *Live) { l.NoNewPrivileges = false }},
		{"no seccomp", func(l *Live) { l.Seccomp = "none" }},
		{"the host's network namespace", func(l *Live) { l.OwnNetNamespace = &no }},
		{"an unreadable network namespace", func(l *Live) { l.OwnNetNamespace = nil }},
	} {
		l := ok()
		c.change(l)
		compareLive(l, want, true)
		if l.Matches || len(l.Differences) != 1 {
			t.Errorf("%s: matches=%v differences=%v", c.what, l.Matches, l.Differences)
		}
	}
}

func TestFindingsFailOnRealProblems(t *testing.T) {
	base := Report{Isolation: "strict", VMM: VMMReport{HostUID: 524288, NoNewPrivileges: true, Seccomp: "x"},
		Network: NetReport{Egress: "none"}, Host: HostReport{KVM: true, IsolationCurrent: true}}
	sound := base
	sound.Findings = findings(sound, boxfile.Spec{})
	if sound.Failed() {
		t.Fatalf("a sound strict sandbox failed: %v", sound.Findings)
	}
	for _, c := range []struct {
		what   string
		change func(*Report)
	}{
		{"strict but running as you", func(r *Report) { r.VMM.RunsAsYou = true }},
		{"no_new_privs off", func(r *Report) { r.VMM.NoNewPrivileges = false }},
		{"no KVM", func(r *Report) { r.Host.KVM = false }},
		{"live differs", func(r *Report) { r.Live = &Live{Differences: []string{"x"}} }},
	} {
		r := base
		c.change(&r)
		r.Findings = findings(r, boxfile.Spec{})
		if !r.Failed() {
			t.Errorf("%s did not fail: %v", c.what, r.Findings)
		}
	}
}

func TestKrunSpecReportsWhatItConfigures(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	newSandbox(t, "k")
	s := boxfile.Default
	s.Network = "none"
	r := Report{}
	if err := reportKrun(&r, "k", s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.VMM.Capabilities, sixCaps) || !r.VMM.NoNewPrivileges || r.VMM.Seccomp == "none" ||
		r.VMM.PidsLimit != vmmPidsLimit || r.VMM.HostUID != os.Getuid() || !r.Network.OwnNamespace {
		t.Errorf("krun report: %+v net %+v", r.VMM, r.Network)
	}
}

func TestFirecrackerReportsNoCapabilitiesAndTheSlot(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	t.Setenv("USER", "boxtest")
	newSandbox(t, "f")
	r := Report{}
	if err := reportFirecracker(&r, "f", boxfile.Default, 0); err != nil {
		t.Fatal(err)
	}
	if len(r.VMM.Capabilities) != 0 || !r.VMM.NoNewPrivileges || r.VMM.HostUID != os.Getuid() {
		t.Errorf("standard firecracker: %+v", r.VMM)
	}
}
