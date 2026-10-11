package boxfile

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParse checks the Boxfile parser never panics, and that whatever it
// accepts still passes validation, so no input reaches Render unchecked.
func FuzzParse(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte(Template))
	f.Add([]byte("base: alpine\ncpus: 2\nenv:\n  A: b\n"))
	f.Add([]byte("base: alpine\nmounts:\n  - host: /tmp\n    guest: /work\n    mode: rw\n"))
	f.Add([]byte("base: [\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		p := filepath.Join(t.TempDir(), "Boxfile")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Parse(p)
		if err != nil {
			return
		}
		if err := s.validate(); err != nil {
			t.Fatalf("accepted a spec that fails validation: %v", err)
		}
	})
}
