package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// CheckIsolation boots the sandbox two ways and confirms the guest kernel
// differs from the baseline a plain container sees. A shared kernel means
// podman fell back to a container, so it is reported as an error rather than a
// silent pass. Comparing against the baseline rather than the host's own uname
// is what keeps this honest on macOS, where every container kernel differs from
// the Darwin host whether or not a microVM is involved.
func CheckIsolation(name string, s boxfile.Spec) (guest, baseline string, err error) {
	guest, err = GuestKernel(name, s)
	if err != nil {
		return "", "", fmt.Errorf("could not start the sandbox (is the image built?): %w", err)
	}
	if guest == "" {
		return "", "", fmt.Errorf("could not start the sandbox; is the image built?")
	}
	baseline, err = BaselineKernel(name, s)
	if err != nil {
		return "", "", fmt.Errorf("could not read the baseline kernel: %w", err)
	}
	if baseline == "" {
		return "", "", fmt.Errorf("could not read the baseline kernel")
	}
	if guest == baseline {
		return guest, baseline, fmt.Errorf("not isolated: the sandbox shares kernel %s.\n"+
			"This is a container, not a microVM", baseline)
	}
	return guest, baseline, nil
}

// RuntimeIdentity fingerprints the parts of the toolchain that decide whether a
// sandbox actually gets its own kernel. When any of them changes -- the krun
// symlink is re-pointed, crun is upgraded, libkrun support is dropped, podman
// changes -- the fingerprint changes and a cached verification no longer
// applies. It is cheap (a couple of --version calls and a stat), so it can gate
// every run without the cost of booting a VM.
func RuntimeIdentity() (string, error) {
	parts := []string{goruntime.GOOS + "/" + goruntime.GOARCH}

	if goruntime.GOOS == "darwin" {
		// Isolation lives inside the podman machine VM, so its identity, not a
		// host-side krun, is what matters.
		out, err := exec.Command("podman", "machine", "inspect", "--format",
			"{{.Name}} {{.Rootful}}").Output()
		if err != nil {
			return "", fmt.Errorf("no podman machine to fingerprint: %w", err)
		}
		parts = append(parts, "machine:"+strings.TrimSpace(string(out)))
	} else {
		// The krun symlink and the crun binary it resolves to are the whole
		// isolation path; capture where it points and the binary's identity so
		// a re-pointed link or an upgraded crun both change the fingerprint.
		krun, err := exec.LookPath("krun")
		if err != nil {
			return "", fmt.Errorf("no 'krun' on PATH")
		}
		real, err := filepath.EvalSymlinks(krun)
		if err != nil {
			real = krun
		}
		parts = append(parts, "krun:"+krun+"->"+real)
		if fi, err := os.Stat(real); err == nil {
			parts = append(parts, fmt.Sprintf("crun:%d:%d", fi.Size(), fi.ModTime().UnixNano()))
		}
		// The version string carries the +LIBKRUN tag; losing it is exactly the
		// degradation this guards against.
		if out, err := exec.Command("krun", "--version").Output(); err == nil {
			parts = append(parts, "krunver:"+strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("podman", "--version").Output(); err == nil {
		parts = append(parts, "podman:"+strings.TrimSpace(string(out)))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// verifyToken is the cached result of a successful isolation check. A run
// trusts it only while Identity still matches the current runtime.
type verifyToken struct {
	Identity   string `json:"identity"`
	Guest      string `json:"guest"`
	Baseline   string `json:"baseline"`
	VerifiedAt string `json:"verified_at"`
}

func loadToken() (verifyToken, bool) {
	p, err := sandbox.VerifyCachePath()
	if err != nil {
		return verifyToken{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return verifyToken{}, false
	}
	var t verifyToken
	if json.Unmarshal(b, &t) != nil {
		return verifyToken{}, false
	}
	return t, true
}

// storeToken records a verification. It is best-effort: a cache-write failure
// only costs a re-verify next time, so it must never fail a build or run.
func storeToken(id, guest, baseline string) {
	p, err := sandbox.VerifyCachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	if b, err := json.Marshal(verifyToken{
		Identity:   id,
		Guest:      guest,
		Baseline:   baseline,
		VerifiedAt: time.Now().UTC().Format(time.RFC3339),
	}); err == nil {
		os.WriteFile(p, b, 0o644)
	}
}

// isolationCurrent reports whether the cached token already cleared runtime id.
func isolationCurrent(id string) bool {
	t, ok := loadToken()
	return ok && t.Identity == id
}

// MarkVerified caches a successful isolation check for the current runtime, so
// later runs can trust it without booting two more VMs. Callers that have just
// proven isolation (build, the verify command) use it to prime the cache.
func MarkVerified(guest, baseline string) {
	if id, err := RuntimeIdentity(); err == nil {
		storeToken(id, guest, baseline)
	}
}

// EnsureIsolated gates a run: it confirms the sandbox still gets its own kernel
// before the command executes. The full check boots two VMs, so it is skipped
// when the runtime is byte-for-byte the one a previous verification already
// cleared; a changed runtime forces a fresh check and a failed check refuses
// the run rather than executing on a shared kernel. It returns fresh=true when
// it had to re-verify, so the caller can explain the pause.
func EnsureIsolated(name string, s boxfile.Spec) (fresh bool, err error) {
	id, err := RuntimeIdentity()
	if err != nil {
		return false, err
	}
	if isolationCurrent(id) {
		return false, nil // this exact runtime is already proven
	}
	guest, baseline, err := CheckIsolation(name, s)
	if err != nil {
		return true, err
	}
	storeToken(id, guest, baseline)
	return true, nil
}
