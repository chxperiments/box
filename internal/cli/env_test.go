package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"box/internal/boxfile"
)

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":         "''",
		"plain":    "'plain'",
		"a b":      "'a b'",
		"it's":     `'it'\''s'`,
		"$(id)`x`": "'$(id)`x`'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// `box env` is documented for eval, and base and env come from a Boxfile
// that may be someone else's. Evaluating the output must reproduce every value
// literally, run nothing, and assign only BOX_* variables.
func TestEnvLinesAreInertUnderEval(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	s := boxfile.Default
	s.Base = "docker.io/library/alpine:latest$(touch${IFS}" + marker + ")"
	s.Env = map[string]string{
		"LANG":      "C.UTF-8;touch " + marker,
		"JAVA_OPTS": "-Xmx1g touch " + marker,
		"QUOTE":     `it's "fine" $HOME ` + "`touch " + marker + "`",
		"PATH":      "/nonexistent",
	}
	lines := envLines("demo", "/data dir/demo", s)

	script := "eval \"$1\"\n" +
		`printf '%s\n' "$BOX_BASE" "$BOX_DATA" "$BOX_ENV_LANG" "$BOX_ENV_JAVA_OPTS" "$BOX_ENV_QUOTE" "$BOX_ENV_PATH" "$PATH"`
	cmd := exec.Command(sh, "-c", script, "sh", strings.Join(lines, "\n"))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("evaluating box env output ran a command from the Boxfile")
	}
	want := []string{s.Base, "/data dir/demo", s.Env["LANG"], s.Env["JAVA_OPTS"], s.Env["QUOTE"], "/nonexistent", "/usr/bin:/bin"}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("value %d = %q, want %q", i, got[i], want[i])
		}
	}
}
