package runtime

import (
	"fmt"
	"path/filepath"
	"strings"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// checkMountSources refuses a mount whose host path can be redirected by a
// guest. podman resolves every -v source afresh at boot, following symlinks,
// so if any component of a mount's host path lies in a tree the guest can
// write, a guest can swap that component for a symlink during one run and
// the next run mounts whatever it points at -- ~/.ssh, say -- under the
// declared guest path and mode. What the spec declares is then no longer
// what the sandbox sees.
//
// The guest can write every sandbox's /data (~/.box/data/*) and this
// sandbox's rw mounts, so no mount source, including this sandbox's own
// /data, may sit inside one of those. The check is about where a path sits,
// not where it currently points, so it holds whatever symlinks a guest has
// already planted. It runs on every boot, in vmArgs.
func checkMountSources(name string, s boxfile.Spec) error {
	home, err := sandbox.Home()
	if err != nil {
		return err
	}
	data, err := sandbox.DataDir(name)
	if err != nil {
		return err
	}
	type tree struct {
		path, what string
		mount      int // index into s.Mounts, or -1
	}
	writable := []tree{{filepath.Join(home, "data"), "box's /data directories", -1}}
	for i, m := range s.Mounts {
		if m.Mode == "rw" {
			writable = append(writable, tree{m.Host, "the rw mount of " + m.Host, i})
		}
	}
	for i, m := range s.Mounts {
		for _, w := range writable {
			if w.mount != i && reachesInto(m.Host, w.path) {
				return fmt.Errorf("mount %s -> %s: the host path is inside %s, which the guest can write, "+
					"so a guest could redirect this mount with a symlink; mount a directory outside it instead",
					m.Host, m.Guest, w.what)
			}
		}
	}
	for _, w := range writable[1:] {
		if reachesInto(data, w.path) {
			return fmt.Errorf("mount %s is rw and contains this sandbox's /data (%s), "+
				"so a guest could redirect /data with a symlink; mount a directory outside it instead", w.path, data)
		}
	}
	return nil
}

// reachesInto reports whether host path p passes through tree w: whether p,
// or any directory on the way down to it, is w or lies inside it. Each prefix
// is compared both as written and with symlinks resolved, so a spelling that
// reaches w through a host symlink (/home vs /var/home) is caught too.
func reachesInto(p, w string) bool {
	w = filepath.Clean(w)
	wr := resolve(w)
	p = filepath.Clean(p)
	prefix := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(p, prefix), string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		if within(prefix, w) || within(resolve(prefix), wr) {
			return true
		}
	}
	return false
}

// resolve follows symlinks in p. A path that does not exist (yet) is taken
// as written; podman will refuse to mount it anyway.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within reports whether p is dir or inside it. Both must be clean.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
