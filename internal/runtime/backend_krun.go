package runtime

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"box/internal/agent"
	"box/internal/boxfile"
	"box/internal/sandbox"
)

// krunBackend boots microVMs by driving crun's libkrun handler directly,
// without podman in the path: podman's share of a cold boot is about 850ms
// of a 1.4s total. Images are still built by podman and exported once
// (rootfs.go); each VM overlays that export, gets an OCI spec with the same
// confinement the podman backend asks podman for, and is launched by
// `krun run` from inside podman's user namespace, where the overlay is
// mounted and the subordinate UID range is addressable. The network
// namespace starts empty; a createRuntime hook attaches pasta to it, with
// the host's gateway address unmapped so the guest cannot reach host
// loopback, and the agent's port forwarded from 127.0.0.1 only.
//
// Per VM, under ~/.box/vms/<vm>/:
//
//	config.json   the OCI spec (owner-only: it carries the agent token)
//	meta.json     sandbox, labels, agent port
//	upper, work   the VM's writes to its rootfs
//	merge         the rootfs it boots from
//	etc/          resolv.conf, hosts, hostname
//	log           stdout and stderr of a detached VM
type krunBackend struct{}

func init() { register(krunBackend{}) }

func (krunBackend) Name() string { return "krun" }

func (krunBackend) Preflight() error {
	if goruntime.GOOS != "linux" {
		return fmt.Errorf("the krun backend needs a Linux host; use backend: podman")
	}
	for _, bin := range []string{"podman", "krun", "crun", "pasta"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("the krun backend needs %s on PATH", bin)
		}
	}
	if out, err := exec.Command("krun", "--version").Output(); err != nil || !strings.Contains(string(out), "+LIBKRUN") {
		return fmt.Errorf("krun is a crun without libkrun support (no +LIBKRUN in --version)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not available -- microVMs need KVM on this host")
	}
	return nil
}

// vmMeta is the bookkeeping a VM directory carries for AgentAddr and
// RemoveLabelled.
type vmMeta struct {
	Sandbox string            `json:"sandbox"`
	Labels  map[string]string `json:"labels,omitempty"`
	Port    int               `json:"port,omitempty"`
	Started string            `json:"started"`
	Rootfs  string            `json:"rootfs"`
}

func vmDir(vm string) (string, error) {
	if err := sandbox.ValidLabel(vm); err != nil {
		return "", err
	}
	d, err := sandbox.VMsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, vm), nil
}

// krunRoot is crun's state directory for box's VMs, apart from podman's.
func krunRoot() string {
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		run = os.TempDir()
	}
	return filepath.Join(run, "box", "krun")
}

func (b krunBackend) Launch(name string, s boxfile.Spec, l Launch) (*exec.Cmd, error) {
	if s.Isolation == "strict" {
		// libkrun fails to start inside a second, nested user namespace here
		// (`readlink: No such file or directory`). Refuse rather than fall
		// back to running the VMM as you.
		return nil, fmt.Errorf("backend: krun does not support isolation: strict yet; use backend: podman for strict sandboxes")
	}
	rootfs, img, err := rootfsFor(name, s)
	if err != nil {
		return nil, err
	}
	if err := checkMountSources(name, s); err != nil {
		return nil, err
	}
	vm := l.VM
	if vm == "" {
		vm = fmt.Sprintf("box-%s-%d-%d", name, os.Getpid(), time.Now().UnixNano()%1000)
	}
	dir, err := vmDir(vm)
	if err != nil {
		return nil, err
	}
	b.Remove(vm)
	for _, d := range []string{"upper", "work", "merge", "etc"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	// Readable by the VM's own user namespace, which does not map this user:
	// crun re-opens the bundle directory from inside it, and the VM reaches
	// merge/ and etc/ beneath. config.json, which carries the agent token,
	// stays 0600, so only file names are visible from there.
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	// The merge directory is traversed from inside the VM's own user
	// namespace, which does not map this user: it needs the "other" bits.
	for _, d := range []string{"merge", "etc"} {
		if err := os.Chmod(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}

	port := 0
	if l.Agent {
		if port, err = freePort(); err != nil {
			return nil, err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	hook := ""
	if s.Network != "none" {
		hook = self
	}
	for f, content := range map[string]string{
		// pasta forwards DNS sent to this address to the host's resolver.
		"resolv.conf": "nameserver 169.254.1.1\n",
		"hosts":       "127.0.0.1 localhost\n::1 localhost\n127.0.1.1 " + vm + "\n",
		"hostname":    vm + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "etc", f), []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	interactive := false
	if l.Interactive {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			interactive = true
		}
	}
	spec, err := krunSpec(name, s, specInput{
		Rootfs:      filepath.Join(dir, "merge"),
		Image:       img,
		Launch:      Launch{VM: vm, Argv: l.Argv, Agent: l.Agent, AgentDir: l.AgentDir, TokenEnv: l.TokenEnv, Token: l.Token},
		Hostname:    vm,
		EtcDir:      filepath.Join(dir, "etc"),
		HookBinary:  hook,
		HookLog:     filepath.Join(dir, "log"),
		AgentPort:   port,
		Interactive: interactive,
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), spec, 0o600); err != nil {
		return nil, err
	}
	meta, _ := json.Marshal(vmMeta{Sandbox: name, Labels: l.Labels, Port: port,
		Started: time.Now().UTC().Format(time.RFC3339), Rootfs: rootfs})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		return nil, err
	}

	// `box __krun` mounts the overlays inside podman's namespace, runs
	// krun (or crun, for the baseline), and cleans up a foreground VM.
	args := []string{self, "__krun", "--dir", dir, "--rootfs", rootfs}
	if l.Detach {
		args = append(args, "--detach")
	}
	if l.Baseline {
		args = append(args, "--baseline")
	}
	if sandbox.IsFork(name) {
		args = append(args, "--fork", name)
	}
	if l.Interactive {
		args = append(args, "--interactive")
	}
	return inPodmanNS(args...), nil
}

// freePort asks the kernel for an unused loopback port. Nothing holds it
// between here and pasta binding it, so a clash is possible but rare, and
// a clash fails the boot loudly rather than quietly.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (krunBackend) AgentAddr(vm string) (string, error) {
	m, err := readMeta(vm)
	if err != nil {
		return "", err
	}
	if m.Port == 0 {
		return "", fmt.Errorf("%s has no agent port", vm)
	}
	return "127.0.0.1:" + strconv.Itoa(m.Port), nil
}

func readMeta(vm string) (vmMeta, error) {
	dir, err := vmDir(vm)
	if err != nil {
		return vmMeta{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return vmMeta{}, err
	}
	var m vmMeta
	return m, json.Unmarshal(b, &m)
}

func (krunBackend) Logs(vm string) string {
	dir, err := vmDir(vm)
	if err != nil {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	return strings.TrimSpace(string(b))
}

// Remove kills the VM, unmounts its rootfs overlay and deletes its directory.
func (krunBackend) Remove(vm string) {
	dir, err := vmDir(vm)
	if err != nil {
		return
	}
	if _, err := os.Stat(dir); err != nil {
		// No directory, but crun may still know the name from a crash.
		exec.Command("krun", "--root", krunRoot(), "delete", "-f", vm).Run()
		return
	}
	exec.Command("krun", "--root", krunRoot(), "kill", vm, "KILL").Run()
	exec.Command("krun", "--root", krunRoot(), "delete", "-f", vm).Run()
	inPodmanNS("sh", "-c", "umount \"$1\" 2>/dev/null; rm -rf -- \"$2\"", "sh", filepath.Join(dir, "merge"), dir).Run()
}

func (b krunBackend) RemoveLabelled(label, value string) {
	vms, err := sandbox.VMsDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(vms)
	if err != nil {
		return
	}
	for _, e := range entries {
		if m, err := readMeta(e.Name()); err == nil && m.Labels[label] == value {
			b.Remove(e.Name())
		}
	}
}

// RunKrun is the `box __krun` command, run inside podman unshare: it
// mounts the VM's rootfs overlay (and a fork's /data overlay), then runs
// krun -- or crun for the isolation baseline -- on the VM's bundle. A
// foreground VM is cleaned up when it exits; a detached one by Remove.
func RunKrun(dir, rootfs, fork string, detach, baseline, interactive bool) error {
	if os.Getenv("_CONTAINERS_USERNS_CONFIGURED") == "" {
		return fmt.Errorf("__krun runs inside podman unshare")
	}
	vm := filepath.Base(dir)
	t0 := time.Now()
	trace := func(stage string) {
		if os.Getenv("BOX_TRACE") != "" {
			fmt.Fprintf(os.Stderr, "trace: %-8s %6.0fms\n", stage, float64(time.Since(t0).Microseconds())/1000)
		}
	}
	if err := mountOverlay(rootfs, filepath.Join(dir, "upper"), filepath.Join(dir, "work"), filepath.Join(dir, "merge")); err != nil {
		return err
	}
	trace("overlay")
	if fork != "" {
		if err := MountOverlay(fork); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(krunRoot(), 0o700); err != nil {
		return err
	}
	runtime := "krun"
	if baseline {
		runtime = "crun"
	}
	// The runtime's child enters the VM's user namespace, where this
	// process's working directory may be unreadable (strict: owned by an
	// unmapped UID). podman chdirs to / before the runtime; so does this.
	if err := os.Chdir("/"); err != nil {
		return err
	}
	args := []string{"--root", krunRoot(), "--systemd-cgroup", "run", "-b", dir}
	if detach {
		args = append(args, "--detach")
	}
	args = append(args, vm)
	cmd := exec.Command(runtime, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if detach {
		// A detached VM's output goes to its log, for Logs.
		log, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer log.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, log, log
	}
	err := cmd.Run()
	trace("vm")
	if !detach {
		exec.Command(runtime, "--root", krunRoot(), "delete", "-f", vm).Run()
		exec.Command("umount", filepath.Join(dir, "merge")).Run()
		os.RemoveAll(dir)
		trace("cleanup")
	}
	if err != nil {
		if code := ExitCode(err); code >= 0 {
			os.Exit(code) // the VM's own status, as `podman run` would give
		}
		return err
	}
	return nil
}

// AttachNetwork is the `box __netns` createRuntime hook. crun hands the
// container state on stdin; pasta joins the VM's user and network namespaces
// by PID and gives the network namespace a route out. --no-map-gw keeps the
// gateway address from reaching the host's own loopback, and the only inbound
// port is the agent's, bound to 127.0.0.1 on the host.
func AttachNetwork(forward int, logPath string) (err error) {
	if logPath != "" {
		// crun shows only a hook's exit code; the reason goes to the VM's log.
		defer func() {
			if err != nil {
				if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
					fmt.Fprintf(f, "network hook: %v\n", err)
					f.Close()
				}
			}
		}()
	}
	var state struct {
		Pid int `json:"pid"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&state); err != nil {
		return fmt.Errorf("reading container state: %w", err)
	}
	if state.Pid == 0 {
		return fmt.Errorf("container state has no pid")
	}
	pid := strconv.Itoa(state.Pid)
	args := []string{"--config-net", "--dns-forward", "169.254.1.1", "--no-map-gw", "--quiet",
		"--netns", "/proc/" + pid + "/ns/net"}
	// Under strict isolation the VM has its own user namespace, which owns
	// its network namespace, so pasta must join it to configure anything.
	// Under standard the two share ours, and joining one's own namespace
	// is refused.
	if mine, _ := os.Readlink("/proc/self/ns/user"); mine != "" {
		if theirs, _ := os.Readlink("/proc/" + pid + "/ns/user"); theirs != "" && theirs != mine {
			args = append(args, "--userns", "/proc/"+pid+"/ns/user")
		}
	}
	if forward > 0 {
		args = append(args, "-t", fmt.Sprintf("127.0.0.1/%d:%d", forward, agent.Port))
	}
	t0 := time.Now()
	out, err := exec.Command("pasta", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pasta: %s", strings.TrimSpace(string(out)))
	}
	if os.Getenv("BOX_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "trace: pasta    %6.0fms\n", float64(time.Since(t0).Microseconds())/1000)
	}
	return nil
}
