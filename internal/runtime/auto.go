package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// backend: auto lets box pick the engine. It picks firecracker when that
// fits -- the Boxfile asks for nothing firecracker cannot do, the host can
// run it, and the sandbox has no forks -- because its VMM holds no
// capabilities and has the smallest device model; podman otherwise, which
// does everything.
//
// The pick is made once and recorded with the sandbox, because the two keep
// /data differently (a disk on firecracker, a directory on podman), and a
// sandbox that silently changed engine would lose sight of its data. A later
// Boxfile change that the recorded engine cannot serve switches engines only
// while /data is empty; otherwise it stops and says how to move the data.

// AutoChoice is what backend: auto picked for a sandbox, and why.
type AutoChoice struct {
	Backend string `json:"backend"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

func autoPath(name string) (string, error) {
	dir, err := sandbox.Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "backend.json"), nil
}

// ReadAutoChoice returns the recorded choice, if backend: auto made one.
func ReadAutoChoice(name string) (AutoChoice, bool) {
	p, err := autoPath(name)
	if err != nil {
		return AutoChoice{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return AutoChoice{}, false
	}
	var c AutoChoice
	if json.Unmarshal(b, &c) != nil || c.Backend == "" {
		return AutoChoice{}, false
	}
	return c, true
}

func writeAutoChoice(name string, c AutoChoice) error {
	p, err := autoPath(name)
	if err != nil {
		return err
	}
	c.At = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// firecrackerFits says why firecracker cannot run this sandbox here, or nil.
func firecrackerFits(name string, s boxfile.Spec) error {
	if err := boxfile.FirecrackerFits(s); err != nil {
		return err
	}
	if sandbox.IsFork(name) {
		return fmt.Errorf("forks run on podman")
	}
	if forks, _ := sandbox.Forks(name); len(forks) > 0 {
		return fmt.Errorf("it has forks, which run on podman")
	}
	return fcHostCheck()
}

// fcHostCheck is firecracker's preflight, replaceable in tests.
var fcHostCheck = func() error { return backends["firecracker"].Preflight() }

// HostFirecracker says whether this host can run firecracker at all, for
// box doctor.
func HostFirecracker() error { return fcHostCheck() }

// autoPick is the engine backend: auto would choose now, and why.
func autoPick(name string, s boxfile.Spec) (string, string) {
	if err := firecrackerFits(name, s); err != nil {
		return "podman", "this sandbox needs podman: " + strings.TrimPrefix(err.Error(), "backend: ")
	}
	return "firecracker", "offline, no host mounts, no warm pool, and the host can run it: firecracker's VMM holds no capabilities"
}

func backendFits(backend, name string, s boxfile.Spec) bool {
	if backend == "firecracker" {
		return firecrackerFits(name, s) == nil
	}
	return true // podman runs anything a Boxfile can ask for
}

// dataInUse reports whether a sandbox has data where backend keeps it.
func dataInUse(name, backend string) bool {
	if backend == "firecracker" {
		return FirecrackerDataExists(name)
	}
	empty, err := sandbox.DataEmpty(name)
	return err == nil && !empty
}

// resolveAuto turns backend: auto into the sandbox's engine.
func resolveAuto(name string, s boxfile.Spec) (boxfile.Spec, error) {
	pick, why := autoPick(name, s)
	rec, ok := ReadAutoChoice(name)
	switch {
	case !ok:
		if err := writeAutoChoice(name, AutoChoice{Backend: pick, Reason: why}); err != nil {
			return s, err
		}
		s.Backend = pick
	case rec.Backend == pick || backendFits(rec.Backend, name, s):
		s.Backend = rec.Backend // stay where the data is
	case dataInUse(name, rec.Backend):
		return s, fmt.Errorf("%s runs on %s (backend: auto picked it: %s), which cannot serve its Boxfile any more, "+
			"and its /data is there. To move it, set the Boxfile back, then:\n"+
			"  box data export %s ./%s-data && box reset %s -y\n"+
			"change the Boxfile again, and:\n"+
			"  box build %s && box data import %s ./%s-data",
			name, rec.Backend, rec.Reason, name, name, name, name, name, name)
	default:
		if err := writeAutoChoice(name, AutoChoice{Backend: pick, Reason: why}); err != nil {
			return s, err
		}
		s.Backend = pick
	}
	return s, nil
}
