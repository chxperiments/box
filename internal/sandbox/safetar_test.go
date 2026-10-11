package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a"), "A")
	write(t, filepath.Join(src, "d", "b"), "B")
	os.Symlink("a", filepath.Join(src, "rel"))
	os.Chmod(filepath.Join(src, "a"), 0o4750)
	var buf bytes.Buffer
	if err := WriteTar(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := SafeUntar(&buf, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "d", "b")); string(b) != "B" {
		t.Error("nested file lost")
	}
	if l, _ := os.Readlink(filepath.Join(dst, "rel")); l != "a" {
		t.Error("symlink not kept as a symlink")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "a")); fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, want 0750 without setuid", fi.Mode())
	}
}

// A guest-made archive must not be able to write outside the destination,
// by path or by a symlink one entry plants for the next to write through.
func TestSafeUntarStaysInside(t *testing.T) {
	outside := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		tw.WriteHeader(h)
		tw.Write([]byte(body))
	}
	add(&tar.Header{Name: "../../escape", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "/abs", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside}, "")
	add(&tar.Header{Name: "link/pwned", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, "")
	add(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}, "")
	tw.Close()

	dst := t.TempDir()
	skipped, err := SafeUntar(&buf, dst)
	if err != nil {
		t.Fatalf("a hostile entry must be skipped, not abort the rest: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the destination: %v", entries)
	}
	// link/pwned (through the planted symlink), the device and the hard link.
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
	// Absolute and ../ names are confined to dst, not dropped.
	for _, p := range []string{"escape", "abs"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("%s should land inside dst: %v", p, err)
		}
	}
}

// gzipTar builds a gzip-compressed archive from headers and bodies.
func gzipTar(t *testing.T, entries ...any) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < len(entries); i += 2 {
		h, body := entries[i].(*tar.Header), entries[i+1].(string)
		h.Size = int64(len(body))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractArchiveKeepsTree(t *testing.T) {
	buf := gzipTar(t,
		&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}, "",
		&tar.Header{Name: "./ro/", Typeflag: tar.TypeDir, Mode: 0o555}, "",
		&tar.Header{Name: "./ro/f", Typeflag: tar.TypeReg, Mode: 0o4755}, "F",
		&tar.Header{Name: "./abs", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin/python3"}, "",
		&tar.Header{Name: "./hard", Typeflag: tar.TypeLink, Linkname: "./ro/f"}, "",
	)
	dst := t.TempDir()
	if err := ExtractArchive(buf, dst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dst, "ro"), 0o755) })
	fi, err := os.Stat(filepath.Join(dst, "ro", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 without setuid", fi.Mode())
	}
	if di, _ := os.Stat(filepath.Join(dst, "ro")); di.Mode().Perm() != 0o755 {
		t.Errorf("a read-only directory should stay enterable by its owner: %v", di.Mode())
	}
	// Guest symlinks are resolved inside the guest, so any target is kept.
	if l, _ := os.Readlink(filepath.Join(dst, "abs")); l != "/usr/bin/python3" {
		t.Errorf("symlink target = %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "hard")); string(b) != "F" {
		t.Errorf("hard link = %q", b)
	}
}

// Every one of these is refused outright, and nothing lands outside dst.
func TestExtractArchiveRefuses(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	reg := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	for name, entries := range map[string][]any{
		"traversal":       {reg("../x"), "x"},
		"absolute":        {reg("/x"), "x"},
		"through symlink": {&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: outside}, "", reg("l/pwned"), "x"},
		"through inner symlink": {
			&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}, "",
			&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "d"}, "", reg("l/x"), "x"},
		"replace symlink": {&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: outside + "/secret"}, "", reg("l"), "x"},
		"hard link out":   {&tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../secret"}, ""},
		"hard link abs":   {&tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: outside + "/secret"}, ""},
		"hard link via symlink": {
			&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: outside}, "",
			&tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "l/secret"}, ""},
		"device": {&tar.Header{Name: "null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, ""},
		"fifo":   {&tar.Header{Name: "p", Typeflag: tar.TypeFifo}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ExtractArchive(gzipTar(t, entries...), t.TempDir()); err == nil {
				t.Error("should be refused")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 1 {
				t.Fatalf("wrote outside the destination: %v", entries)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "s" {
				t.Fatal("a file outside the destination was changed")
			}
		})
	}
}

// Only gzip-compressed tar is read, whatever else a host tar would accept.
func TestExtractArchiveRefusesOtherFormats(t *testing.T) {
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Close()
	if err := ExtractArchive(&plain, t.TempDir()); err == nil {
		t.Error("an uncompressed tar should be refused")
	}
}
