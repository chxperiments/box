package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"box/internal/sandbox"
)

// autoSandbox writes a Boxfile and points the firecracker host check at
// hostOK, so the tests do not depend on the machine running them.
func autoSandbox(t *testing.T, name, boxfile string, hostOK bool) {
	t.Helper()
	old := fcHostCheck
	fcHostCheck = func() error {
		if hostOK {
			return nil
		}
		return errors.New("no KVM here")
	}
	t.Cleanup(func() { fcHostCheck = old })
	dir, err := sandbox.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Boxfile"), []byte(boxfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := sandbox.DataDir(name); err == nil {
		os.MkdirAll(data, 0o755)
	}
}

const offline = "base: alpine\nnetwork: none\nbackend: auto\n"
const online = "base: alpine\nnetwork: bridge\nbackend: auto\n"

func resolvedBackend(t *testing.T, name string) string {
	t.Helper()
	s, err := parseSpec(name)
	if err != nil {
		t.Fatal(err)
	}
	return s.Backend
}

func TestAutoPicksFirecrackerWhenItFits(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	autoSandbox(t, "off", offline, true)
	if got := resolvedBackend(t, "off"); got != "firecracker" {
		t.Fatalf("offline sandbox on a capable host: %s", got)
	}
	if c, ok := ReadAutoChoice("off"); !ok || c.Backend != "firecracker" || c.Reason == "" {
		t.Errorf("choice not recorded: %+v %v", c, ok)
	}
}

func TestAutoPicksPodmanOtherwise(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	autoSandbox(t, "on", online, true)
	if got := resolvedBackend(t, "on"); got != "podman" {
		t.Errorf("networked sandbox: %s", got)
	}
	c, _ := ReadAutoChoice("on")
	if !strings.Contains(c.Reason, "network: none") {
		t.Errorf("reason does not say why: %q", c.Reason)
	}
	autoSandbox(t, "nokvm", offline, false)
	if got := resolvedBackend(t, "nokvm"); got != "podman" {
		t.Errorf("host without firecracker: %s", got)
	}
}

func TestAutoStaysWhereTheDataIs(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	// Picked podman while online; going offline would now fit firecracker,
	// but the sandbox stays on podman.
	autoSandbox(t, "a", online, true)
	resolvedBackend(t, "a")
	autoSandbox(t, "a", offline, true)
	if got := resolvedBackend(t, "a"); got != "podman" {
		t.Errorf("moved off podman: %s", got)
	}
}

func TestAutoSwitchesOnlyWithoutData(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	autoSandbox(t, "b", offline, true)
	if resolvedBackend(t, "b") != "firecracker" {
		t.Fatal("setup")
	}
	// It goes online, which firecracker cannot do, and has no data: switch.
	autoSandbox(t, "b", online, true)
	if got := resolvedBackend(t, "b"); got != "podman" {
		t.Errorf("empty sandbox did not switch: %s", got)
	}

	autoSandbox(t, "c", offline, true)
	resolvedBackend(t, "c")
	disk, _ := fcDataDiskPath("c")
	os.MkdirAll(filepath.Dir(disk), 0o700)
	os.WriteFile(disk, []byte("data"), 0o600)
	autoSandbox(t, "c", online, true)
	if _, err := parseSpec("c"); err == nil || !strings.Contains(err.Error(), "box data export c") {
		t.Errorf("a sandbox with data on firecracker switched or said nothing useful: %v", err)
	}
	if c, _ := ReadAutoChoice("c"); c.Backend != "firecracker" {
		t.Errorf("the record moved anyway: %s", c.Backend)
	}
}

func TestNoBackendKeyStaysPodman(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	autoSandbox(t, "old", "base: alpine\nnetwork: none\n", true)
	if got := resolvedBackend(t, "old"); got != "podman" {
		t.Errorf("an existing Boxfile without backend: moved to %s", got)
	}
	if _, ok := ReadAutoChoice("old"); ok {
		t.Error("recorded an auto choice for a sandbox that did not ask for one")
	}
}
