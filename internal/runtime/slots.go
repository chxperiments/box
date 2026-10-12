package runtime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// Every strict sandbox runs as its own block of subordinate IDs. Inside
// podman's user namespace, ID 0 is you and IDs 1.. are your subordinate
// range, in /etc/subuid order. A sandbox's IDs 0-65535 need 65536 of them, so
// the range is cut into slots of that size, and slot k maps the sandbox onto
// namespace IDs 1+k*65536 onward. Two strict sandboxes therefore never share
// a UID: a guest that escapes its VM and its VMM lands in an account that
// owns nothing of yours and nothing of any other sandbox's.
//
// A sandbox's slot is recorded in its directory, so it survives rename and
// is freed by destroy. A fork shares its parent's slot, because the two share
// files and files have one owner.

const slotSize = 65536

// idShift is how far a sandbox's IDs move in podman's user namespace: 0
// under standard isolation (the sandbox's root is you), 1+slot*65536 under
// strict.
func idShift(name string, s boxfile.Spec) (int, error) {
	if s.Isolation != "strict" {
		return 0, nil
	}
	owner := name
	if parent, err := sandbox.Parent(name); err == nil {
		owner = parent
	}
	slot, err := strictSlot(owner)
	if err != nil {
		return 0, err
	}
	return 1 + slot*slotSize, nil
}

// idMap is the --uidmap/--gidmap value for a shift: the sandbox's 65536 IDs
// onto namespace IDs shift.. -- never onto you, who are ID 0.
func idMap(shift int) string { return fmt.Sprintf("0:%d:%d", shift, slotSize) }

func slotPath(name string) (string, error) {
	dir, err := sandbox.Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "slot"), nil
}

func readSlot(name string) (int, bool) {
	p, err := slotPath(name)
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// strictSlot returns the sandbox's slot, allocating the lowest free one the
// first time. Allocation holds a lock, so concurrent box processes (a warm
// pool refilling, two runs) never hand out the same slot twice.
func strictSlot(name string) (int, error) {
	if n, ok := readSlot(name); ok {
		return n, nil
	}
	home, err := sandbox.Home()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return 0, err
	}
	lock, err := os.OpenFile(filepath.Join(home, "slots.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if n, ok := readSlot(name); ok { // allocated while we waited
		return n, nil
	}
	capacity := slotCapacityFn()
	// Every sandbox directory counts, Boxfile or not: a slot recorded
	// anywhere is taken, so no two sandboxes can ever be handed the same one.
	entries, err := os.ReadDir(filepath.Join(home, "sandboxes"))
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	used := map[int]bool{}
	for _, e := range entries {
		if !e.IsDir() || sandbox.ValidName(e.Name()) != nil {
			continue
		}
		if n, ok := readSlot(e.Name()); ok {
			used[n] = true
		}
	}
	for n := 0; n < capacity; n++ {
		if used[n] {
			continue
		}
		p, err := slotPath(name)
		if err != nil {
			return 0, err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(n)+"\n"), 0o600); err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, p); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, slotsExhausted(capacity, len(used))
}

func slotsExhausted(capacity, used int) error {
	if capacity == 0 {
		return fmt.Errorf("isolation: strict needs at least %d subordinate IDs for %s and has none in /etc/subuid and /etc/subgid; see box doctor",
			slotSize, currentUser())
	}
	want := 64 * slotSize
	start := freeSubordinateStart()
	return fmt.Errorf("every strict sandbox needs its own %d subordinate IDs, and all %d slot(s) in your range are taken by %d strict sandbox(es).\n"+
		"Add a range no other user has, then let podman pick it up:\n"+
		"  sudo usermod --add-subuids %d-%d --add-subgids %d-%d %s\n"+
		"  podman system migrate    # stops running containers\n"+
		"That makes room for 64 more strict sandboxes.",
		slotSize, capacity, used, start, start+want-1, start, start+want-1, currentUser())
}

// subRange is one line of /etc/subuid or /etc/subgid for this user.
type subRange struct{ start, count int }

// subordinateRanges returns this user's ranges from path, in file order --
// the order podman lays them out in its namespace.
func subordinateRanges(path string) []subRange {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	user, uid := currentUser(), strconv.Itoa(os.Getuid())
	var out []subRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(p) != 3 || (p[0] != user && p[0] != uid) {
			continue
		}
		start, err1 := strconv.Atoi(p[1])
		count, err2 := strconv.Atoi(p[2])
		if err1 == nil && err2 == nil && count > 0 {
			out = append(out, subRange{start, count})
		}
	}
	return out
}

func rangeTotal(rs []subRange) int {
	n := 0
	for _, r := range rs {
		n += r.count
	}
	return n
}

// slotCapacityFn is slotCapacity, replaceable in tests.
var slotCapacityFn = slotCapacity

// slotCapacity is how many strict sandboxes the subordinate ranges hold:
// whichever of the UID and GID ranges is smaller decides.
func slotCapacity() int {
	n := min(rangeTotal(subordinateRanges("/etc/subuid")), rangeTotal(subordinateRanges("/etc/subgid")))
	return n / slotSize
}

// hostID maps an ID in podman's namespace to the host: 0 is you, 1.. walk
// your subordinate ranges in order.
func hostID(nsID int, self int, rs []subRange) (int, bool) {
	if nsID == 0 {
		return self, true
	}
	i := nsID - 1
	for _, r := range rs {
		if i < r.count {
			return r.start + i, true
		}
		i -= r.count
	}
	return 0, false
}

// freeSubordinateStart suggests where a new range can begin: past the end of
// every range any user has, rounded up to a slot.
func freeSubordinateStart() int {
	end := 100000
	for _, path := range []string{"/etc/subuid", "/etc/subgid"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.Split(strings.TrimSpace(line), ":")
			if len(p) != 3 {
				continue
			}
			s, err1 := strconv.Atoi(p[1])
			c, err2 := strconv.Atoi(p[2])
			if err1 == nil && err2 == nil && s+c > end {
				end = s + c
			}
		}
	}
	return (end + slotSize - 1) / slotSize * slotSize
}

// releaseData hands a strict sandbox's kept /data back to you when the
// sandbox is destroyed without --data. Its slot is freed with it, and the
// next strict sandbox given that slot must not own the files.
func releaseData(name string) error {
	data, err := sandbox.DataDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(data); err != nil {
		return nil
	}
	if mine, err := sandbox.OwnedByMe(data); err != nil || mine {
		return err
	}
	if out, err := exec.Command("podman", "unshare", "chown", "-R", "0:0", data).CombinedOutput(); err != nil {
		return fmt.Errorf("handing %s's /data back to you: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReleaseData is releaseData for the CLI's destroy and nuke.
func ReleaseData(name string) error { return releaseData(name) }
