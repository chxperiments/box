package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// FuzzExtractArchive feeds arbitrary bytes to the restore reader. Whatever it
// accepts or refuses, nothing may appear beside the destination.
func FuzzExtractArchive(f *testing.F) {
	seed := func(entries ...*tar.Header) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, h := range entries {
			if h.Typeflag == tar.TypeReg {
				h.Size = 1
			}
			tw.WriteHeader(h)
			if h.Typeflag == tar.TypeReg {
				tw.Write([]byte("x"))
			}
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	f.Add(seed(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./a", Typeflag: tar.TypeReg, Mode: 0o644}))
	f.Add(seed(&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: ".."},
		&tar.Header{Name: "l/x", Typeflag: tar.TypeReg, Mode: 0o644}))
	f.Add(seed(&tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../x"}))
	f.Add(seed(&tar.Header{Name: "../x", Typeflag: tar.TypeReg, Mode: 0o644}))
	f.Add([]byte("not gzip"))

	f.Fuzz(func(t *testing.T, b []byte) {
		parent := t.TempDir()
		dest := filepath.Join(parent, "dest")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		ExtractArchive(bytes.NewReader(b), dest)
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "dest" {
			t.Fatalf("wrote beside the destination: %v", entries)
		}
		// Leave it removable for t.TempDir's cleanup.
		filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}
