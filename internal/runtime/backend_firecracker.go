package runtime

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"box/internal/agent"
	"box/internal/boxfile"
	"box/internal/sandbox"
)

// fcBackend boots sandboxes with the Firecracker VMM. Each image is booted
// once and snapshotted with its agent ready (fc_image.go); every run
// restores that snapshot, which costs tens of milliseconds instead of a
// kernel boot. The VMM runs as the only process of a crun container with no
// capabilities, its own pid, mount, network and IPC namespaces, no new
// privileges and podman's default seccomp filter, under which Firecracker
// installs its own per-thread filters -- the confinement Firecracker's jailer
// gives, without the root the jailer needs. The agent is reached over vsock,
// whose host side is a Unix socket in the VM's owner-only directory, so it
// needs no network at all. /data is an ext4 disk per sandbox; `box data
// export` and `import` move files in and out of it.
type fcBackend struct{}

func init() { register(fcBackend{}) }

func (fcBackend) Name() string { return "firecracker" }

func (fcBackend) Preflight() error {
	if goruntime.GOOS != "linux" || goruntime.GOARCH != "amd64" {
		return fmt.Errorf("the firecracker backend needs an x86_64 Linux host")
	}
	for _, bin := range []string{"podman", "crun"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("the firecracker backend needs %s on PATH", bin)
		}
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		return fmt.Errorf("/dev/kvm is not usable: %v", err)
	} else {
		f.Close()
	}
	if _, err := os.Stat("/dev/vhost-vsock"); err != nil {
		return fmt.Errorf("/dev/vhost-vsock missing: load the vhost_vsock module (sudo modprobe vhost_vsock)")
	}
	return nil
}

// fcRun is what one `box __fc` invocation does, written to the VM
// directory as run.json.
type fcRun struct {
	Mode      string            `json:"mode"` // snapshot | run | detach
	Sandbox   string            `json:"sandbox"`
	VM        string            `json:"vm"`
	Image     string            `json:"image"`
	Disk      string            `json:"disk"`
	CPUs      int               `json:"cpus"`
	RAMMiB    int               `json:"ram_mib"`
	Strict    bool              `json:"strict"`
	ReadOnly  bool              `json:"readonly"`
	Argv      []string          `json:"argv,omitempty"`
	Stdin     bool              `json:"stdin,omitempty"`
	BootToken string            `json:"boot_token,omitempty"` // snapshot mode
	Token     string            `json:"token,omitempty"`      // detach mode: rotate to this
	Timeout   int               `json:"timeout,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Spec      boxfile.Spec      `json:"spec"`
}

func (b fcBackend) Launch(name string, s boxfile.Spec, l Launch) (*exec.Cmd, error) {
	if l.Baseline {
		// The isolation check's reference is a plain container of the
		// image, which podman provides.
		return podmanBackend{}.Launch(name, s, l)
	}
	if sandbox.IsFork(name) {
		return nil, fmt.Errorf("forks are not supported on the firecracker backend yet")
	}
	img, err := fcImage(name, s)
	if err != nil {
		return nil, err
	}
	disk, err := fcDataDisk(name)
	if err != nil {
		return nil, err
	}
	vm := l.VM
	if vm == "" {
		vm = fmt.Sprintf("box-%s-%d-%d", name, os.Getpid(), time.Now().UnixNano()%100000)
	}
	r := fcRun{
		Mode: "run", Sandbox: name, VM: vm, Image: img, Disk: disk,
		CPUs: s.CPUs, RAMMiB: s.RAMMiB, Strict: s.Isolation == "strict",
		ReadOnly: s.ReadOnlyRootfs, Argv: l.Argv, Stdin: l.Interactive,
		Labels: l.Labels, Spec: s,
	}
	if l.Detach {
		r.Mode, r.Token, r.Argv = "detach", l.Token, nil
	}
	return fcLaunchCmd(r)
}

// fcLaunchCmd writes run.json into a fresh VM directory and returns the
// supervisor command, which runs inside podman's namespace.
func fcLaunchCmd(r fcRun) (*exec.Cmd, error) {
	dir, err := vmDir(r.VM)
	if err != nil {
		return nil, err
	}
	fcBackend{}.Remove(r.VM)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), b, 0o600); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return inPodmanNS(self, "__fc", dir), nil
}

func (fcBackend) AgentAddr(vm string) (string, error) {
	dir, err := vmDir(vm)
	if err != nil {
		return "", err
	}
	return "unix:" + filepath.Join(dir, "v.sock"), nil
}

func (fcBackend) Logs(vm string) string {
	dir, err := vmDir(vm)
	if err != nil {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	return strings.TrimSpace(string(b))
}

func (fcBackend) Remove(vm string) {
	dir, err := vmDir(vm)
	if err != nil {
		return
	}
	var r fcRun
	if b, err := os.ReadFile(filepath.Join(dir, "run.json")); err == nil {
		json.Unmarshal(b, &r)
	}
	fcFlush(dir)
	root := fcCrunRoot()
	inPodmanNS("sh", "-c", `crun --root "$1" kill "$2" KILL 2>/dev/null; crun --root "$1" delete -f "$2" 2>/dev/null; rm -rf -- "$3"`,
		"sh", root, vm, dir).Run()
	if r.Sandbox != "" {
		releaseDisk(r.Sandbox, vm)
	}
}

func (b fcBackend) RemoveLabelled(label, value string) {
	vms, err := sandbox.VMsDir()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(vms)
	for _, e := range entries {
		var r fcRun
		if bb, err := os.ReadFile(filepath.Join(vms, e.Name(), "run.json")); err == nil &&
			json.Unmarshal(bb, &r) == nil && r.Labels[label] == value {
			b.Remove(e.Name())
		}
	}
}

func fcCrunRoot() string {
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		run = os.TempDir()
	}
	return filepath.Join(run, "box", "fc")
}

// /data is a block device here, and an ext4 filesystem mounted by two VMs at
// once is corrupted. A lock file names the one VM that may mount it; a lock
// whose VM is gone is taken over.
func diskLockPath(name string) string {
	h, _ := sandbox.Home()
	return filepath.Join(h, "disks", name+".lock")
}

func acquireDisk(name, vm string) error {
	p := diskLockPath(name)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.WriteString(vm)
			return f.Close()
		}
		holder, _ := os.ReadFile(p)
		if fcAlive(string(holder)) {
			return fmt.Errorf("%s's /data disk is in use by %s; on the firecracker backend one VM at a time mounts it (exec into the running one, or bring it down)",
				name, strings.TrimSpace(string(holder)))
		}
		os.Remove(p) // stale: that VM is gone
	}
	return fmt.Errorf("could not lock %s's /data disk", name)
}

func releaseDisk(name, vm string) {
	p := diskLockPath(name)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == vm {
		os.Remove(p)
	}
}

// fcAlive reports whether a VM's Firecracker process is still running.
func fcAlive(vm string) bool {
	dir, err := vmDir(strings.TrimSpace(vm))
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && unix.Kill(pid, 0) == nil
}

// RunFirecracker is `box __fc <vm dir>`, run inside podman's namespace.
func RunFirecracker(dir string) error {
	if os.Getenv("_CONTAINERS_USERNS_CONFIGURED") == "" {
		return errors.New("__fc runs inside podman's user namespace")
	}
	var r fcRun
	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	// crun's child enters the VM's user namespace, where our working
	// directory may be unreadable; start from /.
	os.Chdir("/")
	if r.Mode != "snapshot" {
		if err := acquireDisk(r.Sandbox, r.VM); err != nil {
			return err
		}
	}
	fail := func(err error) error {
		fcBackend{}.Remove(r.VM)
		return err
	}
	if err := fcStart(dir, r); err != nil {
		return fail(err)
	}
	api := fcAPI(filepath.Join(dir, "api.sock"))

	switch r.Mode {
	case "snapshot":
		err := fcBootAndSnapshot(api, dir, r)
		fcBackend{}.Remove(r.VM)
		return err
	case "detach":
		if err := fcRestore(api, dir, r, r.Token); err != nil {
			return fail(err)
		}
		return nil
	default:
		token, err := newToken()
		if err != nil {
			return fail(err)
		}
		if err := fcRestore(api, dir, r, token); err != nil {
			return fail(err)
		}
		var stdin io.Reader
		if r.Stdin {
			stdin = os.Stdin
		}
		res, err := agent.Exec("unix:"+filepath.Join(dir, "v.sock"),
			agent.Request{Token: token, Argv: r.Argv, TimeoutSeconds: r.Spec.TimeoutSeconds},
			nil, stdin, os.Stdout, os.Stderr)
		fcBackend{}.Remove(r.VM)
		if err != nil {
			return err
		}
		if res.TimedOut {
			os.Exit(ExitTimeout)
		}
		os.Exit(res.Code)
	}
	return nil
}

// fcStart writes the container spec and starts Firecracker in it, waiting
// for its API socket.
func fcStart(dir string, r fcRun) error {
	fcBin, kernel, err := fcAssets()
	if err != nil {
		return err
	}
	jail, err := fcJailRoot()
	if err != nil {
		return err
	}
	if r.Strict {
		// The VMM runs as the subordinate UID (1 in this namespace). It owns
		// the /data disk; the VM directory stays yours, shared with it through
		// the group, so it can create its sockets there and nobody else can.
		if err := os.Lchown(r.Disk, 1, 1); err != nil {
			return err
		}
		if err := os.Lchown(dir, 0, 1); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o770); err != nil {
			return err
		}
	}
	spec, err := fcSpec(r, dir, jail, filepath.Dir(fcBin), filepath.Base(fcBin), filepath.Dir(kernel))
	if err != nil {
		return err
	}
	// crun reopens its bundle directory from inside the VMM's namespaces;
	// under strict that is the subordinate UID, which must be able to enter
	// it. config.json inside stays owner-only, and holds no secret.
	bundle := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(bundle, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), spec, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(fcCrunRoot(), 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command("crun", "--root", fcCrunRoot(), "--systemd-cgroup", "run", "--detach",
		"--pid-file", filepath.Join(dir, "pid"), "-b", bundle, r.VM)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		return fmt.Errorf("starting firecracker: %v: %s", err, strings.TrimSpace(string(b)))
	}
	sock := filepath.Join(dir, "api.sock")
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return nil
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	return fmt.Errorf("firecracker never opened its API socket: %s", strings.TrimSpace(string(b)))
}

// fcJailRoot is the VMM container's root: an empty, read-only directory
// holding only the mountpoints its spec fills.
func fcJailRoot() (string, error) {
	dir, err := fcDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(dir, "jail")
	for _, d := range []string{"fc", "kernel", "img", "vm", "disk", "dev", "proc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return "", err
		}
	}
	f, err := os.OpenFile(filepath.Join(root, "disk", "data.ext4"), os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return "", err
	}
	f.Close()
	return root, nil
}

// fcSpec confines the VMM: nothing but its binary, kernel, image, VM
// directory, /data disk and /dev/kvm; no capabilities; no network; podman's
// default seccomp filter for a process with no capabilities.
func fcSpec(r fcRun, dir, jail, binDir, binName, kernelDir string) ([]byte, error) {
	ro := func(src, dst string) ociMount {
		return ociMount{Destination: dst, Type: "bind", Source: src, Options: []string{"rbind", "ro", "nosuid", "nodev"}}
	}
	rw := func(src, dst string) ociMount {
		return ociMount{Destination: dst, Type: "bind", Source: src, Options: []string{"rbind", "nosuid", "nodev"}}
	}
	mounts := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "mode=755", "size=64k"}},
		// A device node needs a mount without nodev.
		{Destination: "/dev/kvm", Type: "bind", Source: "/dev/kvm", Options: []string{"rbind", "nosuid"}},
		{Destination: "/dev/vhost-vsock", Type: "bind", Source: "/dev/vhost-vsock", Options: []string{"rbind", "nosuid"}},
		ro(binDir, "/fc"),
		ro(kernelDir, "/kernel"),
		ro(r.Image, "/img"),
		rw(dir, "/vm"),
		rw(r.Disk, "/disk/data.ext4"),
	}
	if r.Mode == "snapshot" {
		mounts[len(mounts)-3] = ociMount{Destination: "/img", Type: "bind", Source: r.Image, Options: []string{"rbind", "ro", "nosuid", "nodev"}}
	}
	seccomp, err := seccompProfileFor(r.Spec, nil)
	if err != nil {
		return nil, err
	}
	spec := ociSpec{
		Version: "1.0.2",
		Process: ociProcess{
			User: ociUser{},
			Args: []string{"/fc/" + binName, "--api-sock", "/vm/api.sock", "--id", "box", "--level", "Warning"},
			Env:  []string{"PATH=/fc"},
			Cwd:  "/vm",
			Capabilities: ociCapabilities{
				Bounding: []string{}, Effective: []string{}, Permitted: []string{},
			},
			NoNewPrivileges: true,
			Rlimits:         []ociRlimit{{Type: "RLIMIT_NOFILE", Hard: 4096, Soft: 4096}},
		},
		Root:   ociRoot{Path: jail, Readonly: true},
		Mounts: mounts,
		Linux: ociLinux{
			Resources: &ociResources{
				Pids:   &ociPids{Limit: vmmPidsLimit},
				Memory: &ociMemory{Limit: int64(r.RAMMiB+vmmOverheadMiB) << 20},
			},
			CgroupsPath: cgroupsPath(r.VM),
			Namespaces: []ociNamespace{
				{Type: "pid"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"}, {Type: "cgroup"}, {Type: "network"},
			},
			MaskedPaths:   ociMaskedPaths,
			ReadonlyPaths: ociReadonlyPaths,
			Seccomp:       seccomp,
		},
	}
	if r.Strict {
		// The VMM runs as UID 1 of podman's namespace -- your first
		// subordinate UID on the host -- rather than in a nested user
		// namespace: crun sets the mounts up as that namespace's root, where
		// every path is reachable whatever your home directory's mode, and
		// only then drops to the VMM's identity, with no capabilities.
		spec.Process.User = ociUser{UID: 1, GID: 1}
	}
	return json.MarshalIndent(spec, "", " ")
}

// fcAPI is a client for Firecracker's API socket.
type fcClient struct{ c *http.Client }

func fcAPI(sock string) fcClient {
	return fcClient{&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

func (f fcClient) call(method, path string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, "http://fc"+path, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.c.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func fcBootAndSnapshot(api fcClient, dir string, r fcRun) error {
	ro := "0"
	if r.ReadOnly {
		ro = "1"
	}
	_, kernel, err := fcAssets()
	if err != nil {
		return err
	}
	args := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ro root=/dev/vda rootfstype=ext4 quiet loglevel=1 "+
		"init=/.box/box box.token=%s box.hostname=%s box.ro=%s", r.BootToken, r.Sandbox, ro)
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/boot-source", map[string]any{"kernel_image_path": "/kernel/" + filepath.Base(kernel), "boot_args": args}},
		{"PUT", "/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": "/img/rootfs.ext4", "is_root_device": true, "is_read_only": true}},
		{"PUT", "/drives/data", map[string]any{"drive_id": "data", "path_on_host": "/disk/data.ext4", "is_root_device": false, "is_read_only": false}},
		{"PUT", "/machine-config", map[string]any{"vcpu_count": r.CPUs, "mem_size_mib": r.RAMMiB}},
		{"PUT", "/vsock", map[string]any{"guest_cid": 3, "uds_path": "/vm/v.sock"}},
		{"PUT", "/actions", map[string]any{"action_type": "InstanceStart"}},
	}
	for _, s := range steps {
		if err := api.call(s.method, s.path, s.body); err != nil {
			return err
		}
	}
	addr := "unix:" + filepath.Join(dir, "v.sock")
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := agent.Exec(addr, agent.Request{Token: r.BootToken}, nil, nil, io.Discard, io.Discard)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, "log"))
			return fmt.Errorf("the guest agent never answered: %v\n%s", err, strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := api.call("PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		return err
	}
	if err := api.call("PUT", "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": "/vm/snap.state", "mem_file_path": "/vm/snap.mem",
	}); err != nil {
		return err
	}
	// The snapshot lands in the VM directory; move it beside the root disk,
	// mem last, since its presence marks the image complete.
	for _, f := range []string{"snap.state", "snap.mem"} {
		if err := os.Rename(filepath.Join(dir, f), filepath.Join(r.Image, f)); err != nil {
			return err
		}
	}
	return nil
}

// fcRestore loads the image's snapshot into the running VMM, then makes the
// copy distinct: the snapshot's token is rotated to token, the guest mixes in
// fresh randomness and takes the host's clock, and /data is mounted.
func fcRestore(api fcClient, dir string, r fcRun, token string) error {
	if err := api.call("PUT", "/snapshot/load", map[string]any{
		"snapshot_path": "/img/snap.state",
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": "/img/snap.mem"},
		"resume_vm":     true,
	}); err != nil {
		return err
	}
	boot, err := os.ReadFile(filepath.Join(r.Image, "token"))
	if err != nil {
		return err
	}
	seed := make([]byte, 64)
	if _, err := io.ReadFull(randReader(), seed); err != nil {
		return err
	}
	var errb strings.Builder
	res, err := agent.Exec("unix:"+filepath.Join(dir, "v.sock"), agent.Request{
		Token:    strings.TrimSpace(string(boot)),
		NewToken: token,
		Argv: []string{"/.box/box", "__prepare", "--data",
			hex.EncodeToString(seed), strconv.FormatInt(time.Now().UnixNano(), 10)},
		TimeoutSeconds: 30,
	}, nil, nil, io.Discard, &errb)
	if err != nil {
		return fmt.Errorf("preparing the restored VM: %w", err)
	}
	if res.Code != 0 {
		return fmt.Errorf("preparing the restored VM: exit %d: %s", res.Code, strings.TrimSpace(errb.String()))
	}
	// The VMM created the sockets as itself; under strict that is the
	// subordinate UID. Hand them to you, owner and group only, so the CLI
	// can connect and other users cannot.
	for _, s := range []string{"v.sock", "api.sock"} {
		p := filepath.Join(dir, s)
		os.Lchown(p, 0, 1)
		os.Chmod(p, 0o660)
	}
	// Kept (owner-only) so teardown can reach the agent to flush /data,
	// whoever tears the VM down.
	return os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600)
}

// fcFlush asks a VM to write /data out and unmount it, so killing the VMM
// next loses nothing the guest still held in its page cache. Best effort,
// bounded: a guest that does not answer is killed regardless.
func fcFlush(dir string) {
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		agent.Exec("unix:"+filepath.Join(dir, "v.sock"), agent.Request{
			Token: string(token), Argv: []string{"/bin/sh", "-c", "sync; umount /data 2>/dev/null; sync"}, TimeoutSeconds: 10,
		}, nil, nil, io.Discard, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func randReader() io.Reader { return crand.Reader }
