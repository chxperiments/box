package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"box/internal/boxfile"
)

// Backend turns a built image into a running microVM. Images are always
// built by podman; what differs between backends is how a VM is launched and
// torn down. Everything above this -- the warm pool, up/exec, forks, the
// isolation checks, what the VMM may do on the host -- is shared, so a
// backend is only as much code as the launch itself.
//
// Which backend a sandbox uses is the Boxfile's `backend:`; BOX_BACKEND
// overrides it for experiments.
type Backend interface {
	// Name is the Boxfile value that selects this backend.
	Name() string
	// Preflight fails loudly when the host cannot run this backend.
	Preflight() error
	// Launch returns the command that boots a VM. It is not started.
	Launch(name string, s boxfile.Spec, l Launch) (*exec.Cmd, error)
	// AgentAddr is host:port where a detached VM's agent can be reached.
	AgentAddr(vm string) (string, error)
	// Logs returns what a VM printed, for diagnosing a boot that failed.
	Logs(vm string) string
	// Remove tears a VM down. A VM that is already gone is not an error.
	Remove(vm string)
	// RemoveLabelled tears down every VM carrying label=value: the pool's
	// way of catching VMs whose bookkeeping was lost.
	RemoveLabelled(label, value string)
}

// Launch is everything one boot can vary.
type Launch struct {
	VM          string            // the VM's name, for Remove and AgentAddr
	Argv        []string          // what to run; nil means the image's default
	Detach      bool              // return once the VM is running (up, the pool)
	Interactive bool              // forward stdin; a TTY too if there is one
	Agent       bool              // run the guest agent as the main process and publish its port
	AgentDir    string            // host directory holding the agent binary
	TokenEnv    string            // environment variable carrying the agent token
	Token       string            // its value; backends that write a spec file put it there (owner-only)
	Labels      map[string]string // attached to the VM, for RemoveLabelled
	Baseline    bool              // a plain container instead of a microVM: the isolation check's reference
}

// backends is every backend this build knows, by Boxfile name.
var backends = map[string]Backend{}

func register(b Backend) { backends[b.Name()] = b }

// backendFor picks the sandbox's backend. The environment override is for
// trying another backend on an existing sandbox without editing it.
func backendFor(s boxfile.Spec) (Backend, error) {
	name := s.Backend
	if env := os.Getenv("BOX_BACKEND"); env != "" {
		name = env
	}
	if name == "" {
		name = boxfile.Default.Backend
	}
	b, ok := backends[name]
	if !ok {
		have := make([]string, 0, len(backends))
		for n := range backends {
			have = append(have, n)
		}
		sort.Strings(have)
		return nil, fmt.Errorf("backend %q is not available in this build (have: %s)", name, strings.Join(have, ", "))
	}
	return b, nil
}

// backendOf is backendFor by sandbox name, for paths that have no spec in
// hand (Down, the pool's cleanup). A sandbox whose Boxfile no longer parses
// gets the default backend, which is the one most likely to own its VMs.
func backendOf(name string) Backend {
	if s, err := parseSpec(name); err == nil {
		if b, err := backendFor(s); err == nil {
			return b
		}
	}
	return backends[boxfile.Default.Backend]
}

// Preflight checks the default backend's host requirements.
func Preflight() error { return backends[boxfile.Default.Backend].Preflight() }

// PreflightFor checks the host can run a sandbox's own backend.
func PreflightFor(s boxfile.Spec) error {
	b, err := backendFor(s)
	if err != nil {
		return err
	}
	return b.Preflight()
}
