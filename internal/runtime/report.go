package runtime

import (
	"encoding/json"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// A security report states what a sandbox's VMM can and cannot touch. It is
// read back out of the same configuration box hands the backend -- podman's
// arguments, or the OCI spec for krun and the Firecracker jail -- not from the
// Boxfile or the docs, so it cannot describe a confinement box does not apply.
// When the sandbox is up, the running VMM is read too, from /proc, and any
// difference from the configuration is a failure.

// Report is box inspect --security-report.
type Report struct {
	Sandbox    string     `json:"sandbox"`
	Backend    string     `json:"backend"`
	Isolation  string     `json:"isolation"`
	ReadFrom   string     `json:"read_from"`
	VMM        VMMReport  `json:"vmm"`
	Network    NetReport  `json:"network"`
	Filesystem FSReport   `json:"filesystem"`
	Host       HostReport `json:"host"`
	Live       *Live      `json:"live"`
	Findings   []Finding  `json:"findings"`
}

// VMMReport is the confinement of the process that runs the VM.
type VMMReport struct {
	HostUID         int      `json:"host_uid"`
	RunsAsYou       bool     `json:"runs_as_you"`
	Slot            *int     `json:"slot,omitempty"`
	Capabilities    []string `json:"capabilities"`
	NoNewPrivileges bool     `json:"no_new_privileges"`
	Seccomp         string   `json:"seccomp"`
	Namespaces      []string `json:"namespaces"`
	PidsLimit       int64    `json:"pids_limit"`
	MemoryLimitMiB  int64    `json:"memory_limit_mib"`
}

// NetReport is what the sandbox can reach.
type NetReport struct {
	Mode                  string `json:"mode"`
	OwnNamespace          bool   `json:"own_namespace"`
	HostLoopbackReachable bool   `json:"host_loopback_reachable"`
	Egress                string `json:"egress"`
}

// FSReport is what the sandbox can read and write on the host.
type FSReport struct {
	RootReadOnly bool          `json:"root_read_only"`
	Data         DataReport    `json:"data"`
	Mounts       []MountReport `json:"mounts"`
}

// DataReport is the one thing that persists between runs.
type DataReport struct {
	Host  string `json:"host"`
	Kind  string `json:"kind"` // directory | disk
	Owner string `json:"owner,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// MountReport is one host directory the Boxfile shares in.
type MountReport struct {
	Host  string `json:"host"`
	Guest string `json:"guest"`
	Mode  string `json:"mode"`
}

// HostReport is what the host provides and what box last proved.
type HostReport struct {
	KVM                 bool   `json:"kvm"`
	Kernel              string `json:"kernel"`
	GuestKernel         string `json:"guest_kernel,omitempty"`
	IsolationVerifiedAt string `json:"isolation_verified_at,omitempty"`
	IsolationCurrent    bool   `json:"isolation_current"`
}

// Live is the running VMM as /proc shows it.
type Live struct {
	PID             int      `json:"pid"`
	HostUID         int      `json:"host_uid"`
	Capabilities    []string `json:"capabilities"`
	NoNewPrivileges bool     `json:"no_new_privileges"`
	Seccomp         string   `json:"seccomp"`
	OwnNetNamespace *bool    `json:"own_net_namespace"`
	Matches         bool     `json:"matches_configuration"`
	Differences     []string `json:"differences,omitempty"`
}

// Finding is something weaker than it could be, or wrong.
type Finding struct {
	Level string `json:"level"` // fail | warn | info
	Text  string `json:"text"`
}

// Failed reports whether anything in the report is wrong, rather than merely
// weaker than it could be.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Level == "fail" })
}

// SecurityReport builds the report for a sandbox.
func SecurityReport(name string, s boxfile.Spec) (Report, error) {
	b, err := backendFor(s)
	if err != nil {
		return Report{}, err
	}
	r := Report{Sandbox: name, Backend: b.Name(), Isolation: s.Isolation}
	if r.Isolation == "" {
		r.Isolation = "standard"
	}
	shift, err := idShift(name, s)
	if err != nil {
		return r, err
	}
	switch b.Name() {
	case "podman":
		err = reportPodman(&r, name, s)
	case "krun":
		err = reportKrun(&r, name, s)
	case "firecracker":
		err = reportFirecracker(&r, name, s, shift)
	default:
		err = fmt.Errorf("no security report for backend %s", b.Name())
	}
	if err != nil {
		return r, err
	}
	if shift > 0 {
		slot := (shift - 1) / slotSize
		r.VMM.Slot = &slot
	}
	r.VMM.RunsAsYou = r.VMM.HostUID == os.Getuid()
	r.Network.Mode = s.Network
	r.Network.HostLoopbackReachable = !r.Network.OwnNamespace
	r.Network.Egress = "all"
	if s.Network == "none" {
		r.Network.Egress = "none"
	}
	r.Filesystem.Data = dataReport(name, s)
	r.Host = hostReport()
	r.Live = liveReport(name, b.Name(), r.VMM, r.Network.OwnNamespace)
	r.Findings = findings(r, s)
	return r, nil
}

func reportPodman(r *Report, name string, s boxfile.Spec) error {
	args, err := vmArgs(name, s, false, true)
	if err != nil {
		return err
	}
	r.ReadFrom = "the podman arguments box runs this sandbox with"
	v := r.VMM
	v.Seccomp = "podman's default filter"
	v.HostUID = os.Getuid()
	r.Network.OwnNamespace = true
	v.Namespaces = []string{"user", "pid", "mount", "ipc", "uts", "cgroup", "network"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case strings.HasPrefix(a, "--cap-add="):
			v.Capabilities = capNames(strings.Split(strings.TrimPrefix(a, "--cap-add="), ","))
		case a == "--security-opt":
			switch opt := next(); {
			case opt == "no-new-privileges":
				v.NoNewPrivileges = true
			case opt == "seccomp=unconfined":
				v.Seccomp = "none"
			case strings.HasPrefix(opt, "seccomp="):
				v.Seccomp = "the Boxfile's filter (" + strings.TrimPrefix(opt, "seccomp=") + ")"
			}
		case strings.HasPrefix(a, "--pids-limit="):
			v.PidsLimit, _ = strconv.ParseInt(strings.TrimPrefix(a, "--pids-limit="), 10, 64)
		case strings.HasPrefix(a, "--memory="):
			m := strings.TrimSuffix(strings.TrimPrefix(a, "--memory="), "m")
			v.MemoryLimitMiB, _ = strconv.ParseInt(m, 10, 64)
		case a == "--uidmap":
			if uid, ok := hostFromMap(next(), "/etc/subuid"); ok {
				v.HostUID = uid
			}
		case strings.HasPrefix(a, "--network="):
			if strings.TrimPrefix(a, "--network=") == "host" {
				r.Network.OwnNamespace = false
				v.Namespaces = slices.DeleteFunc(v.Namespaces, func(n string) bool { return n == "network" })
			}
		case a == "--read-only":
			r.Filesystem.RootReadOnly = true
		case a == "-v":
			if m, ok := parseVolume(next()); ok && m.Guest != "/data" && m.Guest != agentMount {
				r.Filesystem.Mounts = append(r.Filesystem.Mounts, m)
			}
		}
	}
	r.VMM = v
	return nil
}

func reportKrun(r *Report, name string, s boxfile.Spec) error {
	b, err := krunSpec(name, s, specInput{Launch: Launch{Agent: true}})
	if err != nil {
		return err
	}
	r.ReadFrom = "the OCI spec box writes for crun"
	return fromSpec(r, b, os.Getuid())
}

func reportFirecracker(r *Report, name string, s boxfile.Spec, shift int) error {
	b, err := fcSpec(fcRun{Sandbox: name, Shift: shift, CPUs: s.CPUs, RAMMiB: s.RAMMiB, ReadOnly: s.ReadOnlyRootfs, Spec: s},
		"/vm", "/jail", "/fc", "firecracker", "/kernel")
	if err != nil {
		return err
	}
	r.ReadFrom = "the OCI spec of the jail box runs Firecracker in"
	if err := fromSpec(r, b, os.Getuid()); err != nil {
		return err
	}
	r.Filesystem.RootReadOnly = s.ReadOnlyRootfs // the guest's root is the image disk, not the jail's
	r.Filesystem.Mounts = nil                    // the jail's binds are its own files, not host directories
	return nil
}

// fromSpec reads the confinement out of an OCI spec. self is the host UID
// that namespace ID 0 stands for.
func fromSpec(r *Report, raw []byte, self int) error {
	var spec ociSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	v := r.VMM
	v.Capabilities = capNames(spec.Process.Capabilities.Effective)
	v.NoNewPrivileges = spec.Process.NoNewPrivileges
	v.Seccomp = "none"
	if len(spec.Linux.Seccomp) > 0 && string(spec.Linux.Seccomp) != "null" {
		v.Seccomp = "podman's default filter"
	}
	for _, n := range spec.Linux.Namespaces {
		v.Namespaces = append(v.Namespaces, n.Type)
		if n.Type == "network" {
			r.Network.OwnNamespace = true
		}
	}
	if !slices.Contains(v.Namespaces, "user") {
		v.Namespaces = append([]string{"user"}, v.Namespaces...) // podman's, which the spec is applied in
	}
	if res := spec.Linux.Resources; res != nil {
		if res.Pids != nil {
			v.PidsLimit = res.Pids.Limit
		}
		if res.Memory != nil {
			v.MemoryLimitMiB = res.Memory.Limit >> 20
		}
	}
	// The VMM's identity: the spec's process user, through the spec's own
	// mapping if it has one, in podman's namespace, onto the host.
	nsID := int(spec.Process.User.UID)
	for _, m := range spec.Linux.UIDMappings {
		if nsID >= int(m.ContainerID) && nsID < int(m.ContainerID+m.Size) {
			nsID = int(m.HostID) + nsID - int(m.ContainerID)
			break
		}
	}
	if uid, ok := hostID(nsID, self, subordinateRanges("/etc/subuid")); ok {
		v.HostUID = uid
	}
	r.Filesystem.RootReadOnly = spec.Root.Readonly
	for _, m := range spec.Mounts {
		if m.Type != "bind" && m.Type != "" {
			continue
		}
		if m.Destination == "/data" || strings.HasPrefix(m.Destination, agentMount) || strings.HasPrefix(m.Destination, "/etc/") {
			continue
		}
		mode := "rw"
		if slices.Contains(m.Options, "ro") {
			mode = "ro"
		}
		r.Filesystem.Mounts = append(r.Filesystem.Mounts, MountReport{Host: m.Source, Guest: m.Destination, Mode: mode})
	}
	r.VMM = v
	return nil
}

// hostFromMap turns a --uidmap "0:N:size" into the host ID sandbox root has.
func hostFromMap(m, file string) (int, bool) {
	p := strings.Split(m, ":")
	if len(p) != 3 || p[0] != "0" {
		return 0, false
	}
	n, err := strconv.Atoi(p[1])
	if err != nil {
		return 0, false
	}
	return hostID(n, os.Getuid(), subordinateRanges(file))
}

func parseVolume(v string) (MountReport, bool) {
	p := strings.Split(v, ":")
	if len(p) < 2 {
		return MountReport{}, false
	}
	mode := "rw"
	if len(p) > 2 && slices.Contains(strings.Split(p[2], ","), "ro") {
		mode = "ro"
	}
	return MountReport{Host: p[0], Guest: p[1], Mode: mode}, true
}

func dataReport(name string, s boxfile.Spec) DataReport {
	if s.Backend == "firecracker" {
		d, _ := fcDataDisk(name)
		dr := DataReport{Host: d, Kind: "disk"}
		statInto(&dr, d)
		return dr
	}
	src, _ := sandbox.DataSource(name)
	dr := DataReport{Host: src, Kind: "directory"}
	statInto(&dr, src)
	return dr
}

func statInto(dr *DataReport, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if uid, gid, ok := statOwner(fi); ok {
		dr.Owner = fmt.Sprintf("%d:%d", uid, gid)
	}
	dr.Mode = fmt.Sprintf("%o", fi.Mode().Perm())
}

func hostReport() HostReport {
	h := HostReport{}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		h.KVM = true
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	if t, ok := loadToken(); ok {
		h.GuestKernel, h.IsolationVerifiedAt = t.Guest, t.VerifiedAt
		if id, err := RuntimeIdentity(); err == nil {
			h.IsolationCurrent = t.Identity == id
		}
	}
	return h
}

// vmmPID finds the running VMM of an up sandbox, or 0.
func vmmPID(name, backend string) int {
	vm := upContainer(name)
	var out string
	switch backend {
	case "firecracker":
		dir, err := sandbox.VMsDir()
		if err != nil {
			return 0
		}
		b, _ := os.ReadFile(filepath.Join(dir, vm, "pid"))
		out = string(b)
	case "krun":
		b, err := exec.Command("krun", "--root", krunRoot(), "state", vm).Output()
		if err != nil {
			return 0
		}
		var st struct {
			Pid int `json:"pid"`
		}
		if json.Unmarshal(b, &st) == nil {
			return st.Pid
		}
	default:
		b, err := exec.Command("podman", "inspect", vm, "--format", "{{.State.Pid}}").Output()
		if err != nil {
			return 0
		}
		out = string(b)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func liveReport(name, backend string, want VMMReport, wantOwnNet bool) *Live {
	if !sandbox.IsUp(name) {
		return nil
	}
	pid := vmmPID(name, backend)
	if pid <= 0 {
		return &Live{Differences: []string{"the sandbox is up but its VMM could not be found"}}
	}
	st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return &Live{PID: pid, Differences: []string{"the VMM's /proc status could not be read"}}
	}
	l := &Live{PID: pid}
	for _, line := range strings.Split(string(st), "\n") {
		k, v, _ := strings.Cut(line, ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "Uid":
			l.HostUID, _ = strconv.Atoi(f[0])
		case "CapEff":
			mask, _ := strconv.ParseUint(f[0], 16, 64)
			l.Capabilities = capsFromMask(mask)
		case "NoNewPrivs":
			l.NoNewPrivileges = f[0] == "1"
		case "Seccomp":
			l.Seccomp = map[string]string{"0": "none", "1": "strict", "2": "filter"}[f[0]]
		}
	}
	if own, ok := ownNetNamespace(pid); ok {
		l.OwnNetNamespace = &own
	}
	compareLive(l, want, wantOwnNet)
	return l
}

// compareLive records every way the running VMM differs from its
// configuration. Anything it cannot read counts as a difference, never as a
// match.
func compareLive(l *Live, want VMMReport, wantOwnNet bool) {
	l.Differences = nil
	if l.HostUID != want.HostUID {
		l.Differences = append(l.Differences, fmt.Sprintf("runs as host UID %d, configured %d", l.HostUID, want.HostUID))
	}
	if !slices.Equal(l.Capabilities, want.Capabilities) {
		l.Differences = append(l.Differences, fmt.Sprintf("holds capabilities %v, configured %v", l.Capabilities, want.Capabilities))
	}
	if l.NoNewPrivileges != want.NoNewPrivileges {
		l.Differences = append(l.Differences, fmt.Sprintf("no_new_privs is %v, configured %v", l.NoNewPrivileges, want.NoNewPrivileges))
	}
	if (want.Seccomp != "none") != (l.Seccomp == "filter" || l.Seccomp == "strict") {
		l.Differences = append(l.Differences, fmt.Sprintf("seccomp is %s, configured %s", l.Seccomp, want.Seccomp))
	}
	if l.OwnNetNamespace == nil {
		l.Differences = append(l.Differences, "the VMM's network namespace could not be read")
	} else if *l.OwnNetNamespace != wantOwnNet {
		l.Differences = append(l.Differences, fmt.Sprintf("own network namespace is %v, configured %v", *l.OwnNetNamespace, wantOwnNet))
	}
	l.Matches = len(l.Differences) == 0
}

// ownNetNamespace compares the VMM's network namespace with box's own. A VMM
// running as another UID hides its namespace links from you; podman's
// namespace, whose root is you, can read them. An unreadable link is
// unknown, never "different".
func ownNetNamespace(pid int) (bool, bool) {
	mine, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return false, false
	}
	theirs, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil {
		out, err := exec.Command("podman", "unshare", "readlink", fmt.Sprintf("/proc/%d/ns/net", pid)).Output()
		if err != nil {
			return false, false
		}
		theirs = strings.TrimSpace(string(out))
	}
	if theirs == "" {
		return false, false
	}
	return theirs != mine, true
}

func findings(r Report, s boxfile.Spec) []Finding {
	var out []Finding
	add := func(level, text string) { out = append(out, Finding{level, text}) }
	if r.Live != nil && !r.Live.Matches {
		add("fail", "the running VMM differs from its configuration: "+strings.Join(r.Live.Differences, "; "))
	}
	if r.Isolation == "strict" && r.VMM.RunsAsYou {
		add("fail", "isolation is strict, but the VMM is configured to run as your own UID")
	}
	if r.Isolation != "strict" {
		add("warn", "the VMM runs as your own UID: escaping the VM and the VMM lands in your account. Use isolation: strict for agents.")
	}
	if !r.VMM.NoNewPrivileges {
		add("fail", "the VMM can gain privileges (no_new_privs is off)")
	}
	if r.VMM.Seccomp == "none" {
		add("warn", "the VMM runs without a seccomp filter")
	}
	if r.Network.Egress == "all" {
		add("warn", "full internet egress: anything the guest can read, including /data and mounts, can leave. Per-sandbox allowlists are planned (#7).")
	}
	for _, m := range r.Filesystem.Mounts {
		if m.Mode == "rw" {
			add("warn", fmt.Sprintf("the guest can write %s on the host (mounted at %s)", m.Host, m.Guest))
		}
	}
	if !r.Host.KVM {
		add("fail", "/dev/kvm is not usable here: no sandbox can run")
	}
	if !r.Host.IsolationCurrent {
		add("info", "the isolation check has not run for the current runtime; the next run makes it")
	}
	if r.Live == nil {
		add("info", "the sandbox is not up, so the running VMM was not compared; box up it to check that too")
	}
	add("info", "image builds run in a container on the host kernel, not in a VM; build only Boxfiles you would run yourself (#35)")
	return out
}

// capNames normalises capability names: CAP_ prefix off, sorted.
func capNames(in []string) []string {
	out := []string{}
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		out = append(out, strings.TrimPrefix(c, "CAP_"))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

var capBits = []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID",
	"SETUID", "SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW",
	"IPC_LOCK", "IPC_OWNER", "SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT",
	"SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE",
	"AUDIT_WRITE", "AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG", "WAKE_ALARM",
	"BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE"}

// capsFromMask names the bits of a CapEff mask.
func capsFromMask(mask uint64) []string {
	out := []string{}
	for mask != 0 {
		b := bits.TrailingZeros64(mask)
		mask &^= 1 << b
		if b < len(capBits) {
			out = append(out, capBits[b])
		} else {
			out = append(out, "CAP_"+strconv.Itoa(b))
		}
	}
	slices.Sort(out)
	return out
}

func statOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
