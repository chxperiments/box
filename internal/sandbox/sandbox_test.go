package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidName(t *testing.T) {
	for _, name := range []string{"devbox", "a", "x9", "a-b_c.d"} {
		if err := ValidName(name); err != nil {
			t.Errorf("ValidName(%q) should accept: %v", name, err)
		}
	}
	for _, name := range []string{
		"", ".", "..", "...", "-x", ".hidden",
		"a/b", "../esc", "../../escape", "x/../y",
		"a b", "a\tb", "a\nb",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 65 chars
	} {
		if err := ValidName(name); err == nil {
			t.Errorf("ValidName(%q) should reject", name)
		}
	}
}

// Every filesystem sink derives its path through these builders, so a
// rejected name makes deletion, moving and creation outside the box
// root unreachable.
func TestPathBuildersRejectUnsafeNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep the assertions independent of the real home
	for _, name := range []string{"../esc", "a/b", "..", ""} {
		if p, err := Dir(name); err == nil {
			t.Errorf("Dir(%q) = %q, want error", name, p)
		}
		if p, err := DataDir(name); err == nil {
			t.Errorf("DataDir(%q) = %q, want error", name, p)
		}
		if p, err := LogPath(name); err == nil {
			t.Errorf("LogPath(%q) = %q, want error", name, p)
		}
		if p, err := SnapshotsDir(name); err == nil {
			t.Errorf("SnapshotsDir(%q) = %q, want error", name, p)
		}
	}
}

func TestPathBuildersAcceptSafeNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, err := Dir("devbox")
	if err != nil || d == "" {
		t.Errorf("Dir(devbox): %q %v", d, err)
	}
}

// writeArchive builds a .tar.gz containing exactly the named entries. Written
// with archive/tar rather than the tar command so a hostile entry can be
// crafted portably, whichever tar the host ships.
func writeArchive(t *testing.T, path string, members ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		body := []byte("payload")
		if err := tw.WriteHeader(&tar.Header{
			Name: m, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMember(t *testing.T) {
	for _, m := range []string{"./", ".", "file.txt", "./sub/nested.txt", "a.b-c_d/e"} {
		if err := checkMember(m); err != nil {
			t.Errorf("checkMember(%q) should accept: %v", m, err)
		}
	}
	for _, m := range []string{
		"/etc/passwd", "/", "../escaped", "../../escaped",
		"./../escaped", "sub/../../escaped", "a/b/../../../c",
	} {
		if err := checkMember(m); err == nil {
			t.Errorf("checkMember(%q) should reject", m)
		}
	}
}

// setup makes a sandbox with data in it and returns its data directory.
func setup(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("BOX_HOME", t.TempDir())
	if _, err := Create(name); err != nil {
		t.Fatal(err)
	}
	data, err := DataDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{
		"keep.txt":     "original",
		"sub/deep.txt": "nested",
	} {
		if err := os.WriteFile(filepath.Join(data, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

// A restore returns /data to exactly the snapshot's contents: files changed
// after the snapshot revert, and files added after it are gone.
func TestRestoreRoundTrip(t *testing.T) {
	data := setup(t, "demo")
	archive, err := Snapshot("demo", "20260101T000000Z")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "keep.txt"), []byte("changed"), 0o644)
	os.WriteFile(filepath.Join(data, "added.txt"), []byte("new"), 0o644)

	if err := Restore("demo", archive); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keep.txt")); string(b) != "original" {
		t.Errorf("keep.txt = %q, want %q", b, "original")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "sub", "deep.txt")); string(b) != "nested" {
		t.Errorf("nested file lost: %q", b)
	}
	if _, err := os.Stat(filepath.Join(data, "added.txt")); !os.IsNotExist(err) {
		t.Error("a file added after the snapshot should not survive a restore")
	}
	// The staging directories must not be left behind.
	for _, leftover := range []string{data + ".restoring", data + ".replaced"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should not survive a restore", filepath.Base(leftover))
		}
	}
}

// An archive whose entries point outside the extraction directory is refused
// before anything is unpacked, and /data is left exactly as it was.
func TestRestoreRefusesTraversalArchive(t *testing.T) {
	data := setup(t, "demo")
	evil := filepath.Join(t.TempDir(), "evil.tar.gz")
	writeArchive(t, evil, "../../escaped.txt")

	if err := Restore("demo", evil); err == nil {
		t.Fatal("a traversing archive must be refused")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keep.txt")); string(b) != "original" {
		t.Errorf("/data was disturbed by a refused restore: %q", b)
	}
	outside := filepath.Join(filepath.Dir(filepath.Dir(data)), "escaped.txt")
	if _, err := os.Stat(outside); err == nil {
		t.Error("the archive escaped the data directory")
	}
	if _, err := os.Stat(data + ".restoring"); !os.IsNotExist(err) {
		t.Error("a refused restore should not leave a staging directory")
	}
}

func TestRestoreRefusesAbsoluteMember(t *testing.T) {
	setup(t, "demo")
	evil := filepath.Join(t.TempDir(), "abs.tar.gz")
	writeArchive(t, evil, "/etc/passwd")
	if err := Restore("demo", evil); err == nil {
		t.Error("an absolute archive entry must be refused")
	}
}

// A corrupt archive must fail before /data is touched.
func TestRestoreKeepsDataWhenArchiveIsUnreadable(t *testing.T) {
	data := setup(t, "demo")
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	os.WriteFile(bad, []byte("not a gzip stream"), 0o644)

	if err := Restore("demo", bad); err == nil {
		t.Fatal("an unreadable archive must be refused")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keep.txt")); string(b) != "original" {
		t.Errorf("/data was disturbed by a failed restore: %q", b)
	}
}

func TestSnapshotPathResolution(t *testing.T) {
	setup(t, "demo")
	first, err := Snapshot("demo", "20260101T000000Z")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := Snapshot("demo", "20260202T000000Z")
	if err != nil {
		t.Fatal(err)
	}
	// No reference means the most recent snapshot.
	if got, err := SnapshotPath("demo", ""); err != nil || got != latest {
		t.Errorf("SnapshotPath(\"\") = %q %v, want %q", got, err, latest)
	}
	// A bare stamp resolves, with or without the suffix.
	for _, ref := range []string{"20260101T000000Z", "20260101T000000Z.tar.gz"} {
		if got, err := SnapshotPath("demo", ref); err != nil || got != first {
			t.Errorf("SnapshotPath(%q) = %q %v, want %q", ref, got, err, first)
		}
	}
	if _, err := SnapshotPath("demo", "nosuchstamp"); err == nil {
		t.Error("an unknown snapshot name must be an error")
	}
}

func TestSnapshotPathWithoutAnySnapshots(t *testing.T) {
	setup(t, "demo")
	if _, err := SnapshotPath("demo", ""); err == nil {
		t.Error("expected an error when the sandbox has no snapshots")
	}
}

// A label is a filename, so the name grammar applies to it: nothing a label
// contains may reach outside the sandbox's snapshot directory.
func TestSnapshotRejectsUnsafeLabel(t *testing.T) {
	setup(t, "demo")
	for _, label := range []string{"", "..", "../escape", "a/b", "with space", ".hidden"} {
		if p, err := Snapshot("demo", label); err == nil {
			t.Errorf("Snapshot(%q) = %q, want error", label, p)
		}
	}
}

// A labelled snapshot is restored by its label, the point of having one.
func TestSnapshotLabelRoundTrip(t *testing.T) {
	data := setup(t, "demo")
	if _, err := Snapshot("demo", "before-upgrade"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "keep.txt"), []byte("changed"), 0o644)

	got, err := SnapshotPath("demo", "before-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "before-upgrade.tar.gz" {
		t.Errorf("SnapshotPath resolved to %q", got)
	}
	if err := Restore("demo", got); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keep.txt")); string(b) != "original" {
		t.Errorf("keep.txt = %q, want %q", b, "original")
	}
}

// Reusing a label replaces that snapshot rather than accumulating archives.
func TestSnapshotLabelIsReusable(t *testing.T) {
	data := setup(t, "demo")
	if _, err := Snapshot("demo", "nightly"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "keep.txt"), []byte("second"), 0o644)
	if _, err := Snapshot("demo", "nightly"); err != nil {
		t.Fatal(err)
	}
	snaps, err := Snapshots("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %v, want one", snaps)
	}
	// The staging file must not be left behind, nor be listed as a snapshot.
	if _, err := os.Stat(snaps[0] + ".partial"); !os.IsNotExist(err) {
		t.Error("a completed snapshot should leave no .partial file")
	}
	if err := Restore("demo", snaps[0]); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keep.txt")); string(b) != "second" {
		t.Errorf("keep.txt = %q, want the re-taken snapshot's contents", b)
	}
}

// Labels sort nowhere near timestamps, so "newest" has to come from the
// archives' times rather than their names.
func TestSnapshotsOrderedByTimeNotName(t *testing.T) {
	setup(t, "demo")
	// "aaa" sorts before any timestamp, but is taken after it.
	older, err := Snapshot("demo", "20260101T000000Z")
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Snapshot("demo", "aaa")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	snaps, err := Snapshots("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[len(snaps)-1] != newer {
		t.Errorf("Snapshots = %v, want %q last", snaps, newer)
	}
	// So an unqualified restore rolls back to the one actually taken last.
	if got, err := SnapshotPath("demo", ""); err != nil || got != newer {
		t.Errorf("SnapshotPath(\"\") = %q %v, want %q", got, err, newer)
	}
}

func TestSocketPathFallsBackForLongHomes(t *testing.T) {
	short := t.TempDir()
	t.Setenv("BOX_HOME", short)
	if p, err := SocketPath(); err != nil || p != filepath.Join(short, "box.sock") {
		t.Fatalf("short home: %q, %v", p, err)
	}

	long := "/" + strings.Repeat("x", 120)
	t.Setenv("BOX_HOME", long)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	p, err := SocketPath()
	if err != nil || !strings.HasPrefix(p, "/run/user/1000/box-") || len(p) > maxSocketPath {
		t.Fatalf("long home: %q, %v", p, err)
	}

	// Never a shared directory: without a private runtime dir, refuse.
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := SocketPath(); err == nil {
		t.Fatal("long home without XDG_RUNTIME_DIR must be refused")
	}
}

// writeBoxfile makes setup's sandbox a defined one, which Rename requires.
func writeBoxfile(t *testing.T, name string) {
	t.Helper()
	p, err := BoxfilePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Snapshots follow a renamed sandbox, as the rename help promises. Left under
// the old name they would escape `destroy --data` and be restored into any
// later sandbox that reuses that name.
func TestRenameMovesSnapshots(t *testing.T) {
	setup(t, "work")
	writeBoxfile(t, "work")
	if _, err := Snapshot("work", "before"); err != nil {
		t.Fatal(err)
	}
	if err := Rename("work", "proj"); err != nil {
		t.Fatal(err)
	}
	if got, err := SnapshotPath("proj", "before"); err != nil {
		t.Errorf("snapshot did not move with the sandbox: %v", err)
	} else if filepath.Base(got) != "before.tar.gz" {
		t.Errorf("SnapshotPath resolved to %q", got)
	}
	old, err := SnapshotsDir("work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("snapshots left behind under the old name at %s", old)
	}

	// A new sandbox that reuses the old name starts with nothing to restore.
	if _, err := Create("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotPath("work", ""); err == nil {
		t.Error("a reused name should not inherit the renamed sandbox's snapshots")
	}
}

// Data or snapshots left under the target name by an earlier sandbox (destroy
// keeps /data by default) are neither adopted nor allowed to break a rename
// halfway: the rename is refused before anything moves.
func TestRenameRefusesLeftoverDestination(t *testing.T) {
	data := setup(t, "work")
	writeBoxfile(t, "work")
	leftover, err := SnapshotsDir("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Rename("work", "proj"); err == nil {
		t.Fatal("rename onto leftover snapshots should be refused")
	}
	if !Exists("work") {
		t.Error("a refused rename must leave the source definition in place")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keep.txt")); string(b) != "original" {
		t.Error("a refused rename must leave the source data in place")
	}
}

// Staging must never use a path that is, or could become, another sandbox's
// /data: a sandbox may legitimately be called demo.restoring.
func TestRestoreLeavesLookalikeSandboxesAlone(t *testing.T) {
	setup(t, "demo")
	archive, err := Snapshot("demo", "s1")
	if err != nil {
		t.Fatal(err)
	}
	var others []string
	for _, n := range []string{"demo.restoring", "demo.replaced"} {
		if _, err := Create(n); err != nil {
			t.Fatal(err)
		}
		d, _ := DataDir(n)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "mine"), []byte(n), 0o644)
		others = append(others, d)
	}
	if err := Restore("demo", archive); err != nil {
		t.Fatal(err)
	}
	for _, d := range others {
		if b, _ := os.ReadFile(filepath.Join(d, "mine")); string(b) != filepath.Base(d) {
			t.Errorf("restoring demo disturbed %s", filepath.Base(d))
		}
	}
}

// A restore killed partway leaves its staging behind; the next one clears it,
// and a successful one leaves nothing but the data directories.
func TestRestoreCleansStaging(t *testing.T) {
	data := setup(t, "demo")
	archive, err := Snapshot("demo", "s1")
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(filepath.Dir(data), ".demo.restore-123")
	os.MkdirAll(filepath.Join(stale, "new"), 0o755)
	if err := Restore("demo", archive); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(data))
	for _, e := range entries {
		if e.Name() != "demo" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// Archives written by the system tar (as Snapshot does) restore with their
// symlinks and hard links intact.
func TestRestoreReadsSystemTar(t *testing.T) {
	data := setup(t, "demo")
	os.Symlink("/usr/bin/env", filepath.Join(data, "abs"))
	os.Symlink("keep.txt", filepath.Join(data, "rel"))
	os.Link(filepath.Join(data, "keep.txt"), filepath.Join(data, "sub", "hard"))
	archive, err := Snapshot("demo", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ResetData("demo"); err != nil {
		t.Fatal(err)
	}
	if err := Restore("demo", archive); err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(data, "abs")); l != "/usr/bin/env" {
		t.Errorf("abs -> %q", l)
	}
	if l, _ := os.Readlink(filepath.Join(data, "rel")); l != "keep.txt" {
		t.Errorf("rel -> %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "sub", "hard")); string(b) != "original" {
		t.Errorf("hard link = %q", b)
	}
}
