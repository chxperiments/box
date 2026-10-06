// Package boxfile parses a Boxfile, the single declarative spec for a
// sandbox, and generates the Containerfile podman builds from.
//
// A Boxfile is YAML describing both the VM (cpus, ram, network, restrictions)
// and its image (base, packages, run steps), so a user never writes a
// Containerfile by hand:
//
//	base: docker.io/library/debian:bookworm-slim
//	cpus: 4
//	ram_mib: 4096
//	network: bridge
//	readonly: true
//	timeout_seconds: 300
//	packages:
//	  - python3
//	  - git
//	run:
//	  - pip3 install --break-system-packages requests
//	env:
//	  LANG: C.UTF-8
package boxfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

// Spec is the parsed, validated contents of a Boxfile.
type Spec struct {
	Base           string            `yaml:"base"`
	CPUs           int               `yaml:"cpus"`
	RAMMiB         int               `yaml:"ram_mib"`
	Network        string            `yaml:"network"` // "bridge" (egress) or "none"
	Passt          bool              `yaml:"passt"`   // real in-guest interface + default route
	ReadOnlyRootfs bool              `yaml:"readonly"`
	TimeoutSeconds int               `yaml:"timeout_seconds"` // 0 = unlimited
	Warm           int               `yaml:"warm"`            // VMs kept booted for run; 0 = boot per run
	Isolation      string            `yaml:"isolation"`       // "standard" or "strict": VMM under its own host UID
	Backend        string            `yaml:"backend"`         // what boots the VM: podman (default), krun, firecracker
	Seccomp        string            `yaml:"seccomp"`         // profile path, filters the VMM
	PkgMgr         string            `yaml:"pkgmgr"`          // apk/apt/dnf; empty = infer from base
	Packages       []string          `yaml:"packages"`
	Run            []string          `yaml:"run"`    // extra Containerfile RUN steps, in order
	Warmup         []string          `yaml:"warmup"` // shell lines run in each warm VM before its first run
	Env            map[string]string `yaml:"env"`
	Mounts         []Mount           `yaml:"mounts"` // explicit host shares, on top of /data
	Blueprint      Blueprint         `yaml:"blueprint"`
}

// Mount is an explicit host directory shared into the guest. Unlike the
// implicit /data share, each mount states what it exposes and whether the
// guest may write it.
type Mount struct {
	Host  string `yaml:"host"`  // absolute host path; ~ expands to the home dir
	Guest string `yaml:"guest"` // absolute guest path
	Mode  string `yaml:"mode"`  // "ro" (default) or "rw"
}

// Blueprint is cloud-init-style provisioning. Unlike cloud-init it is applied
// at build time, not first boot: every run is a fresh VM, so provisioning at
// boot would repeat on every command.
type Blueprint struct {
	Users      []User      `yaml:"users"`
	WriteFiles []WriteFile `yaml:"write_files"`
	RunCmd     []string    `yaml:"runcmd"`
}

type User struct {
	Name  string `yaml:"name"`
	Shell string `yaml:"shell"` // defaults to /bin/sh
	Sudo  bool   `yaml:"sudo"`  // passwordless sudo; needs sudo in packages
}

type WriteFile struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
	Mode    string `yaml:"mode"` // e.g. "0755"; optional
}

func (b Blueprint) empty() bool {
	return len(b.Users) == 0 && len(b.WriteFiles) == 0 && len(b.RunCmd) == 0
}

// MaxWarm bounds the warm pool: every pooled VM holds its RAM while it waits.
const MaxWarm = 8

// Default supplies values a Boxfile omits.
var Default = Spec{
	Base:      "docker.io/library/alpine:latest",
	CPUs:      2,
	RAMMiB:    2048,
	Network:   "bridge",
	Isolation: "standard",
	Backend:   "podman",
}

// Backends are the values `backend:` accepts. podman drives crun+libkrun
// through podman; krun drives crun+libkrun directly, skipping podman's
// per-boot cost; firecracker uses the Firecracker VMM.
var Backends = []string{"podman", "krun", "firecracker"}

// BackendNames lists the accepted backends for error messages.
func BackendNames() string { return strings.Join(Backends, ", ") }

// Template is written by `box new`.
const Template = `# Boxfile -- the whole sandbox in one place. box generates the
# Containerfile from this; you never edit that directly.

base: docker.io/library/alpine:latest

cpus: 2
ram_mib: 2048
network: bridge       # bridge = internet access, none = offline
passt: false          # real NIC + default route in the guest (needed for k3s)
readonly: false       # read-only guest root (/tmp and /data stay writable)
isolation: standard   # strict: VMM runs as a UID that is not yours (use for agents)
backend: podman       # podman (default) | krun (direct, faster boots) | firecracker (snapshot per run, no network yet)
timeout_seconds: 0    # per-run wall clock limit, 0 = unlimited
warm: 0               # VMs kept booted so run starts in ms (each holds its RAM)

# Tools baked into the image. Reset every run; keep work in /data.
packages:
  - bash
  - curl
  - git

# Extra build steps, run in order:
# run:
#   - pip3 install --break-system-packages requests

# Host directories shared into the guest (on top of /data):
# mounts:
#   - host: ~/projects/demo
#     guest: /work
#     mode: ro              # ro (default) or rw

# env:
#   LANG: C.UTF-8
`

// Parse reads and validates a Boxfile. Unknown keys are rejected so a typo
// fails loudly instead of being silently ignored.
func Parse(path string) (Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	s := Default
	// Decoding empty input clears the destination, wiping the defaults, so
	// skip it entirely: a Boxfile with no content means "all defaults".
	if len(bytes.TrimSpace(b)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(b), yaml.DisallowUnknownField())
		if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
			return Spec{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := s.validate(); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Grammar rules for spec fields that are data, not instructions. A value
// violating its rule could otherwise change the structure of the generated
// Containerfile (a newline in an env value becomes a new instruction, a mode
// like "0755; x" becomes a second shell command), so it is rejected at parse
// time instead of being escaped at render time.
var (
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	modeRe   = regexp.MustCompile(`^[0-7]{3,4}$`)
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	shellRe  = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)
	pathRe   = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)
)

func (s Spec) validate() error {
	if s.Base == "" {
		return errors.New("base is required")
	}
	if strings.ContainsAny(s.Base, " \t\r\n\x00") {
		return fmt.Errorf("base must be an image reference without whitespace or control characters, got %q", s.Base)
	}
	if s.CPUs < 1 || s.CPUs > 16 {
		return fmt.Errorf("cpus must be 1-16 (krun's limit), got %d", s.CPUs)
	}
	if s.RAMMiB < 128 {
		return fmt.Errorf("ram_mib must be at least 128, got %d", s.RAMMiB)
	}
	if s.Network != "bridge" && s.Network != "none" {
		return fmt.Errorf("network must be bridge or none, got %q", s.Network)
	}
	if !slices.Contains(Backends, s.Backend) {
		return fmt.Errorf("backend must be one of %s, got %q", BackendNames(), s.Backend)
	}
	if s.Backend == "firecracker" {
		// /data is a disk there, not a shared directory, and a restore from
		// a snapshot already starts in tens of milliseconds.
		switch {
		case len(s.Mounts) > 0:
			return errors.New("backend: firecracker has no host mounts; move files with `box data import`")
		case s.Warm > 0:
			return errors.New("backend: firecracker restores a snapshot per run; drop warm")
		case s.Network != "none":
			return errors.New("backend: firecracker supports network: none for now")
		case s.Passt:
			return errors.New("backend: firecracker does not use passt")
		}
	}
	if s.Isolation != "standard" && s.Isolation != "strict" {
		return fmt.Errorf("isolation must be standard or strict, got %q", s.Isolation)
	}
	if s.Warm < 0 || s.Warm > MaxWarm {
		return fmt.Errorf("warm must be 0-%d, got %d", MaxWarm, s.Warm)
	}
	for i, w := range s.Warmup {
		if strings.TrimSpace(w) == "" || strings.ContainsRune(w, 0) {
			return fmt.Errorf("warmup[%d] must be a non-empty shell line", i)
		}
	}
	if s.Warm > 0 && s.Network == "none" {
		// A warm VM is reached through its agent's published port, and
		// podman publishes no port on a sandbox without a network.
		return errors.New("warm needs network: bridge; an offline sandbox boots per run")
	}
	if s.TimeoutSeconds < 0 {
		return fmt.Errorf("timeout_seconds must be >= 0, got %d", s.TimeoutSeconds)
	}
	if s.Seccomp != "" {
		if _, err := os.Stat(s.Seccomp); err != nil {
			return fmt.Errorf("seccomp profile: %w", err)
		}
	}
	if s.PkgMgr != "" && !isKnownMgr(s.PkgMgr) {
		return fmt.Errorf("pkgmgr must be apk, apt or dnf, got %q", s.PkgMgr)
	}
	// A blueprint needs the package manager too, for the right useradd form.
	if len(s.Packages) > 0 || len(s.Blueprint.Users) > 0 {
		if _, ok := s.pkgMgr(); !ok {
			return fmt.Errorf("cannot infer a package manager for base %q; "+
				"set pkgmgr to apk, apt or dnf", s.Base)
		}
	}
	for i, u := range s.Blueprint.Users {
		if strings.TrimSpace(u.Name) == "" {
			return fmt.Errorf("blueprint.users[%d]: name is required", i)
		}
		if !userRe.MatchString(u.Name) {
			return fmt.Errorf("blueprint.users[%d]: name must be lowercase letters, digits, '_' or '-' (max 32), got %q", i, u.Name)
		}
		if u.Shell != "" && !shellRe.MatchString(u.Shell) {
			return fmt.Errorf("blueprint.users[%d]: shell must be an absolute path (letters, digits, '/', '.', '_', '-'), got %q", i, u.Shell)
		}
	}
	for i, f := range s.Blueprint.WriteFiles {
		// The path is interpolated into a COPY and, when a mode is set, into a
		// RUN chmod line -- so it is build grammar the same way mode is. A path
		// like "/tmp/x; touch /PWNED" would inject a second shell command.
		if !pathRe.MatchString(f.Path) {
			return fmt.Errorf("blueprint.write_files[%d]: path must be an absolute path (letters, digits, '/', '.', '_', '-'), got %q", i, f.Path)
		}
		if f.Mode != "" && !modeRe.MatchString(f.Mode) {
			return fmt.Errorf("blueprint.write_files[%d]: mode must be octal (e.g. \"0644\"), got %q", i, f.Mode)
		}
	}
	for _, k := range s.EnvKeys() {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("env: key must be an identifier (letters, digits, '_'), got %q", k)
		}
		if strings.ContainsAny(s.Env[k], "\r\n") {
			return fmt.Errorf("env[%s]: value must not contain a newline; put build steps in 'run' instead", k)
		}
	}
	for i, p := range s.Packages {
		if p == "" || strings.ContainsAny(p, " \t\r\n;|&$`") {
			return fmt.Errorf("packages[%d]: %q is not a plain package name", i, p)
		}
	}
	// Mounts are the most security-relevant surface in the spec, so every
	// field is checked here and normalized (tilde expanded, mode defaulted)
	// rather than re-decided at run time.
	seen := map[string]bool{"/data": true} // /data belongs to the implicit share
	for i := range s.Mounts {
		m := &s.Mounts[i]
		host, err := expandTilde(m.Host)
		if err != nil {
			return fmt.Errorf("mounts[%d]: %w", i, err)
		}
		m.Host = host
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("mounts[%d]: host path must be absolute (or ~), got %q", i, m.Host)
		}
		// A ':' would split the host off into extra fields of the -v spec, so a
		// colon in the path silently mounts the wrong thing. Reject it here
		// rather than hand podman a corrupt volume argument.
		if strings.ContainsRune(m.Host, ':') {
			return fmt.Errorf("mounts[%d]: host path must not contain ':', got %q", i, m.Host)
		}
		if !pathRe.MatchString(m.Guest) {
			return fmt.Errorf("mounts[%d]: guest path must be absolute (letters, digits, '/', '.', '_', '-'), got %q", i, m.Guest)
		}
		if m.Mode == "" {
			m.Mode = "ro" // fail safe: a mount you forgot to make writable stays read-only
		}
		if m.Mode != "ro" && m.Mode != "rw" {
			return fmt.Errorf("mounts[%d]: mode must be ro or rw, got %q", i, m.Mode)
		}
		if m.Mode == "rw" && s.Isolation == "strict" {
			// Under strict isolation the VMM runs as a UID that is not yours,
			// so it could only write your directory if that directory were
			// re-owned to it -- which would take it away from you.
			return fmt.Errorf("mounts[%d]: isolation: strict allows read-only mounts only; "+
				"exchange files through /data instead", i)
		}
		if seen[m.Guest] {
			return fmt.Errorf("mounts[%d]: guest path %q is mounted more than once", i, m.Guest)
		}
		seen[m.Guest] = true
	}
	return nil
}

// expandTilde resolves a leading ~ to the user's home directory.
func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", p, err)
	}
	return filepath.Join(h, strings.TrimPrefix(p, "~")), nil
}

// EnvKeys returns the env keys in sorted order.
func (s Spec) EnvKeys() []string {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pkgMgr returns the package manager for this spec: pkgmgr if set, otherwise
// inferred from the base image name. ok is false when neither works, so the
// caller can ask for pkgmgr instead of emitting a Containerfile that fails
// halfway through a build.
func (s Spec) pkgMgr() (mgr string, ok bool) {
	if s.PkgMgr != "" {
		return strings.ToLower(s.PkgMgr), isKnownMgr(s.PkgMgr)
	}
	l := strings.ToLower(s.Base)
	for _, d := range []struct{ match, mgr string }{
		{"alpine", "apk"},
		{"debian", "apt"},
		{"ubuntu", "apt"},
		{"fedora", "dnf"},
		{"rockylinux", "dnf"},
		{"almalinux", "dnf"},
		{"centos", "dnf"},
	} {
		if strings.Contains(l, d.match) {
			return d.mgr, true
		}
	}
	return "", false
}

func isKnownMgr(m string) bool {
	switch strings.ToLower(m) {
	case "apk", "apt", "dnf":
		return true
	}
	return false
}

// assetDir holds blueprint file contents inside the build context. Contents go
// through COPY rather than being escaped into a RUN, so arbitrary text (quotes,
// newlines, shell metacharacters) survives intact.
const assetDir = ".blueprint"

// Render writes any blueprint assets into the build context at dir and returns
// the Containerfile text. Use it instead of Containerfile when the spec may
// carry a blueprint.
func (s Spec) Render(dir string) (string, error) {
	target := filepath.Join(dir, assetDir)
	// Clear stale assets so a removed write_files entry cannot linger.
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if len(s.Blueprint.WriteFiles) > 0 {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", err
		}
		for i, f := range s.Blueprint.WriteFiles {
			p := filepath.Join(target, assetName(i))
			if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
				return "", err
			}
		}
	}
	return s.Containerfile(), nil
}

func assetName(i int) string { return "f" + strconv.Itoa(i) }

// Containerfile renders the image build instructions for this spec. The package
// manager comes from pkgmgr, or is inferred from the base image name. Blueprint
// write_files are referenced by COPY; call Render first to materialise them.
func (s Spec) Containerfile() string {
	var b strings.Builder
	b.WriteString("# generated by box from the Boxfile -- do not edit\n")
	fmt.Fprintf(&b, "FROM %s\n\n", s.Base)

	// Sorted so the same Boxfile always yields byte-identical output, which
	// keeps podman's layer cache warm across rebuilds.
	if len(s.Env) > 0 {
		for _, k := range s.EnvKeys() {
			fmt.Fprintf(&b, "ENV %s=%s\n", k, s.Env[k])
		}
		b.WriteByte('\n')
	}
	if len(s.Packages) > 0 {
		mgr, _ := s.pkgMgr() // validate() already guaranteed this resolves
		fmt.Fprintf(&b, "RUN %s\n\n", installCmd(mgr, s.Packages))
	}
	for _, r := range s.Run {
		fmt.Fprintf(&b, "RUN %s\n", r)
	}
	if len(s.Run) > 0 {
		b.WriteByte('\n')
	}
	if !s.Blueprint.empty() {
		mgr, _ := s.pkgMgr()
		b.WriteString(s.Blueprint.render(mgr))
	}
	b.WriteString("WORKDIR /data\n")
	return b.String()
}

// render emits the blueprint as Containerfile steps: users, then files, then
// commands -- so a runcmd can rely on both existing.
func (b Blueprint) render(mgr string) string {
	var out strings.Builder
	for _, u := range b.Users {
		shell := u.Shell
		if shell == "" {
			shell = "/bin/sh"
		}
		if mgr == "apk" {
			fmt.Fprintf(&out, "RUN adduser -D -s %s %s\n", shell, u.Name)
		} else {
			fmt.Fprintf(&out, "RUN useradd -m -s %s %s\n", shell, u.Name)
		}
		if u.Sudo {
			// wheel on apk/dnf, sudo on apt; the sudoers drop-in is what
			// actually grants access, the group just matches convention.
			group := "wheel"
			if mgr == "apt" {
				group = "sudo"
			}
			fmt.Fprintf(&out, "RUN usermod -aG %s %s || addgroup %s %s || true\n",
				group, u.Name, u.Name, group)
			fmt.Fprintf(&out, "RUN mkdir -p /etc/sudoers.d && "+
				"echo '%s ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/%s && "+
				"chmod 0440 /etc/sudoers.d/%s\n", u.Name, u.Name, u.Name)
		}
	}
	for i, f := range b.WriteFiles {
		fmt.Fprintf(&out, "COPY %s/%s %s\n", assetDir, assetName(i), f.Path)
		if f.Mode != "" {
			fmt.Fprintf(&out, "RUN chmod %s %s\n", f.Mode, f.Path)
		}
	}
	for _, c := range b.RunCmd {
		fmt.Fprintf(&out, "RUN %s\n", c)
	}
	if out.Len() > 0 {
		out.WriteByte('\n')
	}
	return out.String()
}

// installCmd builds a non-interactive install line, with the cache cleanup each
// manager needs to avoid bloating the image layer.
func installCmd(mgr string, pkgs []string) string {
	list := strings.Join(pkgs, " ")
	switch mgr {
	case "apt":
		return "apt-get update && apt-get install -y --no-install-recommends " +
			list + " && rm -rf /var/lib/apt/lists/*"
	case "dnf":
		return "dnf install -y " + list + " && dnf clean all"
	default: // apk
		return "apk add --no-cache " + list
	}
}
