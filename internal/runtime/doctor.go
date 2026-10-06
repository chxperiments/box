package runtime

import (
	"bufio"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"box/internal/sandbox"
)

// Check is one finding of Doctor. A failed check carries the command or
// change that fixes it; a warning is something that limits box (no
// strict mode, no up/exec) without stopping it.
type Check struct {
	Name   string
	OK     bool
	Warn   bool // true: degraded, not broken
	Detail string
	Fix    string
}

// Doctor runs every environment check and reports all of them, where
// Preflight stops at the first. It is what a user runs when something does
// not work, so each failure says what to do about it.
func Doctor() []Check {
	var cs []Check
	add := func(c Check) { cs = append(cs, c) }

	if p, err := exec.LookPath("podman"); err != nil {
		add(Check{Name: "podman", Detail: "not on PATH",
			Fix: "sudo dnf install podman   (or your distro's package)"})
	} else {
		ver, _ := exec.Command("podman", "--version").Output()
		add(Check{Name: "podman", OK: true, Detail: strings.TrimSpace(string(ver)) + " at " + p})
	}

	if goruntime.GOOS == "darwin" {
		out, err := exec.Command("podman", "machine", "list", "--format", "{{.Running}}").Output()
		running := err == nil && strings.Contains(string(out), "true")
		add(Check{Name: "podman machine", OK: running,
			Detail: map[bool]string{true: "running", false: "not running"}[running],
			Fix:    "podman machine init && podman machine start"})
		add(Check{Name: "up / exec / warm", Warn: true,
			Detail: "Linux hosts only for now; run and shell work"})
		return cs
	}

	if fi, err := os.Stat("/dev/kvm"); err != nil {
		add(Check{Name: "/dev/kvm", Detail: "missing: no KVM on this host",
			Fix: "enable virtualization in firmware; in a VM, enable nested virtualization"})
	} else if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		add(Check{Name: "/dev/kvm", Detail: "present but not writable by you (" + fi.Mode().String() + ")",
			Fix: "sudo usermod -aG kvm $USER   then log in again"})
	} else {
		f.Close()
		add(Check{Name: "/dev/kvm", OK: true, Detail: "present and accessible"})
	}

	krun, err := exec.LookPath("krun")
	if err != nil {
		add(Check{Name: "krun", Detail: "not on PATH",
			Fix: "sudo ln -sf $(command -v crun) /usr/local/bin/krun"})
	} else {
		real, _ := filepath.EvalSymlinks(krun)
		ver, _ := exec.Command("krun", "--version").Output()
		if strings.Contains(string(ver), "+LIBKRUN") {
			add(Check{Name: "krun", OK: true, Detail: krun + " -> " + real + " (+LIBKRUN)"})
		} else {
			add(Check{Name: "krun", Detail: krun + " resolves to a crun built without libkrun support",
				Fix: "sudo dnf install crun libkrun   (Fedora); elsewhere build crun with --with-libkrun"})
		}
	}

	if ids := subordinateIDs(); ids == "" {
		add(Check{Name: "subordinate UIDs", Warn: true,
			Detail: "none for " + currentUser() + " in /etc/subuid: isolation: strict cannot run",
			Fix:    "sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 " + currentUser()})
	} else {
		add(Check{Name: "subordinate UIDs", OK: true, Detail: ids})
	}

	if exe, err := os.Executable(); err == nil {
		if dyn, err := dynamicallyLinked(exe); err == nil && dyn {
			add(Check{Name: "static binary", Warn: true,
				Detail: "this box is dynamically linked, so it cannot be the guest agent: up, exec and warm will refuse",
				Fix:    "CGO_ENABLED=0 go build -o box ./cmd/box   (release builds are static)"})
		} else {
			add(Check{Name: "static binary", OK: true, Detail: "up, exec and warm can use it as the guest agent"})
		}
	}

	home, err := sandbox.Home()
	switch {
	case err != nil:
		add(Check{Name: "box home", Detail: err.Error()})
	default:
		if err := os.MkdirAll(home, 0o755); err != nil {
			add(Check{Name: "box home", Detail: home + ": " + err.Error(),
				Fix: "point BOX_HOME at a writable directory"})
		} else {
			add(Check{Name: "box home", OK: true, Detail: home})
		}
	}
	if _, err := sandbox.SocketPath(); err != nil {
		add(Check{Name: "SDK socket", Warn: true, Detail: err.Error()})
	}

	if t, ok := loadToken(); ok {
		id, _ := RuntimeIdentity()
		if id == t.Identity {
			add(Check{Name: "isolation", OK: true,
				Detail: fmt.Sprintf("verified %s: guest %s, outside %s", t.VerifiedAt, t.Guest, t.Baseline)})
		} else {
			add(Check{Name: "isolation", Warn: true,
				Detail: "the runtime changed since it was last verified; the next run re-checks it"})
		}
	} else {
		add(Check{Name: "isolation", Warn: true, Detail: "not verified yet",
			Fix: "box build <name>   proves a sandbox gets its own kernel"})
	}
	return cs
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return fmt.Sprint(os.Getuid())
}

// subordinateIDs returns this user's /etc/subuid range, or "" if none.
func subordinateIDs() string {
	f, err := os.Open("/etc/subuid")
	if err != nil {
		return ""
	}
	defer f.Close()
	user := currentUser()
	uid := fmt.Sprint(os.Getuid())
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) == 3 && (parts[0] == user || parts[0] == uid) {
			return parts[1] + " + " + parts[2]
		}
	}
	return ""
}

func dynamicallyLinked(exe string) (bool, error) {
	f, err := elf.Open(exe)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true, nil
		}
	}
	return false, nil
}
