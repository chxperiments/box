package runtime

// The warm pool keeps `box run`'s promise -- every run gets a fresh
// microVM -- without paying a boot per run. VMs are booted ahead of time and
// each one serves exactly one run before it is destroyed.
//
// Each pooled VM is a file in the sandbox's pool directory. Its suffix is its
// state, and every transition is a rename, so two runs can never claim the
// same VM:
//
//	<id>.json            booted and waiting
//	<id>.claimed-<pid>   serving one run
//	<id>.stale           used, or no longer matching the sandbox; to remove
//
// Nothing here is a daemon. A run that uses or misses a VM starts a detached
// `box __tend`, which removes spent VMs and boots replacements. Tenders
// serialise on a lock, so any number may be started at once.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"box/internal/agent"
	"box/internal/boxfile"
	"box/internal/sandbox"
)

// poolLabel marks pooled containers, so a drain finds strays whose file is
// gone.
const poolLabel = "box.pool"

type poolEntry struct {
	upState
	Fingerprint string `json:"fingerprint"`
}

// fingerprint identifies what a sandbox's VMs are booted from: its Boxfile,
// and its Containerfile, which each build rewrites. A pooled VM whose
// fingerprint no longer matches was booted from an older definition and is
// discarded rather than used.
func fingerprint(name string) string {
	h := sha256.New()
	if p, err := sandbox.BoxfilePath(name); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			h.Write(b)
		}
	}
	if p, err := sandbox.ContainerfilePath(name); err == nil {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(h, "\x00%d:%d", fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func poolContainer(name string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "box-pool-" + name + "-" + hex.EncodeToString(b), nil
}

func readEntry(path string) (poolEntry, error) {
	var e poolEntry
	b, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(b, &e)
}

// stalePath is where a pool file goes once its VM is spent: same id, whatever
// state it was in.
func stalePath(path string) string {
	id, _, _ := strings.Cut(filepath.Base(path), ".")
	return filepath.Join(filepath.Dir(path), id+".stale")
}

// claim takes one waiting VM for this process. A VM that no longer matches
// the sandbox, or does not answer, is marked stale and the next one tried.
func claim(name string) (poolEntry, string, bool) {
	dir, err := sandbox.PoolDir(name)
	if err != nil {
		return poolEntry{}, "", false
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return poolEntry{}, "", false
	}
	fp := fingerprint(name)
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		src := filepath.Join(dir, f.Name())
		mine := strings.TrimSuffix(src, ".json") + ".claimed-" + strconv.Itoa(os.Getpid())
		if os.Rename(src, mine) != nil {
			continue // another run got it first
		}
		e, err := readEntry(mine)
		if err != nil || e.Fingerprint != fp || ping(e.upState) != nil {
			os.Rename(mine, stalePath(mine))
			continue
		}
		return e, mine, true
	}
	return poolEntry{}, "", false
}

// RunWarm runs argv in a pooled VM, if one is waiting. It reports false when
// none is, and the caller boots one the usual way; either way the pool is
// topped up in the background for the next run.
func RunWarm(name string, s boxfile.Spec, argv []string, streams Streams) (bool, error) {
	e, claimed, ok := claim(name)
	defer SpawnTender(name)
	if !ok {
		return false, nil
	}
	streams.Stdin = nil // run never forwards stdin, warm or cold
	err := execOn(name, e.upState, s, "run", argv, streams)
	// One run per VM, whatever happened in it: that is what makes it fresh.
	os.Rename(claimed, stalePath(claimed))
	if errors.Is(err, errLost) {
		return true, fmt.Errorf("the sandbox was lost mid-run: %v", err)
	}
	return true, err
}

// SpawnTender starts a detached `box __tend <name>` and returns at once.
// It is best-effort: a pool that is not refilled only means the next run
// boots cold.
func SpawnTender(name string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "__tend", name)
	// Its own session, so it outlives this command and a ^C here.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// lockPool serialises tenders and drains on one sandbox's pool.
func lockPool(name string) (dir string, unlock func(), err error) {
	dir, err = sandbox.PoolDir(name)
	if err != nil {
		return "", nil, err
	}
	// Pool files hold agent tokens, so the directory is owner-only.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return "", nil, err
	}
	return dir, func() { f.Close() }, nil
}

// alive reports whether the process that claimed a VM is still running.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// sweep removes spent VMs and returns the paths of those still waiting.
func sweep(b Backend, dir, fp string, keep func(poolEntry) bool) []string {
	files, _ := os.ReadDir(dir)
	var ready []string
	for _, f := range files {
		p := filepath.Join(dir, f.Name())
		n := f.Name()
		spent := strings.HasSuffix(n, ".stale")
		if i := strings.Index(n, ".claimed-"); i >= 0 {
			pid, err := strconv.Atoi(n[i+len(".claimed-"):])
			spent = err != nil || !alive(pid)
		}
		if strings.HasSuffix(n, ".json") {
			e, err := readEntry(p)
			spent = err != nil || e.Fingerprint != fp || !keep(e)
			if !spent {
				ready = append(ready, p)
				continue
			}
		}
		if !spent {
			continue
		}
		if e, err := readEntry(p); err == nil {
			b.Remove(e.Container)
		}
		os.Remove(p)
	}
	return ready
}

// Tend brings a sandbox's pool to the size its Boxfile asks for: spent and
// outdated VMs are removed, and replacements booted. It holds the pool lock
// throughout, so concurrent tenders queue and each finds the pool as the last
// one left it.
func Tend(name string) error {
	dir, unlock, err := lockPool(name)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := parseSpec(name)
	if err != nil {
		sweep(backendOf(name), dir, "", func(poolEntry) bool { return false })
		return err
	}
	fp := fingerprint(name)
	kept := 0
	ready := sweep(backendOf(name), dir, fp, func(poolEntry) bool {
		kept++
		return kept <= s.Warm // shrink to a lowered warm
	})
	if len(ready) >= s.Warm {
		return nil
	}
	if err := Preflight(); err != nil {
		return err
	}
	// Pooled VMs will run untrusted code, so they get the same boundary check
	// a cold run does before any is booted.
	if _, err := EnsureIsolated(name, s); err != nil {
		return err
	}
	for n := len(ready); n < s.Warm; n++ {
		ctr, err := poolContainer(name)
		if err != nil {
			return err
		}
		st, err := boot(name, s, ctr, map[string]string{poolLabel: name})
		if err != nil {
			return err
		}
		warmUp(st, s)
		b, err := json.Marshal(poolEntry{upState: st, Fingerprint: fp})
		if err != nil {
			backendOf(name).Remove(ctr)
			return err
		}
		p := filepath.Join(dir, strings.TrimPrefix(ctr, "box-pool-"+name+"-"))
		// Written aside and renamed in, so a claim never reads half a file.
		if err := os.WriteFile(p+".partial", b, 0o600); err != nil {
			backendOf(name).Remove(ctr)
			return err
		}
		if err := os.Rename(p+".partial", p+".json"); err != nil {
			os.Remove(p + ".partial")
			backendOf(name).Remove(ctr)
			return err
		}
	}
	return nil
}

// warmUp runs a no-op, then the Boxfile's warmup lines, in a freshly booted
// VM before it joins the pool. A VM's first command pays for loading the
// shell, libc and the binary into the guest; left idle, that costs a run
// ~30ms more than it should. warmup lines let a sandbox preload what its runs
// actually use -- starting python3 once, say. They are the sandbox's own
// Boxfile content, like run steps, and a failing one only leaves the VM
// less warm.
func warmUp(st upState, s boxfile.Spec) {
	lines := append([]string{":"}, s.Warmup...)
	for _, l := range lines {
		agent.Exec(st.Addr, agent.Request{
			Token:          st.Token,
			Argv:           []string{"/bin/sh", "-c", l},
			TimeoutSeconds: 60,
		}, nil, nil, io.Discard, io.Discard)
	}
}

// Drain removes every pooled VM of a sandbox, waiting for a running tender to
// finish first. Anything that swaps /data, the image or the name drains, since
// a waiting VM holds the old ones.
func Drain(name string) error {
	dir, unlock, err := lockPool(name)
	if err != nil {
		return err
	}
	b := backendOf(name)
	sweep(b, dir, "", func(poolEntry) bool { return false })
	// Catch VMs whose file is gone, e.g. a tender killed mid-boot.
	b.RemoveLabelled(poolLabel, name)
	unlock()
	return nil
}

// RemovePool drains a sandbox's pool and deletes its directory.
func RemovePool(name string) error {
	if err := Drain(name); err != nil {
		return err
	}
	dir, err := sandbox.PoolDir(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Warm counts a sandbox's VMs that are booted and waiting.
func Warm(name string) int {
	dir, err := sandbox.PoolDir(name)
	if err != nil {
		return 0
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return len(m)
}

func parseSpec(name string) (boxfile.Spec, error) {
	p, err := sandbox.BoxfilePath(name)
	if err != nil {
		return boxfile.Spec{}, err
	}
	return boxfile.Parse(p)
}

// TendLog records a background tender's failure where `box logs` shows
// it, since a detached process has no terminal to report to.
func TendLog(name string, err error) {
	if log := openLog(name); log != nil {
		fmt.Fprintf(log, "\n=== %s pool: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		log.Close()
	}
}
