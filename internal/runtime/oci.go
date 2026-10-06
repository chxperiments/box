package runtime

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// The subset of the OCI runtime spec box writes. Field names follow the
// spec so the JSON is what crun expects; anything box never sets is left
// out rather than modelled.

type ociSpec struct {
	Version     string            `json:"ociVersion"`
	Process     ociProcess        `json:"process"`
	Root        ociRoot           `json:"root"`
	Hostname    string            `json:"hostname,omitempty"`
	Mounts      []ociMount        `json:"mounts"`
	Hooks       *ociHooks         `json:"hooks,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Linux       ociLinux          `json:"linux"`
}

type ociProcess struct {
	Terminal        bool            `json:"terminal"`
	User            ociUser         `json:"user"`
	Args            []string        `json:"args"`
	Env             []string        `json:"env,omitempty"`
	Cwd             string          `json:"cwd"`
	Capabilities    ociCapabilities `json:"capabilities"`
	NoNewPrivileges bool            `json:"noNewPrivileges"`
	Rlimits         []ociRlimit     `json:"rlimits,omitempty"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ociCapabilities struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Permitted   []string `json:"permitted"`
	Ambient     []string `json:"ambient,omitempty"`
	Inheritable []string `json:"inheritable,omitempty"`
}

type ociRlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type ociHooks struct {
	CreateRuntime []ociHook `json:"createRuntime,omitempty"`
}

type ociHook struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
}

type ociLinux struct {
	UIDMappings   []ociIDMapping  `json:"uidMappings,omitempty"`
	GIDMappings   []ociIDMapping  `json:"gidMappings,omitempty"`
	Resources     *ociResources   `json:"resources,omitempty"`
	CgroupsPath   string          `json:"cgroupsPath,omitempty"`
	Namespaces    []ociNamespace  `json:"namespaces"`
	Seccomp       json.RawMessage `json:"seccomp,omitempty"`
	MaskedPaths   []string        `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string        `json:"readonlyPaths,omitempty"`
}

type ociIDMapping struct {
	ContainerID uint32 `json:"containerID"`
	HostID      uint32 `json:"hostID"`
	Size        uint32 `json:"size"`
}

type ociResources struct {
	Pids   *ociPids   `json:"pids,omitempty"`
	Memory *ociMemory `json:"memory,omitempty"`
}

type ociPids struct {
	Limit int64 `json:"limit"`
}

type ociMemory struct {
	Limit int64 `json:"limit"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

// imageConfig is what podman applies from an image when it runs one, saved
// beside the exported rootfs so the krun backend applies the same.
type imageConfig struct {
	Env        []string `json:"env"`
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	WorkingDir string   `json:"workdir"`
	User       string   `json:"user"`
}

// cgroupsPath names the VM's systemd scope. A suffix unique to this boot
// keeps a scope left behind by a crashed VM of the same name from blocking
// the next one.
func cgroupsPath(vm string) string {
	b := make([]byte, 4)
	crand.Read(b)
	return "user.slice:box:" + vm + "-" + hex.EncodeToString(b)
}

// vmmCapList is vmmCaps as the OCI spec wants it.
var vmmCapList = func() []string {
	var out []string
	for _, c := range strings.Split(vmmCaps, ",") {
		out = append(out, "CAP_"+c)
	}
	return out
}()

// The paths crun's own `crun spec` masks and makes read-only, kept the same.
var (
	ociMaskedPaths = []string{"/proc/acpi", "/proc/asound", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
		"/sys/firmware", "/proc/scsi"}
	ociReadonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// specInput is what krunSpec needs beyond the Boxfile.
type specInput struct {
	Rootfs      string // the merged rootfs the VM boots from
	Image       imageConfig
	Launch      Launch
	Hostname    string
	EtcDir      string // holds resolv.conf, hosts, hostname for the guest
	HookBinary  string // box itself, run as the createRuntime hook
	HookLog     string // where the hook reports a failure
	AgentPort   int    // host port forwarded to the agent, 0 for none
	Interactive bool   // a terminal is attached
}

// krunSpec writes the OCI spec for one VM: the image's process as podman
// would run it, the Boxfile's mounts, and the same confinement the podman
// backend asks podman for, stated directly.
func krunSpec(name string, s boxfile.Spec, in specInput) ([]byte, error) {
	argv := in.Launch.Argv
	switch {
	case in.Launch.Agent:
		argv = []string{agentMount + "/box", "__agent"}
	case len(argv) == 0:
		argv = append(append([]string{}, in.Image.Entrypoint...), in.Image.Cmd...)
	case len(in.Image.Entrypoint) > 0:
		argv = append(append([]string{}, in.Image.Entrypoint...), argv...)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("image of %s has no entrypoint or command and none was given", name)
	}

	env := append([]string{}, in.Image.Env...)
	if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "PATH=") }) {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	for _, k := range s.EnvKeys() {
		env = append(env, k+"="+s.Env[k])
	}
	if in.Launch.Agent && in.Launch.TokenEnv != "" {
		// The token travels in this process's environment and lands in the
		// spec file, which is owner-only; the same exposure as run/<name>.json.
		env = append(env, in.Launch.TokenEnv+"="+in.Launch.Token)
	}

	user, err := parseUser(in.Image.User)
	if err != nil {
		return nil, err
	}
	cwd := in.Image.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	mounts := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
	}
	for _, f := range []string{"resolv.conf", "hosts", "hostname"} {
		mounts = append(mounts, bind(in.EtcDir+"/"+f, "/etc/"+f, true))
	}
	if s.ReadOnlyRootfs {
		// What podman's --read-only leaves writable.
		for _, d := range []string{"/tmp", "/run", "/var/tmp"} {
			mounts = append(mounts, ociMount{Destination: d, Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "nodev", "mode=1777"}})
		}
	}
	data, err := dataBind(name)
	if err != nil {
		return nil, err
	}
	mounts = append(mounts, data)
	for _, m := range s.Mounts {
		mounts = append(mounts, bind(m.Host, m.Guest, m.Mode != "rw"))
	}
	if in.Launch.Agent {
		mounts = append(mounts, bind(in.Launch.AgentDir, agentMount, true))
	}

	spec := ociSpec{
		Version: "1.0.2",
		Process: ociProcess{
			Terminal: in.Interactive,
			User:     user,
			Args:     argv,
			Env:      env,
			Cwd:      cwd,
			Capabilities: ociCapabilities{
				Bounding: vmmCapList, Effective: vmmCapList, Permitted: vmmCapList,
			},
			NoNewPrivileges: true,
			Rlimits:         []ociRlimit{{Type: "RLIMIT_NOFILE", Hard: 1048576, Soft: 1048576}},
		},
		Root:     ociRoot{Path: in.Rootfs, Readonly: s.ReadOnlyRootfs},
		Hostname: in.Hostname,
		Mounts:   mounts,
		Annotations: map[string]string{
			"krun.cpus":    strconv.Itoa(s.CPUs),
			"krun.ram_mib": strconv.Itoa(s.RAMMiB),
		},
		Linux: ociLinux{
			Resources: &ociResources{
				Pids:   &ociPids{Limit: vmmPidsLimit},
				Memory: &ociMemory{Limit: int64(s.RAMMiB+vmmOverheadMiB) << 20},
			},
			CgroupsPath: cgroupsPath(in.Launch.VM),
			Namespaces: []ociNamespace{
				{Type: "pid"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"}, {Type: "cgroup"}, {Type: "network"},
			},
			MaskedPaths:   ociMaskedPaths,
			ReadonlyPaths: ociReadonlyPaths,
		},
	}
	if s.Passt {
		spec.Annotations["krun.use_passt"] = "1"
	}
	if s.Isolation == "strict" {
		// Relative to podman's user namespace, where this spec is applied: IDs
		// 1..65536 are the subordinate range, 0 is you, left unmapped.
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, ociNamespace{Type: "user"})
		spec.Linux.UIDMappings = []ociIDMapping{{ContainerID: 0, HostID: 1, Size: 65536}}
		spec.Linux.GIDMappings = []ociIDMapping{{ContainerID: 0, HostID: 1, Size: 65536}}
	}
	if s.Network != "none" && in.HookBinary != "" {
		// The network namespace is created empty; the hook gives it a route
		// out through pasta before the VM's process starts.
		args := []string{in.HookBinary, "__netns", "--log", in.HookLog}
		if in.AgentPort > 0 {
			args = append(args, "--forward", strconv.Itoa(in.AgentPort))
		}
		spec.Hooks = &ociHooks{CreateRuntime: []ociHook{{
			Path: in.HookBinary, Args: args, Env: []string{"PATH=" + os.Getenv("PATH")},
		}}}
	}
	seccomp, err := seccompProfile(s)
	if err != nil {
		return nil, err
	}
	spec.Linux.Seccomp = seccomp
	return json.MarshalIndent(spec, "", " ")
}

func bind(src, dst string, ro bool) ociMount {
	opts := []string{"rbind", "nosuid", "nodev"}
	if ro {
		opts = append(opts, "ro")
	}
	return ociMount{Destination: dst, Type: "bind", Source: src, Options: opts}
}

// dataBind is the /data mount: the sandbox's data directory, or for a fork
// its overlay's mountpoint, exactly as the podman backend's -v.
func dataBind(name string) (ociMount, error) {
	src, err := sandbox.DataSource(name)
	if err != nil {
		return ociMount{}, err
	}
	return bind(src, "/data", false), nil
}

// parseUser reads an image's USER: a uid[:gid], or empty for root. Names
// would need the image's /etc/passwd; box's own images use numbers or
// nothing, and a name is refused rather than guessed.
func parseUser(u string) (ociUser, error) {
	if u == "" {
		return ociUser{}, nil
	}
	uidS, gidS, _ := strings.Cut(u, ":")
	uid, err := strconv.ParseUint(uidS, 10, 32)
	if err != nil {
		return ociUser{}, fmt.Errorf("image USER %q: the krun backend needs a numeric uid[:gid]", u)
	}
	gid := uid
	if gidS != "" {
		if gid, err = strconv.ParseUint(gidS, 10, 32); err != nil {
			return ociUser{}, fmt.Errorf("image USER %q: the krun backend needs a numeric uid[:gid]", u)
		}
	}
	return ociUser{UID: uint32(uid), GID: uint32(gid)}, nil
}

// seccompPaths is where podman's default profile lives.
var seccompPaths = []string{"/etc/containers/seccomp.json", "/usr/share/containers/seccomp.json"}

// containersSeccomp is the containers/common profile format: OCI's, plus
// per-rule includes/excludes that podman resolves against the container's
// capabilities and architecture before handing crun the result.
type containersSeccomp struct {
	DefaultAction   string `json:"defaultAction"`
	DefaultErrnoRet *uint  `json:"defaultErrnoRet,omitempty"`
	ArchMap         []struct {
		Architecture     string   `json:"architecture"`
		SubArchitectures []string `json:"subArchitectures"`
	} `json:"archMap"`
	Syscalls []struct {
		Names    []string        `json:"names"`
		Action   string          `json:"action"`
		ErrnoRet *uint           `json:"errnoRet,omitempty"`
		Args     json.RawMessage `json:"args,omitempty"`
		Includes seccompFilter   `json:"includes,omitempty"`
		Excludes seccompFilter   `json:"excludes,omitempty"`
	} `json:"syscalls"`
}

type seccompFilter struct {
	Caps   []string `json:"caps,omitempty"`
	Arches []string `json:"arches,omitempty"`
}

// seccompProfile resolves the profile the podman backend gets by default --
// or the Boxfile's own -- into the OCI form, for the VMM's six capabilities
// and this architecture. The result is the same filter podman would install.
func seccompProfile(s boxfile.Spec) (json.RawMessage, error) {
	return seccompProfileFor(s, vmmCapList)
}

// seccompProfileFor resolves the profile for a process holding caps.
func seccompProfileFor(s boxfile.Spec, caps []string) (json.RawMessage, error) {
	var path string
	if s.Seccomp != "" {
		path = s.Seccomp
	} else {
		for _, p := range seccompPaths {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return nil, fmt.Errorf("no seccomp profile found (looked in %s); install podman's containers-common", strings.Join(seccompPaths, ", "))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var in containersSeccomp
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("seccomp profile %s: %w", path, err)
	}

	arch, scmpArch := map[string][2]string{
		"amd64": {"amd64", "SCMP_ARCH_X86_64"},
		"arm64": {"arm64", "SCMP_ARCH_AARCH64"},
	}[goruntime.GOARCH][0], map[string][2]string{
		"amd64": {"amd64", "SCMP_ARCH_X86_64"},
		"arm64": {"arm64", "SCMP_ARCH_AARCH64"},
	}[goruntime.GOARCH][1]
	if arch == "" {
		return nil, fmt.Errorf("no seccomp architecture mapping for %s", goruntime.GOARCH)
	}
	have := func(c string) bool { return slices.Contains(caps, c) }

	type rule struct {
		Names    []string        `json:"names"`
		Action   string          `json:"action"`
		ErrnoRet *uint           `json:"errnoRet,omitempty"`
		Args     json.RawMessage `json:"args,omitempty"`
	}
	out := struct {
		DefaultAction   string   `json:"defaultAction"`
		DefaultErrnoRet *uint    `json:"defaultErrnoRet,omitempty"`
		Architectures   []string `json:"architectures"`
		Syscalls        []rule   `json:"syscalls"`
	}{DefaultAction: in.DefaultAction, DefaultErrnoRet: in.DefaultErrnoRet, Architectures: []string{scmpArch}}
	for _, am := range in.ArchMap {
		if am.Architecture == scmpArch {
			out.Architectures = append(out.Architectures, am.SubArchitectures...)
		}
	}
	for _, r := range in.Syscalls {
		if len(r.Excludes.Arches) > 0 && slices.Contains(r.Excludes.Arches, arch) {
			continue
		}
		if slices.ContainsFunc(r.Excludes.Caps, have) {
			continue
		}
		if len(r.Includes.Arches) > 0 && !slices.Contains(r.Includes.Arches, arch) {
			continue
		}
		if len(r.Includes.Caps) > 0 && !slices.ContainsFunc(r.Includes.Caps, have) {
			continue
		}
		out.Syscalls = append(out.Syscalls, rule{Names: r.Names, Action: r.Action, ErrnoRet: r.ErrnoRet, Args: r.Args})
	}
	return json.Marshal(out)
}
