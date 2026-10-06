package runtime

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"

	"box/internal/sandbox"
)

// MountOverlay mounts a fork's overlay if it is not mounted yet. It must run
// inside podman's user namespace (podman unshare), whose root may mount an
// unprivileged overlayfs; the podman backend arranges that. The namespace is kept by
// podman's pause process, so the mount outlives this process and later
// launches find it already there.
func MountOverlay(name string) error {
	lower, upper, work, merge, err := sandbox.ForkLayers(name)
	if err != nil {
		return err
	}
	for _, p := range []string{lower, upper, work, merge} {
		// The option string is comma-separated; a comma in a path would
		// split it. Names are validated, so only BOX_HOME can bring one.
		if strings.ContainsAny(p, ",:") {
			return fmt.Errorf("fork paths cannot contain ',' or ':' (BOX_HOME is %q)", p)
		}
	}
	if err := os.MkdirAll(lower, 0o755); err != nil {
		return err
	}
	if err := mountOverlay(lower, upper, work, merge); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// mountOverlay mounts lower+upper at merge, unless it already is. It needs
// the caller to be root in a user namespace (podman unshare).
func mountOverlay(lower, upper, work, merge string) error {
	if mounted(merge) {
		return nil
	}
	// userxattr: an unprivileged overlay keeps its marks (opaque, whiteout
	// redirects) in user.overlay.*; Diff reads the same names.
	opts := "userxattr,lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work
	if err := syscall.Mount("overlay", merge, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mounting overlay at %s: %w (is this inside podman unshare?)", merge, err)
	}
	return nil
}

// mounted reports whether path is a mountpoint in this mount namespace.
func mounted(path string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && fields[4] == path {
			return true
		}
	}
	return false
}
