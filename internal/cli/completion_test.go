package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// makeSandboxes creates real sandboxes under a temporary box home, so the
// completions are read the same way the running CLI reads them.
func makeSandboxes(t *testing.T, names ...string) {
	t.Helper()
	t.Setenv("BOX_HOME", t.TempDir())
	for _, n := range names {
		if _, err := sandbox.Create(n); err != nil {
			t.Fatal(err)
		}
		p, err := sandbox.BoxfilePath(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(boxfile.Template), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompleteName(t *testing.T) {
	makeSandboxes(t, "devbox", "demo", "other")

	got, d := completeName(nil, nil, "")
	if len(got) != 3 {
		t.Errorf("completions = %v, want all three sandboxes", got)
	}
	if d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", d)
	}
	// Cobra does not filter what a completion function returns, so the prefix
	// has to be honoured here or the shell offers non-matching names.
	if got, _ := completeName(nil, nil, "de"); len(got) != 2 {
		t.Errorf("completions for %q = %v, want devbox and demo", "de", got)
	}
	if got, _ := completeName(nil, nil, "zzz"); len(got) != 0 {
		t.Errorf("completions for an unmatched prefix = %v, want none", got)
	}
	// The name is one argument: past it there is nothing left to offer.
	if got, _ := completeName(nil, []string{"devbox"}, ""); got != nil {
		t.Errorf("completions after the name = %v, want none", got)
	}
}

// With no box home to read, completion stays silent rather than failing
// the shell or falling back to offering file names.
func TestCompleteNameWithoutAnySandboxes(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	got, d := completeName(nil, nil, "")
	if len(got) != 0 || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completeName = %v %v, want none and NoFileComp", got, d)
	}
}

func TestCompleteSnapshot(t *testing.T) {
	makeSandboxes(t, "devbox")
	for _, stamp := range []string{"20260101T000000Z", "20260202T000000Z"} {
		if _, err := sandbox.Snapshot("devbox", stamp); err != nil {
			t.Fatal(err)
		}
	}
	// The first argument is still the sandbox.
	if got, _ := completeSnapshot(nil, nil, ""); len(got) != 1 || got[0] != "devbox" {
		t.Errorf("first argument completions = %v, want [devbox]", got)
	}
	// The second is that sandbox's snapshots, by stamp and newest first --
	// the same short form restore accepts.
	got, d := completeSnapshot(nil, []string{"devbox"}, "")
	want := []string{"20260202T000000Z", "20260101T000000Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("snapshot completions = %v, want %v", got, want)
	}
	if d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", d)
	}
	if got, _ := completeSnapshot(nil, []string{"devbox"}, "20260101"); len(got) != 1 {
		t.Errorf("prefixed completions = %v, want one", got)
	}
	// restore also takes an archive kept elsewhere, so once it looks like a
	// path the shell's own file completion is the useful answer.
	if _, d := completeSnapshot(nil, []string{"devbox"}, "/tmp"+string(filepath.Separator)); d != cobra.ShellCompDirectiveDefault {
		t.Errorf("directive for a path = %v, want Default", d)
	}
	// A sandbox with no snapshots offers nothing, not an error.
	makeSandboxes(t, "empty")
	if got, _ := completeSnapshot(nil, []string{"empty"}, ""); len(got) != 0 {
		t.Errorf("completions for a sandbox without snapshots = %v", got)
	}
}

// Every command that takes a sandbox name must complete it: a command wired up
// without one silently falls back to offering file names.
func TestEveryNamedCommandCompletes(t *testing.T) {
	for _, c := range newRoot().Commands() {
		if !strings.Contains(c.Use, "<") {
			continue // takes no name
		}
		if c.ValidArgsFunction == nil {
			t.Errorf("%q takes an argument but has no completion", c.Use)
		}
	}
}
