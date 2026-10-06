package examples

import (
	"os"
	"path/filepath"
	"testing"

	"box/internal/boxfile"
)

// Every shipped example must parse: `box new --from` writes it verbatim,
// so a broken one would fail at the user's first build.
func TestEveryExampleParses(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no examples embedded")
	}
	for _, n := range names {
		bf, _, err := Load(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		p := filepath.Join(t.TempDir(), "Boxfile")
		if err := os.WriteFile(p, bf, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := boxfile.Parse(p); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestUnknownExampleIsRefused(t *testing.T) {
	for _, n := range []string{"nope", "../internal", "", "."} {
		if _, _, err := Load(n); err == nil {
			t.Errorf("Load(%q) succeeded", n)
		}
	}
}
