package runtime

import (
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"

	"box/internal/agent"
	"box/internal/boxfile"
	"box/internal/sandbox"
)

// podmanBackend boots microVMs with `podman run --runtime krun`. It is the
// default: podman supplies the rootless user namespace, the network
// namespace with pasta, image storage and cleanup, and crun applies the
// confinement flags. The price is about 850ms of podman per boot, which the
// krun backend removes by driving crun itself.
type podmanBackend struct{}

func init() { register(podmanBackend{}) }

func (podmanBackend) Name() string { return "podman" }

// Preflight fails loudly rather than letting podman silently fall back to a
// plain container, which would look identical but share a kernel.
func (podmanBackend) Preflight() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman not found on PATH")
	}
	if goruntime.GOOS == "darwin" {
		// On macOS every container already runs inside the podman machine VM,
		// so KVM and the krun runtime live in there, not out here. All we can
		// check from this side is that the machine is up; whether it can
		// actually nest a microVM is settled by Verify.
		out, err := exec.Command("podman", "machine", "list", "--format", "{{.Running}}").Output()
		if err != nil || !strings.Contains(string(out), "true") {
			return fmt.Errorf("no podman machine is running:\n" +
				"  podman machine init && podman machine start")
		}
		return nil
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not available -- microVMs need KVM on this host")
	}
	if _, err := exec.LookPath("krun"); err != nil {
		return fmt.Errorf("no 'krun' on PATH. It is a symlink to crun:\n" +
			"  sudo ln -sf $(command -v crun) /usr/local/bin/krun\n" +
			"and libkrun must be installed (Fedora: sudo dnf install libkrun)")
	}
	return nil
}

func (podmanBackend) Launch(name string, s boxfile.Spec, l Launch) (*exec.Cmd, error) {
	args, err := vmArgs(name, s, l.Interactive, !l.Baseline)
	if err != nil {
		return nil, err
	}
	if l.Detach {
		args = append(args, "-d")
	}
	if l.VM != "" {
		args = append(args, "--name", l.VM)
	}
	for k, v := range l.Labels {
		args = append(args, "--label", k+"="+v)
	}
	if l.Agent {
		args = append(args,
			"-p", fmt.Sprintf("127.0.0.1::%d", agent.Port),
			"-v", l.AgentDir+":"+agentMount+":ro",
			// Name only: podman copies the value from its own environment, so
			// the token never appears in an argv that any host user can read
			// from ps.
			"-e", l.TokenEnv,
			"--entrypoint", agentMount+"/box",
		)
	}
	args = append(args, sandbox.ImageTag(name))
	if l.Agent {
		args = append(args, "__agent")
	} else {
		args = append(args, l.Argv...)
	}
	return podmanCmd(name, args...), nil
}

func (podmanBackend) AgentAddr(vm string) (string, error) {
	out, err := exec.Command("podman", "port", vm, fmt.Sprintf("%d/tcp", agent.Port)).Output()
	if err != nil {
		return "", fmt.Errorf("could not find the agent's port: %w", err)
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

func (podmanBackend) Logs(vm string) string {
	out, _ := exec.Command("podman", "logs", vm).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func (podmanBackend) Remove(vm string) {
	exec.Command("podman", "rm", "-f", "-t", "0", vm).Run()
}

func (b podmanBackend) RemoveLabelled(label, value string) {
	out, err := exec.Command("podman", "ps", "-aq", "--filter", "label="+label+"="+value).Output()
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		b.Remove(id)
	}
}

// podmanCmd is how every podman-backed VM is launched. For a fork, podman
// runs inside podman's own user namespace, after `box __overlay` has
// mounted the fork's overlay there: a fork's /data exists only in that
// namespace, and a VM launched from outside it would see an empty directory.
func podmanCmd(name string, args ...string) *exec.Cmd {
	if !sandbox.IsFork(name) {
		return exec.Command("podman", args...)
	}
	self, err := os.Executable()
	if err != nil {
		self = "box"
	}
	return inPodmanNS(append([]string{self, "__overlay", name, "--", "podman"}, args...)...)
}
