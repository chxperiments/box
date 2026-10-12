package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

func TestIDMap(t *testing.T) {
	if got := idMap(1); got != "0:1:65536" {
		t.Errorf("slot 0: %s", got)
	}
	if got := idMap(1 + 3*slotSize); got != "0:196609:65536" {
		t.Errorf("slot 3: %s", got)
	}
}

func TestHostIDWalksRangesInOrder(t *testing.T) {
	rs := []subRange{{start: 100000, count: 65536}, {start: 500000, count: 131072}}
	for _, c := range []struct{ ns, host int }{
		{0, 1000},                // you
		{1, 100000},              // first subordinate ID
		{65536, 165535},          // end of the first range
		{65537, 500000},          // start of the second
		{1 + 2*slotSize, 565536}, // slot 2 begins inside the second range
	} {
		got, ok := hostID(c.ns, 1000, rs)
		if !ok || got != c.host {
			t.Errorf("hostID(%d) = %d, %v; want %d", c.ns, got, ok, c.host)
		}
	}
	if _, ok := hostID(1+3*slotSize, 1000, rs); ok {
		t.Error("an ID past the ranges mapped")
	}
}

func TestSubordinateRanges(t *testing.T) {
	t.Setenv("USER", "alice")
	p := filepath.Join(t.TempDir(), "subuid")
	os.WriteFile(p, []byte("bob:100000:65536\nalice:165536:65536\n# x\nalice:900000:131072\n"), 0o644)
	rs := subordinateRanges(p)
	if len(rs) != 2 || rs[0] != (subRange{165536, 65536}) || rs[1] != (subRange{900000, 131072}) {
		t.Fatalf("ranges: %+v", rs)
	}
	if rangeTotal(rs)/slotSize != 3 {
		t.Errorf("capacity %d, want 3", rangeTotal(rs)/slotSize)
	}
}

func withSlots(t *testing.T, capacity int) {
	t.Helper()
	t.Setenv("BOX_HOME", t.TempDir())
	old := slotCapacityFn
	slotCapacityFn = func() int { return capacity }
	t.Cleanup(func() { slotCapacityFn = old })
}

func newSandbox(t *testing.T, name string) {
	t.Helper()
	dir, err := sandbox.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Boxfile"), []byte("base: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStrictSandboxesGetTheirOwnSlots(t *testing.T) {
	withSlots(t, 2)
	strict := boxfile.Spec{Isolation: "strict"}
	for _, n := range []string{"a", "b", "c"} {
		newSandbox(t, n)
	}
	sa, err := idShift("a", strict)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := idShift("b", strict)
	if err != nil {
		t.Fatal(err)
	}
	if sa == sb {
		t.Fatalf("a and b share shift %d", sa)
	}
	if again, _ := idShift("a", strict); again != sa {
		t.Errorf("a's slot moved: %d then %d", sa, again)
	}
	// No third slot: refuse, with the fix, rather than share a UID.
	if _, err := idShift("c", strict); err == nil || !strings.Contains(err.Error(), "usermod --add-subuids") {
		t.Fatalf("third strict sandbox: %v", err)
	}
	// Destroying a frees its slot for c.
	if err := sandbox.Remove("a", false); err != nil {
		t.Fatal(err)
	}
	sc, err := idShift("c", strict)
	if err != nil || sc != sa {
		t.Errorf("c got %d, %v; want a's freed %d", sc, err, sa)
	}
}

func TestSlotsCountedWithoutABoxfile(t *testing.T) {
	withSlots(t, 2)
	strict := boxfile.Spec{Isolation: "strict"}
	newSandbox(t, "a")
	sa, err := idShift("a", strict)
	if err != nil {
		t.Fatal(err)
	}
	// a's Boxfile goes missing: its slot must still count as taken.
	dir, _ := sandbox.Dir("a")
	os.Remove(filepath.Join(dir, "Boxfile"))
	newSandbox(t, "b")
	sb, err := idShift("b", strict)
	if err != nil || sb == sa {
		t.Errorf("b got %d, %v; a holds %d", sb, err, sa)
	}
}

func TestStandardSandboxesTakeNoSlot(t *testing.T) {
	withSlots(t, 0)
	newSandbox(t, "plain")
	if shift, err := idShift("plain", boxfile.Spec{Isolation: "standard"}); err != nil || shift != 0 {
		t.Errorf("standard: %d, %v", shift, err)
	}
	if _, err := idShift("plain", boxfile.Spec{Isolation: "strict"}); err == nil || !strings.Contains(err.Error(), "none in /etc/subuid") {
		t.Errorf("strict without a range: %v", err)
	}
}

func TestSlotSurvivesRename(t *testing.T) {
	withSlots(t, 4)
	newSandbox(t, "old")
	strict := boxfile.Spec{Isolation: "strict"}
	before, err := idShift("old", strict)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	after, err := idShift("new", strict)
	if err != nil || after != before {
		t.Errorf("after rename: %d, %v; want %d", after, err, before)
	}
}
