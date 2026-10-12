package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// data_size limits /data. On firecracker /data is a disk of exactly that
// size, so the guest's own kernel stops a write that would not fit. On
// podman and krun /data is a host directory shared over virtiofs, and a
// rootless process has no way to put a quota on it; box measures it instead
// -- before a VM starts, and every quotaInterval while one runs -- and stops
// the VM when it goes over. A fast writer can overshoot by what it writes in
// one interval; the limit is what it settles back under, not a wall.

var quotaInterval = 2 * time.Second // a var so tests can shorten it

// ErrDataFull is returned when a run is stopped for going over data_size.
var ErrDataFull = errors.New("/data went over its data_size")

// quotaApplies reports whether box enforces data_size by measuring: a
// directory-backed /data with a limit set.
func quotaApplies(s boxfile.Spec) (int64, bool) {
	if s.Backend == "firecracker" {
		return 0, false // the disk is the limit
	}
	limit, err := s.DataBytes()
	return limit, err == nil && limit > 0
}

// dataUsage is the space a sandbox's /data takes on the host. A fork is its
// parent's data plus its own layer: counted in full, which errs high.
func dataUsage(name string) (int64, error) {
	dirs := []string{}
	if parent, err := sandbox.Parent(name); err == nil {
		d, err := sandbox.DataDir(parent)
		if err != nil {
			return 0, err
		}
		u, err := sandbox.UpperDir(name)
		if err != nil {
			return 0, err
		}
		dirs = append(dirs, d, u)
	} else {
		d, err := sandbox.DataDir(name)
		if err != nil {
			return 0, err
		}
		dirs = append(dirs, d)
	}
	var total int64
	for _, d := range dirs {
		n, err := dirUsage(d)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// dirUsage sums the blocks a tree holds, hard links once. A strict
// sandbox's tree belongs to its own UID and may hold directories you cannot
// enter, so it is measured from podman's namespace, where every file is.
func dirUsage(dir string) (int64, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if mine, err := sandbox.OwnedByMe(dir); err == nil && !mine {
		out, err := exec.Command("podman", "unshare", "du", "-s", "-B1", "--", dir).Output()
		if err != nil {
			return 0, fmt.Errorf("measuring %s: %v", dir, err)
		}
		f := strings.Fields(string(out))
		if len(f) == 0 {
			return 0, fmt.Errorf("measuring %s: no output from du", dir)
		}
		return strconv.ParseInt(f[0], 10, 64)
	}
	seen := map[[2]uint64]bool{}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return nil // removed while walking
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			total += fi.Size()
			return nil
		}
		if st.Nlink > 1 && !fi.IsDir() {
			k := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		total += st.Blocks * 512
		return nil
	})
	return total, err
}

// overQuota says how a sandbox stands against data_size, or nil when it is
// within it or has none.
func overQuota(name string, s boxfile.Spec) error {
	limit, ok := quotaApplies(s)
	if !ok {
		return nil
	}
	used, err := dataUsage(name)
	if err != nil {
		return err
	}
	if used > limit {
		return fmt.Errorf("%w: it holds %s, its data_size is %s. Free space (box data export, then box reset) or raise data_size",
			ErrDataFull, humanBytes(used), s.DataSize)
	}
	return nil
}

// watchQuota measures /data every quotaInterval until done is closed, and
// calls stop once if it goes over. It returns a function that reports
// whether it did.
func watchQuota(name string, s boxfile.Spec, done <-chan struct{}, stop func()) func() error {
	if _, ok := quotaApplies(s); !ok {
		return func() error { return nil }
	}
	result := make(chan error, 1)
	go func() {
		t := time.NewTicker(quotaInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				result <- nil
				return
			case <-t.C:
				if err := overQuota(name, s); errors.Is(err, ErrDataFull) {
					stop()
					result <- err
					return
				}
			}
		}
	}()
	var got error
	var read bool
	return func() error {
		if !read {
			select {
			case got = <-result:
			case <-time.After(quotaInterval + time.Second):
			}
			read = true
		}
		return got
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// spawnDataWatch starts a detached `box __watch-data <name>` for an up
// sandbox: between execs, a process left running in it can still write.
func spawnDataWatch(name string, s boxfile.Spec) {
	if _, ok := quotaApplies(s); !ok {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "__watch-data", name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// WatchData is `box __watch-data`: while the sandbox is up, measure /data
// every quotaInterval and bring the sandbox down if it goes over.
func WatchData(name string) error {
	for sandbox.IsUp(name) {
		time.Sleep(quotaInterval)
		s, err := parseSpec(name)
		if err != nil {
			return err
		}
		if err := overQuota(name, s); errors.Is(err, ErrDataFull) {
			Down(name)
			if log := openLog(name); log != nil {
				fmt.Fprintf(log, "\n=== %s data: brought down: %v\n", time.Now().UTC().Format(time.RFC3339), err)
				log.Close()
			}
			return nil
		}
	}
	return nil
}
