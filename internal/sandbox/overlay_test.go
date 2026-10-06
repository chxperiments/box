package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeFork lays out a parent with data and a fork of it, returning the
// parent's data dir and the fork's upper dir for the test to populate.
func makeFork(t *testing.T) (data, upper string) {
	t.Helper()
	t.Setenv("BOX_HOME", t.TempDir())
	if _, err := Create("parent"); err != nil {
		t.Fatal(err)
	}
	p, _ := BoxfilePath("parent")
	os.WriteFile(p, []byte("base: docker.io/library/alpine:latest\n"), 0o644)
	if err := CreateFork("parent", "fork"); err != nil {
		t.Fatal(err)
	}
	data, _ = DataDir("parent")
	upper, _ = UpperDir("fork")
	return data, upper
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForkLayout(t *testing.T) {
	data, _ := makeFork(t)
	if p, err := Parent("fork"); err != nil || p != "parent" {
		t.Fatalf("Parent = %q, %v", p, err)
	}
	if _, err := Parent("parent"); err != ErrNotFork {
		t.Fatalf("parent reported as a fork: %v", err)
	}
	forks, _ := Forks("parent")
	if len(forks) != 1 || forks[0] != "fork" {
		t.Fatalf("Forks = %v", forks)
	}
	merge, _ := MergeDir("fork")
	if m, err := DataMount("fork"); err != nil || m != merge+":/data" {
		t.Fatalf("DataMount(fork) = %q, %v", m, err)
	}
	if lower, _, _, _, err := ForkLayers("fork"); err != nil || lower != data {
		t.Fatalf("ForkLayers lower = %q, %v", lower, err)
	}
	if m, _ := DataMount("parent"); m != data+":/data" {
		t.Fatalf("DataMount(parent) = %q", m)
	}
	if err := CreateFork("fork", "grandchild"); err == nil {
		t.Fatal("a fork of a fork was allowed")
	}
	if !Exists("fork") {
		t.Fatal("fork has no Boxfile")
	}
}

func TestDiffAndApply(t *testing.T) {
	data, upper := makeFork(t)
	write(t, filepath.Join(data, "keep"), "k")
	write(t, filepath.Join(data, "change"), "old")
	write(t, filepath.Join(data, "sub", "a"), "a")
	write(t, filepath.Join(upper, "change"), "new")
	write(t, filepath.Join(upper, "added"), "+")
	write(t, filepath.Join(upper, "sub", "b"), "b")
	os.Symlink("/etc/passwd", filepath.Join(upper, "link"))
	os.Chmod(filepath.Join(upper, "added"), 0o4755) // setuid must not survive apply

	got := DescribeChanges(must(Diff("fork")))
	want := "A added\nM change\nA link\nA sub/b\n"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}

	skipped, err := Apply("fork")
	if err != nil || len(skipped) != 0 {
		t.Fatalf("apply: %v, skipped %v", err, skipped)
	}
	for path, content := range map[string]string{"keep": "k", "change": "new", "added": "+", "sub/a": "a", "sub/b": "b"} {
		if b, _ := os.ReadFile(filepath.Join(data, path)); string(b) != content {
			t.Errorf("%s = %q, want %q", path, b, content)
		}
	}
	if l, _ := os.Readlink(filepath.Join(data, "link")); l != "/etc/passwd" {
		t.Errorf("symlink not carried over as a symlink: %q", l)
	}
	if fi, _ := os.Stat(filepath.Join(data, "added")); fi.Mode()&os.ModeSetuid != 0 {
		t.Error("setuid bit survived apply")
	}
	if cs := must(Diff("fork")); len(cs) != 0 {
		t.Errorf("fork not clean after apply: %v", cs)
	}
}

// The parent's /data is guest-written. A symlink there, pointing out of
// /data, must never turn an apply into a write somewhere else on the host.
func TestApplyNeverFollowsSymlinksOutOfData(t *testing.T) {
	data, upper := makeFork(t)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(data, "x"))
	os.Symlink(filepath.Join(outside, "victim"), filepath.Join(data, "f"))
	write(t, filepath.Join(upper, "x", "pwned"), "x")
	write(t, filepath.Join(upper, "f"), "overwritten")

	if _, err := Apply("fork"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("apply wrote outside /data: %v", entries)
	}
	if fi, err := os.Lstat(filepath.Join(data, "x")); err != nil || !fi.IsDir() {
		t.Fatalf("x should now be a real directory in /data: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "x", "pwned")); string(b) != "x" {
		t.Error("x/pwned not written inside /data")
	}
	if fi, _ := os.Lstat(filepath.Join(data, "f")); fi.Mode()&os.ModeSymlink != 0 {
		t.Error("f is still a symlink; the upper's regular file should have replaced it")
	}
}

// Whiteouts are 0:0 character devices, which only root (or a user
// namespace) can create: make them through podman unshare when available.
func TestDiffAndApplyWithWhiteouts(t *testing.T) {
	data, upper := makeFork(t)
	write(t, filepath.Join(data, "gone"), "g")
	write(t, filepath.Join(data, "dir", "old"), "o")
	if out, err := exec.Command("podman", "unshare", "sh", "-c",
		"mknod "+filepath.Join(upper, "gone")+" c 0 0 && mkdir "+filepath.Join(upper, "dir")+
			" && setfattr -n user.overlay.opaque -v y "+filepath.Join(upper, "dir")).CombinedOutput(); err != nil {
		t.Skipf("cannot create whiteouts here: %s", strings.TrimSpace(string(out)))
	}
	write(t, filepath.Join(upper, "dir", "new"), "n")

	got := DescribeChanges(must(Diff("fork")))
	want := "M dir\nA dir/new\nD dir/old\nD gone\n"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
	if _, err := Apply("fork"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(data, "gone")); !os.IsNotExist(err) {
		t.Error("whiteout did not delete gone")
	}
	if _, err := os.Lstat(filepath.Join(data, "dir", "old")); !os.IsNotExist(err) {
		t.Error("opaque dir did not drop old")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "dir", "new")); string(b) != "n" {
		t.Error("dir/new missing after apply")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
