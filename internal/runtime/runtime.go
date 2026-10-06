// Package runtime drives podman + the krun runtime. It is the only place that
// knows how a Boxfile spec becomes VM isolation, so swapping the backend later
// means touching this file alone.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"box/internal/agent"
	"box/internal/boxfile"
	"box/internal/sandbox"
)

// ExitTimeout matches timeout(1) so a harness can tell "killed on time" from a
// normal non-zero exit.
const ExitTimeout = 124

// ErrTimeout is returned by Run when the wall-clock limit is hit.
var ErrTimeout = errors.New("timed out")

// vmArgs builds the podman invocation. All isolation lives in these flags, so
// they are constructed in exactly one place.
func vmArgs(name string, s boxfile.Spec, interactive, useKrun bool) ([]string, error) {
	// /data itself, or for a fork an overlay on the parent's /data.
	data, err := sandbox.DataMount(name)
	if err != nil {
		return nil, err
	}
	if err := checkMountSources(name, s); err != nil {
		return nil, err
	}
	// The sandbox image is always built locally. --pull=never makes a missing
	// image an error rather than a registry pull, so the only image that can
	// boot here is the one `box build` produced.
	args := []string{"run", "--rm", "--pull=never"}
	if useKrun {
		args = append(args, "--runtime", "krun")
	}
	args = append(args, hardening(s)...)
	args = append(args,
		"--network="+s.Network,
		"--annotation", "krun.cpus="+strconv.Itoa(s.CPUs),
		"--annotation", "krun.ram_mib="+strconv.Itoa(s.RAMMiB),
		"-v", data,
	)
	// Declarative mounts from the Boxfile. Mode was normalized at parse
	// time, but stay defensive: an empty mode means read-only.
	for _, m := range s.Mounts {
		mode := m.Mode
		if mode == "" {
			mode = "ro"
		}
		args = append(args, "-v", m.Host+":"+m.Guest+":"+mode)
	}
	// passt gives the guest a real interface with a default route, which
	// krun's default TSI networking does not. Workloads that inspect routing
	// (k3s, anything expecting a normal NIC) need it.
	if s.Passt {
		args = append(args, "--annotation", "krun.use_passt=1")
	}
	if s.ReadOnlyRootfs {
		args = append(args, "--read-only")
	}
	if s.Seccomp != "" {
		args = append(args, "--security-opt", "seccomp="+s.Seccomp)
	}
	if interactive {
		args = append(args, "-i")
		// Only request a TTY when we have one; -t with piped stdin hangs.
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			args = append(args, "-t")
		}
	}
	return args, nil
}

// LoadSpec reads a sandbox's Boxfile.
func LoadSpec(name string) (boxfile.Spec, error) {
	if !sandbox.Exists(name) {
		return boxfile.Spec{}, fmt.Errorf("%w %q (create it: box new %s)", ErrNoSandbox, name, name)
	}
	return parseSpec(name)
}

// ErrNoSandbox is returned for a name with no Boxfile.
var ErrNoSandbox = errors.New("no sandbox")

// RunFresh runs argv in a fresh microVM: a pooled one when the sandbox keeps
// a warm pool and one is waiting, otherwise one booted now. notice, if not
// nil, hears about slow paths such as a re-verification.
func RunFresh(name string, s boxfile.Spec, argv []string, streams Streams, notice func(string)) error {
	// A pooled VM was checked when it booted and its kernel is checked again
	// before the command starts, so the warm path skips the toolchain checks
	// below and costs a connection, not a boot.
	if s.Warm > 0 {
		if ran, err := RunWarm(name, s, argv, streams); ran {
			return err
		}
	}
	if err := checked(name, s, notice); err != nil {
		return err
	}
	return Run(name, s, argv, streams)
}

// UpChecked is Up behind the same checks a run gets.
func UpChecked(name string, s boxfile.Spec, notice func(string)) (time.Duration, error) {
	if err := checked(name, s, notice); err != nil {
		return 0, err
	}
	return Up(name, s)
}

// checked confirms the toolchain is present and the kernel boundary still
// holds -- cheaply when the runtime is unchanged -- before untrusted code runs.
func checked(name string, s boxfile.Spec, notice func(string)) error {
	b, err := backendFor(s)
	if err != nil {
		return err
	}
	if err := b.Preflight(); err != nil {
		return err
	}
	fresh, err := EnsureIsolated(name, s)
	if err != nil {
		return err
	}
	if fresh && notice != nil {
		notice("re-verified isolation (runtime changed since last check)")
	}
	return nil
}

// vmmCaps are the only capabilities the VMM keeps. libkrun's own security
// model puts the guest and the VMM in one security context -- whatever the
// VMM may do, the guest can reach through it -- so the VMM gets no more than
// the guest's file sharing needs. virtiofs acts for guest users, which takes
// CHOWN, DAC_OVERRIDE, FOWNER, SETUID and SETGID; without any of them guest
// root cannot create a directory. NET_BIND_SERVICE lets a guest server take
// a port below 1024 in the sandbox's own network namespace. Everything else
// podman grants by default (KILL, SETPCAP, SETFCAP, SYS_CHROOT, FSETID) goes.
const vmmCaps = "CHOWN,DAC_OVERRIDE,FOWNER,SETUID,SETGID,NET_BIND_SERVICE"

// vmmOverheadMiB is memory the VMM may use beyond the guest's RAM. A guest
// filling all 512 MiB of its RAM ran within 64 MiB of overhead; this leaves
// room for device buffers without letting the VMM grow unbounded.
const vmmOverheadMiB = 256

// vmmPidsLimit bounds the VMM's host-side threads (about 25 at rest). Guest
// processes live in the guest kernel and do not count against it.
const vmmPidsLimit = 512

// hardening confines the VMM process on the host. It is the boundary between
// a guest that has broken out of libkrun and the rest of the machine.
func hardening(s boxfile.Spec) []string {
	args := []string{
		"--security-opt", "no-new-privileges",
		"--cap-drop=all", "--cap-add=" + vmmCaps,
		"--pids-limit=" + strconv.Itoa(vmmPidsLimit),
		"--memory=" + strconv.Itoa(s.RAMMiB+vmmOverheadMiB) + "m",
	}
	if s.Isolation == "strict" {
		// Rootless podman maps the sandbox's root to your own UID, so a guest
		// that escaped libkrun would land in your account. Strict maps it to
		// the first UID of your subordinate range instead and leaves your UID
		// out of the mapping entirely: an escape lands in an account that owns
		// nothing of yours. The range is the same for every strict sandbox, so
		// it separates sandboxes from you, not from each other.
		args = append(args, "--uidmap", strictMap, "--gidmap", strictMap)
	}
	return args
}

// strictMap maps sandbox IDs 0-65535 onto IDs 1-65536 of the rootless user
// namespace, i.e. onto your subordinate UIDs and never onto you (ID 0 there).
const strictMap = "0:1:65536"

// prepareData gives /data to whoever the sandbox's root is on the host: you
// under standard isolation, the first subordinate UID under strict. Ownership
// only changes when a sandbox switches between the two, so this is a stat on
// every boot and a chown once.
func prepareData(name string, s boxfile.Spec) error {
	if parent, err := sandbox.Parent(name); err == nil {
		// A fork's /data is the parent's, overlaid. The lower layer is
		// prepared for the parent's isolation, so the fork must use the same
		// one: the two share the files, and files have one owner.
		ps, err := parseSpec(parent)
		if err != nil {
			return err
		}
		if ps.Isolation != s.Isolation {
			return fmt.Errorf("%s is a fork of %s, whose isolation is %s; set the same in the fork's Boxfile",
				name, parent, ps.Isolation)
		}
		if err := prepareData(parent, ps); err != nil {
			return err
		}
		upper, err := sandbox.UpperDir(name)
		if err != nil {
			return err
		}
		return ownData(upper, s)
	}
	data, err := sandbox.DataDir(name)
	if err != nil {
		return err
	}
	return ownData(data, s)
}

// ownData gives a data directory to whoever the sandbox's root is on the host.
func ownData(data string, s boxfile.Spec) error {
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	mine, err := sandbox.OwnedByMe(data)
	if err != nil {
		return err
	}
	strict := s.Isolation == "strict"
	if mine != strict {
		return nil // already owned by the sandbox's root
	}
	// Inside podman unshare, ID 0 is you and ID 1 the first subordinate UID.
	owner := "0:0"
	if strict {
		owner = "1:1"
	}
	if out, err := exec.Command("podman", "unshare", "chown", "-R", owner, data).CombinedOutput(); err != nil {
		return fmt.Errorf("re-owning /data for isolation %s: %s", s.Isolation, strings.TrimSpace(string(out)))
	}
	return nil
}

// Build renders the Containerfile from the spec, writes it, and builds the image.
func Build(name string, s boxfile.Spec) error {
	cfPath, err := sandbox.ContainerfilePath(name)
	if err != nil {
		return err
	}
	dir, err := sandbox.Dir(name)
	if err != nil {
		return err
	}
	cf, err := s.Render(dir) // materialises any blueprint files first
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfPath, []byte(cf), 0o644); err != nil {
		return err
	}
	if err := stream(exec.Command("podman", "build", "-t", sandbox.ImageTag(name), dir)); err != nil {
		return err
	}
	_, err = RecordImageID(name)
	return err
}

// RemoveImage deletes a sandbox's built image, if there is one.
func RemoveImage(name string) {
	exec.Command("podman", "rmi", "-f", sandbox.ImageTag(name)).Run()
}

// CopyImage gives a fork its parent's image under its own tag, so it needs
// no build of its own. A parent that never built has no image to copy.
func CopyImage(from, to string) {
	if exec.Command("podman", "tag", sandbox.ImageTag(from), sandbox.ImageTag(to)).Run() == nil {
		if id, err := os.ReadFile(imageIDPath(from)); err == nil {
			os.WriteFile(imageIDPath(to), id, 0o644)
		}
	}
}

// RetagImage moves a built image to a new name so a rename does not force a
// rebuild. A missing image is not an error: the sandbox may never have built.
func RetagImage(from, to string) {
	if err := exec.Command("podman", "tag",
		sandbox.ImageTag(from), sandbox.ImageTag(to)).Run(); err == nil {
		exec.Command("podman", "rmi", "-f", sandbox.ImageTag(from)).Run()
	}
}

// Streams is where a command's input comes from and its output goes. The CLI
// passes its own terminal; the SDK server passes buffers.
type Streams struct {
	Stdin          io.Reader // nil: the command sees a closed stdin
	Stdout, Stderr io.Writer
}

// Terminal is this process's own stdout and stderr, with no stdin.
func Terminal() Streams { return Streams{Stdout: os.Stdout, Stderr: os.Stderr} }

// Run executes argv in a fresh microVM. A timed-out run is reaped explicitly:
// killing the podman CLI does not stop the VM it started, so --rm never fires.
func Run(name string, s boxfile.Spec, argv []string, streams Streams) error {
	if err := prepareData(name, s); err != nil {
		return err
	}
	b, err := backendFor(s)
	if err != nil {
		return err
	}
	runName := fmt.Sprintf("box-%s-%d", name, os.Getpid())
	cmd, err := b.Launch(name, s, Launch{VM: runName, Argv: argv, Interactive: streams.Stdin != nil})
	if err != nil {
		return err
	}
	cmd.Stdin = streams.Stdin

	ctx := context.Background()
	if s.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(s.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	log := openLog(name)
	if log != nil {
		defer log.Close()
		fmt.Fprintf(log, "\n=== %s run: %s\n",
			time.Now().UTC().Format(time.RFC3339), strings.Join(argv, " "))
	}
	started := time.Now()
	// Bound to ctx by hand: the backend chose the argv.
	if ctx.Done() != nil {
		stop := context.AfterFunc(ctx, func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		})
		defer stop()
	}
	err = streamTee(cmd, streams, log)
	if log != nil {
		code := 0
		if err != nil {
			code = ExitCode(err)
		}
		fmt.Fprintf(log, "=== exit %d (%.1fs)\n", code, time.Since(started).Seconds())
	}

	if ctx.Err() == context.DeadlineExceeded {
		b.Remove(runName)
		return ErrTimeout
	}
	return err
}

// openLog returns an append handle for the sandbox's run log, or nil if it
// cannot be opened. Logging is best-effort: it must never break a run.
func openLog(name string) *os.File {
	p, err := sandbox.LogPath(name)
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// Shell opens an interactive session in one microVM. No timeout: the user is it.
func Shell(name string, s boxfile.Spec) error {
	if err := prepareData(name, s); err != nil {
		return err
	}
	b, err := backendFor(s)
	if err != nil {
		return err
	}
	cmd, err := b.Launch(name, s, Launch{Interactive: true, Argv: []string{"/bin/sh", "-l"}})
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	return stream(cmd)
}

// GuestKernel returns the kernel reported from inside the microVM.
func GuestKernel(name string, s boxfile.Spec) (string, error) {
	return kernelOf(name, s, true)
}

// BaselineKernel returns the kernel a plain container sees: the host kernel on
// Linux, the podman machine's kernel on macOS. Comparing against this rather
// than the host's own uname is what makes the isolation check honest on macOS,
// where the host runs Darwin and every container kernel differs from it
// whether or not a microVM is involved.
func BaselineKernel(name string, s boxfile.Spec) (string, error) {
	return kernelOf(name, s, false)
}

func kernelOf(name string, s boxfile.Spec, useKrun bool) (string, error) {
	if err := prepareData(name, s); err != nil {
		return "", err
	}
	b, err := backendFor(s)
	if err != nil {
		return "", err
	}
	cmd, err := b.Launch(name, s, Launch{Argv: []string{"uname", "-r"}, Baseline: !useKrun})
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), err
}

// HostKernel is the kernel release this process runs on.
func HostKernel() (string, error) { return agent.KernelRelease() }

// stream runs cmd with its stdout/stderr wired to the process. Any timeout is
// bound into the command via exec.CommandContext before it reaches here.
func stream(cmd *exec.Cmd) error { return streamTee(cmd, Terminal(), nil) }

// streamTee is stream, additionally copying output to log when non-nil.
func streamTee(cmd *exec.Cmd, s Streams, log io.Writer) error {
	out, errw := tee(s, log)
	cmd.Stdout = out
	cmd.Stderr = errw
	return cmd.Run()
}

// tee adds log, when non-nil, to a command's output streams.
func tee(s Streams, log io.Writer) (out, errw io.Writer) {
	if log == nil {
		return s.Stdout, s.Stderr
	}
	return io.MultiWriter(s.Stdout, log), io.MultiWriter(s.Stderr, log)
}

// ExitCode extracts a child process exit code from a Run/Shell error, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	var es *ExitStatus
	if errors.As(err, &es) {
		return es.Code
	}
	return -1
}
