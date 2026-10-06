package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// vmArgs builds every -v in one place, so the declarative mounts from the
// Boxfile must show up there, after /data, with their mode attached.
func TestVmArgsMounts(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())

	s := boxfile.Default
	s.Mounts = []boxfile.Mount{
		{Host: "/tmp/inputs", Guest: "/inputs", Mode: "ro"},
		{Host: "/tmp/out", Guest: "/out", Mode: "rw"},
		{Host: "/tmp/unset", Guest: "/unset"}, // parse normally defaults this to ro
	}
	args, err := vmArgs("devbox", s, false, true)
	if err != nil {
		t.Fatal(err)
	}
	var mounts []string
	for i, a := range args {
		if a == "-v" && strings.Contains(args[i+1], ":/") && !strings.HasSuffix(args[i+1], ":/data") {
			mounts = append(mounts, args[i+1])
		}
	}
	want := []string{"/tmp/inputs:/inputs:ro", "/tmp/out:/out:rw", "/tmp/unset:/unset:ro"}
	if len(mounts) != len(want) {
		t.Fatalf("mounts wrong: %v", mounts)
	}
	for i, w := range want {
		if mounts[i] != w {
			t.Errorf("mount[%d] = %q, want %q", i, mounts[i], w)
		}
	}
}

func TestVmArgsWithoutMounts(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	args, err := vmArgs("devbox", boxfile.Default, false, true)
	if err != nil {
		t.Fatal(err)
	}
	var data string
	for i, a := range args {
		if a == "-v" {
			data = args[i+1]
		}
	}
	if !strings.HasSuffix(data, ":/data") {
		t.Errorf("expected exactly the /data mount, got %q", data)
	}
}

// Every VM, krun or the plain-container baseline, starts confined: libkrun
// gives the guest whatever its VMM can do, so these flags are the boundary
// behind a libkrun escape and must never silently drop out of vmArgs.
func TestVmArgsConfineTheVMM(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	s := boxfile.Default
	s.RAMMiB = 1024
	for _, krun := range []bool{true, false} {
		args, err := vmArgs("devbox", s, false, krun)
		if err != nil {
			t.Fatal(err)
		}
		joined := " " + strings.Join(args, " ") + " "
		for _, want := range []string{
			" no-new-privileges ",
			" --cap-drop=all ",
			" --cap-add=CHOWN,DAC_OVERRIDE,FOWNER,SETUID,SETGID,NET_BIND_SERVICE ",
			" --pids-limit=512 ",
			" --memory=1280m ",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("krun=%v: missing %q in %v", krun, strings.TrimSpace(want), args)
			}
		}
		if strings.Contains(joined, "--privileged") {
			t.Errorf("krun=%v: --privileged must never be passed", krun)
		}
	}
}

// The sandbox image is only ever the locally built one: a missing image must
// fail rather than be resolved and pulled from a registry.
func TestVmArgsNeverPulls(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	args, err := vmArgs("devbox", boxfile.Default, false, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range args {
		if a == "--pull=never" {
			found = true
		}
	}
	if !found {
		t.Errorf("vmArgs must pass --pull=never, got %v", args)
	}
	if tag := sandbox.ImageTag("devbox"); !strings.HasPrefix(tag, "localhost/") {
		t.Errorf("ImageTag %q is a short name, so podman may resolve it against a registry", tag)
	}
}

// mountTree lays out a host directory tree under a temp dir and points
// BOX_HOME at another part of it.
func mountTree(t *testing.T) (root, home string) {
	t.Helper()
	root = t.TempDir()
	home = filepath.Join(root, "bbhome")
	t.Setenv("BOX_HOME", home)
	for _, d := range []string{"proj/config", "secret", "other", "bbhome/data/devbox"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, home
}

func mountErr(t *testing.T, mounts ...boxfile.Mount) error {
	t.Helper()
	s := boxfile.Default
	s.Mounts = mounts
	_, err := vmArgs("devbox", s, false, true)
	return err
}

// A mount nested inside an rw mount can be redirected by the guest: it
// replaces the inner directory with a symlink during one run, and the next
// run mounts the symlink's target. That layout is refused outright, whether
// or not a symlink has been planted yet.
func TestVmArgsRefusesMountInsideRwMount(t *testing.T) {
	root, _ := mountTree(t)
	proj, config := filepath.Join(root, "proj"), filepath.Join(root, "proj", "config")
	rw := boxfile.Mount{Host: proj, Guest: "/work", Mode: "rw"}
	for _, inner := range []boxfile.Mount{
		{Host: config, Guest: "/config", Mode: "ro"},
		{Host: config, Guest: "/config", Mode: "rw"},
		{Host: proj, Guest: "/again", Mode: "ro"}, // the same tree twice
	} {
		if err := mountErr(t, rw, inner); err == nil {
			t.Errorf("%s inside rw %s should be refused", inner.Host, proj)
		}
	}

	// After the guest has redirected it: config -> ../secret.
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret", config); err != nil {
		t.Fatal(err)
	}
	if err := mountErr(t, rw, boxfile.Mount{Host: config, Guest: "/config", Mode: "ro"}); err == nil {
		t.Error("a mount already redirected through a guest-planted symlink should be refused")
	}
}

// Every sandbox's /data is guest-writable, so no mount may live under it,
// and an rw mount may not contain this sandbox's /data.
func TestVmArgsRefusesMountsAroundData(t *testing.T) {
	root, home := mountTree(t)
	if err := mountErr(t, boxfile.Mount{Host: filepath.Join(home, "data", "devbox"), Guest: "/x", Mode: "ro"}); err == nil {
		t.Error("a mount under box's data directory should be refused")
	}
	if err := mountErr(t, boxfile.Mount{Host: root, Guest: "/x", Mode: "rw"}); err == nil {
		t.Error("an rw mount containing /data should be refused")
	}
	if err := mountErr(t, boxfile.Mount{Host: root, Guest: "/x", Mode: "ro"}); err != nil {
		t.Errorf("a ro mount containing /data cannot redirect anything: %v", err)
	}
}

// A second spelling of an rw tree through a host symlink is still the same
// tree.
func TestVmArgsRefusesNestingThroughHostSymlink(t *testing.T) {
	root, _ := mountTree(t)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "proj"), alias); err != nil {
		t.Fatal(err)
	}
	err := mountErr(t,
		boxfile.Mount{Host: alias, Guest: "/work", Mode: "rw"},
		boxfile.Mount{Host: filepath.Join(root, "proj", "config"), Guest: "/config", Mode: "ro"},
	)
	if err == nil {
		t.Error("nesting reached through a host symlink should be refused")
	}
}

// Side-by-side mounts, ro-inside-ro, and mounts that merely share a name
// prefix are all fine.
func TestVmArgsAllowsIndependentMounts(t *testing.T) {
	root, _ := mountTree(t)
	if err := os.MkdirAll(filepath.Join(root, "proj-extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := mountErr(t,
		boxfile.Mount{Host: filepath.Join(root, "proj"), Guest: "/work", Mode: "rw"},
		boxfile.Mount{Host: filepath.Join(root, "proj-extra"), Guest: "/extra", Mode: "rw"},
		boxfile.Mount{Host: filepath.Join(root, "other"), Guest: "/other", Mode: "ro"},
		boxfile.Mount{Host: filepath.Join(root, "other", "sub"), Guest: "/sub", Mode: "ro"},
	)
	if err != nil {
		t.Errorf("independent mounts should be accepted: %v", err)
	}
}
