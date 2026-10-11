// Package sandbox owns the on-disk layout of box sandboxes.
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// nameRe keeps a sandbox name a single safe path component, so every path
// derived from a name stays inside the box root. The alphanumeric first
// character rejects ".", ".." and hidden names in one stroke.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports whether name is usable as a sandbox name.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid sandbox name %q: use letters, digits, '.', '_' or '-' (max 64, starting with a letter or digit)", name)
	}
	return nil
}

// ValidLabel reports whether label is usable to name a snapshot. Labels share
// the sandbox name grammar: an archive is <label>.tar.gz, so a label has to be
// a single safe path component for the same reason a name does.
func ValidLabel(label string) error {
	if !nameRe.MatchString(label) {
		return fmt.Errorf("invalid snapshot name %q: use letters, digits, '.', '_' or '-' (max 64, starting with a letter or digit)", label)
	}
	return nil
}

// Home is the box root, overridable with BOX_HOME.
func Home() (string, error) {
	if h := os.Getenv("BOX_HOME"); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(h, ".box"), nil
}

// Dir holds a sandbox's definition (its Boxfile and generated Containerfile).
func Dir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "sandboxes", name), nil
}

// DataDir is the only path that survives between runs; mounted at /data.
func DataDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "data", name), nil
}

func BoxfilePath(name string) (string, error) {
	d, err := Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "Boxfile"), nil
}

// ContainerfilePath is a build artifact generated from the Boxfile.
func ContainerfilePath(name string) (string, error) {
	d, err := Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "Containerfile"), nil
}

// LogPath is the append-only record of runs for a sandbox.
func LogPath(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "logs", name+".log"), nil
}

// ImageTag formats the podman image tag. The name is not re-validated here:
// every call site reaches this through a path builder that already ran
// ValidName, and podman itself rejects malformed refs loudly.
//
// The tag is fully qualified with localhost/, which is where podman stores a
// locally built image anyway. A short name like box/<name> would go
// through registries.conf short-name resolution whenever the local image is
// missing (never built, pruned, failed build), and could pull and run
// someone else's box/<name> image with /data and the rw mounts attached.
func ImageTag(name string) string { return "localhost/box/" + name + ":latest" }

// VerifyCachePath holds the last successful isolation check, keyed on the
// runtime's identity. It is not per-sandbox: isolation is a property of the
// podman + krun toolchain, so one cleared runtime clears it for every sandbox.
func VerifyCachePath() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "verify.json"), nil
}

// RunStatePath records a sandbox brought up with `box up`: where its agent
// listens and the token that admits a client. It exists only while the VM is
// meant to be running.
func RunStatePath(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "run", name+".json"), nil
}

// IsUp reports whether a sandbox was brought up and not yet brought down. It
// reads only the state file, so it is cheap enough to guard every command
// that must not pull /data out from under a running VM.
func IsUp(name string) bool {
	p, err := RunStatePath(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// maxSocketPath is the longest Unix socket path every platform accepts
// (sun_path is 108 bytes on Linux, 104 on macOS, and includes the NUL).
const maxSocketPath = 103

// SocketPath is where `box serve` listens for SDK clients: in the box
// root, or -- when that path is too long for a socket -- in the per-user
// runtime directory, named for the root so each BOX_HOME gets its own.
// It never falls back to a shared directory like /tmp, where another user
// could create the path first and receive every command an SDK sends.
// The Python SDK computes the same path; keep the two in step.
func SocketPath() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	p := filepath.Join(h, "box.sock")
	if len(p) <= maxSocketPath {
		return p, nil
	}
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		return "", fmt.Errorf("socket path %s is too long for a Unix socket; "+
			"shorten BOX_HOME or set XDG_RUNTIME_DIR", p)
	}
	sum := sha256.Sum256([]byte(h))
	return filepath.Join(run, "box-"+hex.EncodeToString(sum[:6])+".sock"), nil
}

// RootfsDir holds the krun backend's exported copies of a sandbox's image.
func RootfsDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "rootfs", name), nil
}

// VMsDir holds the krun backend's per-VM state: one directory per VM.
func VMsDir() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "vms"), nil
}

// PoolDir holds the warm pool: one file per pre-booted VM waiting for a run.
func PoolDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "pool", name), nil
}

// AgentDir holds the copy of the box binary that running sandboxes mount
// read-only and start as their agent.
func AgentDir() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "agent"), nil
}

// SnapshotsDir holds archived copies of a sandbox's /data.
func SnapshotsDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "snapshots", name), nil
}

// OwnedByMe reports whether path belongs to this user. A strict sandbox's
// /data belongs to a subordinate UID instead, and host-side operations on it
// go through podman unshare, where both IDs are reachable.
func OwnedByMe(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true, nil
	}
	return int(st.Uid) == os.Getuid(), nil
}

// asOwner prefixes argv with podman unshare when path is not ours, so a
// strict sandbox's files can be archived and removed from the host.
func asOwner(path string, argv ...string) *exec.Cmd {
	if mine, err := OwnedByMe(path); err == nil && !mine {
		argv = append([]string{"podman", "unshare"}, argv...)
	}
	return exec.Command(argv[0], argv[1:]...)
}

// removeTree deletes a directory tree, falling back to podman unshare for
// files a strict sandbox's guest created under its own UIDs.
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	if out, uerr := exec.Command("podman", "unshare", "rm", "-rf", "--", path).CombinedOutput(); uerr != nil {
		return fmt.Errorf("%v (and via podman unshare: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DataEmpty reports whether a sandbox has nothing worth losing.
func DataEmpty(name string) (bool, error) {
	d, err := DataDir(name)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(d)
	if os.IsNotExist(err) {
		return true, nil
	}
	if os.IsPermission(err) {
		out, err := asOwner(d, "ls", "-A", d).Output()
		if err != nil {
			return false, err
		}
		return len(strings.TrimSpace(string(out))) == 0, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// ResetData empties a sandbox's /data, leaving the sandbox itself intact.
func ResetData(name string) error {
	d, err := DataDir(name)
	if err != nil {
		return err
	}
	if err := removeTree(d); err != nil {
		return err
	}
	return os.MkdirAll(d, 0o755)
}

// Snapshot archives a sandbox's /data as label and returns the archive path.
// The label is either a timestamp or a name the user chose; either way it
// becomes a filename, so it is validated here rather than trusted. tar is used
// rather than a Go implementation so permissions and symlinks are preserved
// exactly as the guest wrote them.
func Snapshot(name, label string) (string, error) {
	if err := ValidLabel(label); err != nil {
		return "", err
	}
	data, err := DataDir(name)
	if err != nil {
		return "", err
	}
	dir, err := SnapshotsDir(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dir, label+".tar.gz")
	// Written beside the target and renamed into place, so an interrupted
	// snapshot leaves no half-written archive -- and, when a label is being
	// reused, does not destroy the archive it was going to replace.
	tmp := out + ".partial"
	cmd := asOwner(data, "tar", "-czf", tmp, "-C", data, ".")
	if msg, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("tar: %s", strings.TrimSpace(string(msg)))
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, nil
}

// Snapshots lists a sandbox's archives, newest last. They are ordered by
// modification time rather than by name: a labelled snapshot sorts nowhere
// near a timestamped one, so a name says nothing about which came last.
func Snapshots(name string) ([]string, error) {
	dir, err := SnapshotsDir(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // no snapshots yet is not an error
	}
	type archive struct {
		path string
		mod  time.Time
	}
	var found []archive
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished between the read and the stat
		}
		found = append(found, archive{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].mod.Equal(found[j].mod) {
			return found[i].path < found[j].path // stable for same-second writes
		}
		return found[i].mod.Before(found[j].mod)
	})
	out := make([]string, 0, len(found))
	for _, a := range found {
		out = append(out, a.path)
	}
	return out, nil
}

// SnapshotPath resolves a snapshot reference to an archive path. A bare
// reference names a snapshot of this sandbox (with or without the .tar.gz
// suffix); an empty one means the most recent. A reference containing a
// separator is taken as a path to an archive kept elsewhere.
func SnapshotPath(name, ref string) (string, error) {
	if ref == "" {
		snaps, err := Snapshots(name)
		if err != nil {
			return "", err
		}
		if len(snaps) == 0 {
			return "", fmt.Errorf("no snapshots for %q; take one with: box snapshot %s", name, name)
		}
		return snaps[len(snaps)-1], nil // Snapshots sorts newest last
	}
	if !strings.ContainsRune(ref, filepath.Separator) {
		dir, err := SnapshotsDir(name)
		if err != nil {
			return "", err
		}
		for _, cand := range []string{filepath.Join(dir, ref+".tar.gz"), filepath.Join(dir, ref)} {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
		return "", fmt.Errorf("no snapshot %q for %s; list them with: box snapshot %s -l", ref, name, name)
	}
	if fi, err := os.Stat(ref); err != nil || fi.IsDir() {
		return "", fmt.Errorf("no archive at %s", ref)
	}
	return ref, nil
}

// checkMember rejects an archive entry that would write outside the directory
// it is extracted into.
func checkMember(m string) error {
	// Checked before the trailing slash is trimmed, or a bare "/" would look
	// like the archive root rather than an absolute path.
	if strings.HasPrefix(m, "/") || filepath.IsAbs(m) {
		return fmt.Errorf("entry %q is an absolute path", m)
	}
	p := strings.TrimSuffix(m, "/")
	if p == "" || p == "." {
		return nil // the archive root
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("entry %q escapes the archive root", m)
		}
	}
	if c := filepath.Clean(p); c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return fmt.Errorf("entry %q escapes the archive root", m)
	}
	return nil
}

// restoreStaging is the prefix of the directory a restore of name works in,
// beside the data directories. The leading dot is something ValidName never
// produces, so it cannot be, or be removed as, another sandbox's /data.
func restoreStaging(name string) (parent, prefix string, err error) {
	data, err := DataDir(name)
	if err != nil {
		return "", "", err
	}
	return filepath.Dir(data), "." + name + ".restore-", nil
}

// Restore replaces a sandbox's /data with the contents of an archive. The
// archive is unpacked beside the existing data by ExtractArchive and swapped
// in only once it is complete, so a refused or failed restore leaves /data as
// it was.
func Restore(name, archive string) error {
	data, err := DataDir(name)
	if err != nil {
		return err
	}
	parent, prefix, err := restoreStaging(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	// Left by a restore that was killed partway; nothing else uses the prefix.
	if old, _ := filepath.Glob(filepath.Join(parent, prefix+"*")); len(old) > 0 {
		for _, o := range old {
			removeTree(o)
		}
	}
	work, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return err
	}
	defer removeTree(work)
	staged, replaced := filepath.Join(work, "new"), filepath.Join(work, "old")
	if err := os.Mkdir(staged, 0o755); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	err = ExtractArchive(f, staged)
	f.Close()
	if err != nil {
		return fmt.Errorf("refusing %s: %w", filepath.Base(archive), err)
	}
	if _, err := os.Lstat(data); err == nil {
		if err := os.Rename(data, replaced); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, data); err != nil {
		os.Rename(replaced, data) // put the original back
		return err
	}
	return nil
}

// Remove deletes a sandbox definition. Data is kept unless withData is set,
// because it is the only thing a rebuild cannot reproduce.
func Remove(name string, withData bool) error {
	dir, err := Dir(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if log, err := LogPath(name); err == nil {
		os.Remove(log)
	}
	if withData {
		data, err := DataDir(name)
		if err != nil {
			return err
		}
		if err := removeTree(data); err != nil {
			return err
		}
		// Snapshots are copies of that same data, so they go with it.
		if snaps, err := SnapshotsDir(name); err == nil {
			return os.RemoveAll(snaps)
		}
	}
	return nil
}

// Rename moves a sandbox's definition, data, log and snapshots to a new name.
// Snapshots move too: left behind, they would escape `destroy --data` on the
// new name and be restored, without a prompt, into any later sandbox that
// reuses the old one.
func Rename(from, to string) error {
	if !Exists(from) {
		return fmt.Errorf("no sandbox %q", from)
	}
	if Exists(to) {
		return fmt.Errorf("sandbox %q already exists", to)
	}
	paths := []func(string) (string, error){Dir, DataDir, LogPath, SnapshotsDir}
	// Exists only looks for a Boxfile, so data or snapshots left by an
	// earlier sandbox of the target name (destroy keeps /data by default)
	// would otherwise be silently adopted, or make a rename fail halfway.
	// Refuse before anything moves.
	for _, path := range paths {
		dst, err := path(to)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("%s already exists (left by an earlier %q?); remove it before renaming", dst, to)
		}
	}
	for _, path := range paths {
		src, err := path(from)
		if err != nil {
			return err
		}
		dst, err := path(to)
		if err != nil {
			return err
		}
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue // data, log or snapshots may not exist yet
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// Exists reports whether a sandbox has been created.
func Exists(name string) bool {
	p, err := BoxfilePath(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Create makes the directories for a new sandbox.
func Create(name string) (dir string, err error) {
	dir, err = Dir(name)
	if err != nil {
		return "", err
	}
	if Exists(name) {
		return "", fmt.Errorf("sandbox %q already exists at %s", name, dir)
	}
	data, err := DataDir(name)
	if err != nil {
		return "", err
	}
	for _, d := range []string{dir, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// List returns the names of all defined sandboxes.
func List() ([]string, error) {
	h, err := Home()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(h, "sandboxes"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && Exists(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
