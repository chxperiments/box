package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A fork is a sandbox whose /data is an overlay on another sandbox's: the
// parent's /data is the read-only lower layer and the fork's own writes go
// to an upper directory. Running a fork therefore never touches the parent,
// Diff reads the upper directly, and Apply merges it into the parent.
//
// The overlay is mounted inside rootless podman's user namespace (an
// unprivileged overlayfs, no root needed), where the subordinate UID a
// strict sandbox runs as can reach it. That namespace persists across podman
// commands but is not the host's: on the host the merge directory is an
// empty directory, and a fork's VMs are launched from within the namespace.
//
//	~/.box/forks/<name>/fork.json   parent and creation time
//	~/.box/forks/<name>/upper       the fork's changes
//	~/.box/forks/<name>/work        overlayfs scratch space
//	~/.box/forks/<name>/merge       where the overlay is mounted

// ErrNotFork is returned by fork-only operations on an ordinary sandbox.
var ErrNotFork = errors.New("not a fork")

type forkMeta struct {
	Parent  string `json:"parent"`
	Created string `json:"created"`
}

// ForkDir holds a fork's layers and metadata.
func ForkDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "forks", name), nil
}

// UpperDir is where a fork's changes to /data land.
func UpperDir(name string) (string, error) {
	d, err := ForkDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "upper"), nil
}

func workDir(name string) (string, error) {
	d, err := ForkDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "work"), nil
}

// MergeDir is the fork's /data as the VM sees it: the overlay's mountpoint.
func MergeDir(name string) (string, error) {
	d, err := ForkDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "merge"), nil
}

// ForkLayers returns the overlay's lower, upper, work and merge paths.
func ForkLayers(name string) (lower, upper, work, merge string, err error) {
	parent, err := Parent(name)
	if err != nil {
		return "", "", "", "", err
	}
	if lower, err = DataDir(parent); err != nil {
		return "", "", "", "", err
	}
	if upper, err = UpperDir(name); err != nil {
		return "", "", "", "", err
	}
	if work, err = workDir(name); err != nil {
		return "", "", "", "", err
	}
	if merge, err = MergeDir(name); err != nil {
		return "", "", "", "", err
	}
	return lower, upper, work, merge, nil
}

// Unmount takes a fork's overlay down, which its upper layer must be
// before it is changed or removed from outside. A fork whose VMs are still
// running holds the mount, and the error says so. Not mounted is not an
// error: the goal is that it is unmounted.
func Unmount(name string) error {
	merge, err := MergeDir(name)
	if err != nil {
		return err
	}
	out, err := exec.Command("podman", "unshare", "umount", merge).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	switch {
	case err == nil, strings.Contains(msg, "not mounted"), strings.Contains(msg, "no mount point"):
		return nil
	case strings.Contains(msg, "busy"):
		return fmt.Errorf("%s is in use by a running VM; stop it first (box down %s)", name, name)
	}
	return fmt.Errorf("unmounting %s: %s", name, msg)
}

// Parent returns the sandbox a fork was taken from, or ErrNotFork.
func Parent(name string) (string, error) {
	d, err := ForkDir(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(d, "fork.json"))
	if err != nil {
		return "", ErrNotFork
	}
	var m forkMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("unreadable fork metadata for %s: %w", name, err)
	}
	return m.Parent, nil
}

// IsFork reports whether name is a fork.
func IsFork(name string) bool {
	_, err := Parent(name)
	return err == nil
}

// Forks lists the forks taken from parent, sorted.
func Forks(parent string) ([]string, error) {
	h, err := Home()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(h, "forks"))
	if err != nil {
		return nil, nil // no forks yet
	}
	var out []string
	for _, e := range entries {
		if p, err := Parent(e.Name()); err == nil && p == parent {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// DataMount is the -v argument that gives a sandbox its /data: the data
// directory itself, or for a fork the overlay's mountpoint, which is only
// populated inside podman's namespace (see Overlay in the runtime package).
func DataMount(name string) (string, error) {
	src, err := DataSource(name)
	if err != nil {
		return "", err
	}
	return src + ":/data", nil
}

// DataSource is the host directory that becomes /data: the data directory,
// or for a fork its overlay's mountpoint.
func DataSource(name string) (string, error) {
	_, err := Parent(name)
	if errors.Is(err, ErrNotFork) {
		return DataDir(name)
	}
	if err != nil {
		return "", err
	}
	return MergeDir(name)
}

// CreateFork makes name a fork of parent: the parent's Boxfile is copied,
// and /data becomes an overlay on the parent's. The image is the caller's
// to retag. A fork of a fork is refused: its lower layer would be the
// parent's merged view, which exists only inside a running VM.
func CreateFork(parent, name string) error {
	if !Exists(parent) {
		return fmt.Errorf("no sandbox %q", parent)
	}
	if IsFork(parent) {
		return fmt.Errorf("%s is itself a fork; apply or discard it before forking from it", parent)
	}
	if Exists(name) {
		return fmt.Errorf("sandbox %q already exists", name)
	}
	src, err := BoxfilePath(parent)
	if err != nil {
		return err
	}
	boxfile, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dir, err := Dir(name)
	if err != nil {
		return err
	}
	fdir, err := ForkDir(name)
	if err != nil {
		return err
	}
	for _, d := range []string{dir, filepath.Join(fdir, "upper"), filepath.Join(fdir, "work"), filepath.Join(fdir, "merge")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	meta, _ := json.Marshal(forkMeta{Parent: parent, Created: time.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(fdir, "fork.json"), meta, 0o644); err != nil {
		return err
	}
	dst, err := BoxfilePath(name)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, boxfile, 0o644)
}

// Discard throws away a fork's changes, leaving it a clean fork of its
// parent again.
func Discard(name string) error {
	if !IsFork(name) {
		return ErrNotFork
	}
	// The overlay must not be live while its upper layer is emptied.
	if err := Unmount(name); err != nil {
		return err
	}
	for _, f := range []func(string) (string, error){UpperDir, workDir} {
		d, err := f(name)
		if err != nil {
			return err
		}
		if err := removeTree(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// RemoveFork deletes a fork's layers and metadata. Its sandbox definition is
// removed separately, with Remove.
func RemoveFork(name string) error {
	d, err := ForkDir(name)
	if err != nil {
		return err
	}
	if err := Unmount(name); err != nil {
		return err
	}
	return removeTree(d)
}
