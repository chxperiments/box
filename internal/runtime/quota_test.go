package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

func writeBytes(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirUsageCountsBlocksAndHardLinksOnce(t *testing.T) {
	d := t.TempDir()
	writeBytes(t, filepath.Join(d, "a"), 1<<20)
	if err := os.Link(filepath.Join(d, "a"), filepath.Join(d, "b")); err != nil {
		t.Fatal(err)
	}
	// A sparse file holds almost nothing, whatever its size says.
	f, _ := os.Create(filepath.Join(d, "sparse"))
	f.Truncate(1 << 30)
	f.Close()
	got, err := dirUsage(d)
	if err != nil {
		t.Fatal(err)
	}
	if got < 1<<20 || got > 1<<20+64<<10 {
		t.Errorf("usage %d, want about 1 MiB", got)
	}
}

func quotaSandbox(t *testing.T, size string) boxfile.Spec {
	t.Helper()
	t.Setenv("BOX_HOME", t.TempDir())
	newSandbox(t, "q")
	return boxfile.Spec{Backend: "podman", DataSize: size}
}

func TestOverQuotaRefusesAFullSandbox(t *testing.T) {
	s := quotaSandbox(t, "64M")
	data, _ := sandbox.DataDir("q")
	writeBytes(t, filepath.Join(data, "small"), 1<<20)
	if err := overQuota("q", s); err != nil {
		t.Fatalf("1 MiB of 64: %v", err)
	}
	writeBytes(t, filepath.Join(data, "big"), 65<<20)
	if err := overQuota("q", s); !errors.Is(err, ErrDataFull) {
		t.Fatalf("66 MiB of 64: %v", err)
	}
	// No data_size, no limit; firecracker's limit is its disk, not this.
	if err := overQuota("q", boxfile.Spec{Backend: "podman"}); err != nil {
		t.Errorf("no data_size: %v", err)
	}
	if err := overQuota("q", boxfile.Spec{Backend: "firecracker", DataSize: "64M"}); err != nil {
		t.Errorf("firecracker: %v", err)
	}
}

func TestWatchQuotaStopsARunThatGoesOver(t *testing.T) {
	old := quotaInterval
	quotaInterval = 50 * time.Millisecond
	defer func() { quotaInterval = old }()
	s := quotaSandbox(t, "64M")
	data, _ := sandbox.DataDir("q")

	stopped := make(chan struct{}, 1)
	done := make(chan struct{})
	exceeded := watchQuota("q", s, done, func() { stopped <- struct{}{} })
	writeBytes(t, filepath.Join(data, "big"), 65<<20) // the run writes too much
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never stopped the run")
	}
	close(done)
	if err := exceeded(); !errors.Is(err, ErrDataFull) {
		t.Errorf("result: %v", err)
	}

	// Under the limit, it never stops anything.
	os.Remove(filepath.Join(data, "big"))
	done2 := make(chan struct{})
	calls := 0
	ex2 := watchQuota("q", s, done2, func() { calls++ })
	time.Sleep(200 * time.Millisecond)
	close(done2)
	if err := ex2(); err != nil || calls != 0 {
		t.Errorf("under the limit: %v, %d stops", err, calls)
	}
}
