// Package guest is box inside a Firecracker microVM. The kernel starts
// the box binary as PID 1 (init=/.box/box); Init sets the
// machine up, runs the agent, and reaps orphans. Prepare runs once per
// restored copy, before anything else, to make it a distinct machine.
//
// Nothing here runs on the host. The podman and krun backends boot images
// with libkrun's own init and never reach this code.
package guest

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Files the image build places in the rootfs for the guest.
const (
	agentPath = "/.box/box"
	envPath   = "/.box/env" // KEY=VALUE lines: the image's ENV and the Boxfile's env
	dataDev   = "/dev/vdb"  // the sandbox's /data disk
	logPrefix = "box-init: "
)

// cmdline reads box.* parameters from the kernel command line.
func cmdline() map[string]string {
	b, _ := os.ReadFile("/proc/cmdline")
	out := map[string]string{}
	for _, f := range strings.Fields(string(b)) {
		if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(k, "box.") {
			out[strings.TrimPrefix(k, "box.")] = v
		}
	}
	return out
}

func logf(format string, args ...any) {
	if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
		fmt.Fprintf(f, logPrefix+format+"\n", args...)
		f.Close()
	}
}

func mount(src, dst, fstype string, flags uintptr, data string) error {
	// The kernel may have mounted it already (devtmpfs on /dev, when the
	// root is used as is under readonly: true).
	if dst != "/proc" && mounted(dst) {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := unix.Mount(src, dst, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s: %w", fstype, dst, err)
	}
	return nil
}

// Init is PID 1. It never returns; on a fatal error the VM powers off.
func Init() {
	if err := setup(); err != nil {
		logf("%v", err)
		unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	}
	startAgent()
	reap()
}

func setup() error {
	// /proc first: the command line is read from it.
	if err := mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return err
	}
	args := cmdline()
	if args["ro"] != "1" {
		// The root disk is read-only and shared by every VM of the image.
		// Writes go to an overlay in RAM, gone with the VM: what a run
		// changes outside /data resets, as on the other backends.
		if err := mount("tmpfs", "/.rw", "tmpfs", 0, "mode=0755"); err != nil {
			return err
		}
		for _, d := range []string{"/.rw/upper", "/.rw/work"} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		if err := mount("overlay", "/.root", "overlay", 0, "lowerdir=/,upperdir=/.rw/upper,workdir=/.rw/work"); err != nil {
			return err
		}
		if err := os.MkdirAll("/.root/.oldroot", 0o700); err != nil {
			return err
		}
		if err := unix.PivotRoot("/.root", "/.root/.oldroot"); err != nil {
			return fmt.Errorf("pivot_root: %w", err)
		}
		if err := unix.Chdir("/"); err != nil {
			return err
		}
		unix.Unmount("/.oldroot/proc", unix.MNT_DETACH)
		if err := mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return err
		}
	}
	for _, m := range []struct{ src, dst, fs, data string }{
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755"},
		{"devpts", "/dev/pts", "devpts", "newinstance,ptmxmode=0666,mode=0620"},
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", "mode=1777"},
	} {
		if err := mount(m.src, m.dst, m.fs, unix.MS_NOSUID, m.data); err != nil {
			return err
		}
	}
	if h := args["hostname"]; h != "" {
		unix.Sethostname([]byte(h))
	}
	if err := linkUp("lo"); err != nil {
		logf("lo: %v", err)
	}
	return os.MkdirAll("/data", 0o755)
}

// linkUp brings an interface up without needing ip(8) in the image.
func linkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// startAgent runs the agent as PID 1's child, with the image's environment,
// so commands it starts see what they would under podman. It is restarted if
// it ever dies.
func startAgent() {
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	if f, err := os.Open(envPath); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if l := sc.Text(); strings.Contains(l, "=") {
				env = append(env, l)
			}
		}
		f.Close()
	}
	env = append(env, "BOX_AGENT_TOKEN="+cmdline()["token"])
	go func() {
		for {
			cmd := exec.Command(agentPath, "__agent", "--vsock")
			cmd.Env = env
			cmd.Dir = "/data"
			if err := cmd.Start(); err != nil {
				logf("agent: %v", err)
				time.Sleep(time.Second)
				continue
			}
			agentPID.Store(int64(cmd.Process.Pid))
			<-agentExited
			logf("agent exited; restarting")
		}
	}()
}

// reap collects every child, as PID 1 must: commands the agent started and
// then orphaned would otherwise stay zombies for the VM's lifetime.
func reap() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if int64(pid) == agentPID.Load() {
			agentExited <- struct{}{}
		}
	}
}

// Prepare is `box __prepare`, run by the host as the very first command
// in a VM restored from a snapshot. Every copy of a snapshot starts with the
// same memory -- the same random state, the same clock -- so this mixes in
// randomness only this copy got, sets the clock, and then mounts /data. The
// /data disk is not mounted at snapshot time, so no copy inherits a cached
// view of another sandbox's disk; the buffer cache is flushed before
// mounting all the same.
func Prepare(seedHex string, unixNanos int64, mountData bool) error {
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) < 32 {
		return fmt.Errorf("prepare needs at least 32 bytes of seed")
	}
	if f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0); err == nil {
		f.Write(seed)
		f.Close()
	}
	tv := unix.NsecToTimeval(unixNanos)
	if err := unix.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	if !mountData {
		return nil
	}
	if mounted("/data") {
		return nil
	}
	if fd, err := unix.Open(dataDev, unix.O_RDONLY|unix.O_CLOEXEC, 0); err == nil {
		unix.IoctlSetInt(fd, unix.BLKFLSBUF, 0)
		unix.Close(fd)
	}
	if err := unix.Mount(dataDev, "/data", "ext4", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount /data: %w", err)
	}
	return nil
}

func mounted(path string) bool {
	b, _ := os.ReadFile("/proc/self/mounts")
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == path {
			return true
		}
	}
	return false
}

var (
	agentPID    atomic.Int64
	agentExited = make(chan struct{}, 1)
)
