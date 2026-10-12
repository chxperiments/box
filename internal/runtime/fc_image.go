package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// A Firecracker image is a directory per built image and isolation mode:
//
//	~/.box/firecracker/images/<name>/<image id>-s<shift>/
//	  rootfs.ext4   the image's files, plus the box agent as /.box/box
//	  snap.state    a snapshot of a VM booted from it, paused once its agent answered
//	  snap.mem      that VM's memory
//	  token         the agent token baked into the snapshot (rotated on restore)
//
// Every run restores the snapshot: the kernel boot and the agent start are
// paid once, at build, never per run.

func fcImageDir(name string, s boxfile.Spec) (string, error) {
	id, err := imageID(name)
	if err != nil {
		return "", err
	}
	dir, err := fcDir()
	if err != nil {
		return "", err
	}
	shift, err := idShift(name, s)
	if err != nil {
		return "", err
	}
	// The agent is baked into the root disk and the snapshot, so a new
	// box means a new image; the key includes its hash.
	agentHash, err := selfHash()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "images", name,
		fmt.Sprintf("%s-a%s-s%d-r%d-c%d-d%d-%s", id[:12], agentHash, shift, s.RAMMiB, s.CPUs, fcDataBytes(s)>>20, roFlag(s))), nil
}

func roFlag(s boxfile.Spec) string {
	if s.ReadOnlyRootfs {
		return "ro"
	}
	return "rw"
}

var selfHashCache string

// selfHash is a short hash of the running box binary.
func selfHash() (string, error) {
	if selfHashCache != "" {
		return selfHashCache, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	selfHashCache = hex.EncodeToString(h.Sum(nil))[:12]
	return selfHashCache, nil
}

// fcImage returns the image directory, building the root disk and the
// snapshot first if this image has not been booted yet.
func fcImage(name string, s boxfile.Spec) (string, error) {
	dir, err := fcImageDir(name, s)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "snap.mem")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "rootfs.ext4")); err != nil {
		if err := fcRootfs(name, s, dir); err != nil {
			return "", err
		}
	}
	if err := fcSnapshot(name, s, dir); err != nil {
		return "", err
	}
	removeStaleImages(dir)
	return dir, nil
}

// removeStaleImages deletes a sandbox's other images. Each is a rootfs and a
// full memory snapshot (its RAM in size), keyed on everything that shapes it
// -- the image, box's own build, isolation, RAM, CPUs, /data's size -- so
// every change, a box upgrade included, makes a new one; without this the
// old ones stayed forever. A VM still restored from one keeps its files
// open, and Linux keeps them until it exits.
func removeStaleImages(current string) {
	parent := filepath.Dir(current)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	var stale []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != filepath.Base(current) {
			stale = append(stale, filepath.Join(parent, e.Name()))
		}
	}
	if len(stale) > 0 {
		inPodmanNS(append([]string{"rm", "-rf", "--"}, stale...)...).Run()
	}
}

// fcRootfs writes rootfs.ext4: the image's files exported by podman and
// written into an ext4 filesystem by the helper image, with the agent binary
// and the image's environment added under /.box.
func fcRootfs(name string, s boxfile.Spec, dir string) error {
	if err := fcTools(); err != nil {
		return err
	}
	agentDir, err := installAgent()
	if err != nil {
		return err
	}
	img, err := inspectImage(sandbox.ImageTag(name))
	if err != nil {
		return err
	}
	env := append([]string{}, img.Env...)
	for _, k := range s.EnvKeys() {
		env = append(env, k+"="+s.Env[k])
	}
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte(strings.Join(env, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	ctr, err := exec.Command("podman", "create", "--pull=never", sandbox.ImageTag(name), "true").Output()
	if err != nil {
		return fmt.Errorf("podman create %s: %w", name, err)
	}
	id := strings.TrimSpace(string(ctr))
	defer exec.Command("podman", "rm", "-f", id).Run()

	// The helper runs as root in its own container, so the tar keeps the
	// image's owners and modes, and mkfs.ext4 -d writes them into the disk.
	// The disk is sized to the files plus headroom; it is read-only in the
	// guest, whose writes go to a RAM overlay.
	script := `set -e
mkdir /r && tar -C /r -xf -
mkdir -p /r/.box /r/.rw /r/.root /r/data
cp /agent/box /r/.box/box && chmod 0755 /r/.box/box
cp /out/env /r/.box/env
size=$(( $(du -sm /r | cut -f1) * 13 / 10 + 64 ))
mkfs.ext4 -q -F -L root -d /r /out/rootfs.ext4.partial ${size}M
mv /out/rootfs.ext4.partial /out/rootfs.ext4`
	export := exec.Command("podman", "export", id)
	build := exec.Command("podman", "run", "--rm", "-i", "--pull=never", "--network=none",
		"-v", agentDir+":/agent:ro", "-v", dir+":/out", fcToolsImage, "sh", "-c", script)
	pipe, err := export.StdoutPipe()
	if err != nil {
		return err
	}
	build.Stdin = pipe
	var out strings.Builder
	build.Stdout, build.Stderr = &out, &out
	if err := export.Start(); err != nil {
		return err
	}
	if err := build.Run(); err != nil {
		export.Process.Kill()
		export.Wait()
		return fmt.Errorf("building %s's root disk: %s", name, strings.TrimSpace(out.String()))
	}
	return export.Wait()
}

// fcDataDisk returns the sandbox's /data disk, creating an empty ext4
// filesystem on first use. It is sparse: its size is a ceiling, not a cost.
// fcDataDiskPath is where a sandbox's /data disk lives, without creating it.
func fcDataDiskPath(name string) (string, error) {
	h, err := sandbox.Home()
	if err != nil {
		return "", err
	}
	if err := sandbox.ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(h, "disks", name+".ext4"), nil
}

// ResetFirecrackerData deletes a sandbox's /data disk; the next boot makes
// an empty one.
func ResetFirecrackerData(name string) error {
	disk, err := fcDataDiskPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(disk); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// FirecrackerDataExists reports whether a sandbox has a /data disk.
func FirecrackerDataExists(name string) bool {
	disk, err := fcDataDiskPath(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(disk)
	return err == nil
}

// fcDataBytes is the size of a sandbox's /data disk: data_size, or 8 GiB.
// The disk is sparse, so only what /data holds takes space.
func fcDataBytes(s boxfile.Spec) int64 {
	if n, err := s.DataBytes(); err == nil && n > 0 {
		return n
	}
	return int64(fcDataDiskGiB) << 30
}

// fcDataDisk returns the sandbox's /data disk, created at size the first
// time. The disk is data_size's limit: the guest's kernel refuses a write
// that does not fit. A disk smaller than size is grown, offline, keeping
// its files; one larger is refused, since shrinking a filesystem in place
// is how data is lost.
func fcDataDisk(name string, size int64) (string, error) {
	disk, err := fcDataDiskPath(name)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(disk); err == nil {
		switch {
		case fi.Size() == size:
			return disk, nil
		case fi.Size() > size:
			return "", fmt.Errorf("%s's /data disk is %s, larger than its data_size %s, and a disk cannot shrink in place: "+
				"box data export %s ./%s-data && box reset %s -y, then box data import %s ./%s-data",
				name, humanBytes(fi.Size()), humanBytes(size), name, name, name, name, name)
		}
		// Never resize a disk a running VM has mounted.
		if holder, err := os.ReadFile(diskLockPath(name)); err == nil && fcAlive(string(holder)) {
			return "", fmt.Errorf("%s's /data disk is in use by %s; bring it down to grow /data to its new data_size",
				name, strings.TrimSpace(string(holder)))
		}
		return disk, growDisk(disk, size)
	}
	if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
		return "", err
	}
	if err := fcTools(); err != nil {
		return "", err
	}
	tmp := name + ".ext4.partial"
	cmd := exec.Command("podman", "run", "--rm", "--pull=never", "--network=none",
		"-v", filepath.Dir(disk)+":/out", fcToolsImage,
		"sh", "-c", fmt.Sprintf("truncate -s %d /out/%s && mkfs.ext4 -q -F -L data /out/%s", size, tmp, tmp))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating %s's /data disk: %s", name, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(filepath.Join(filepath.Dir(disk), tmp), disk); err != nil {
		return "", err
	}
	return disk, nil
}

// growDisk enlarges an unmounted ext4 disk and its filesystem. A strict
// sandbox's disk belongs to its UID; it is given back to you for the resize
// and handed to the sandbox again at its next start.
func growDisk(disk string, size int64) error {
	if err := fcTools(); err != nil {
		return err
	}
	if out, err := exec.Command("podman", "unshare", "chown", "0:0", disk).CombinedOutput(); err != nil {
		return fmt.Errorf("growing %s: %s", disk, strings.TrimSpace(string(out)))
	}
	// Check for the tools first, and put the size back if the resize fails:
	// a grow either happens whole or leaves the disk as it was.
	cmd := exec.Command("podman", "run", "--rm", "--pull=never", "--network=none",
		"-v", filepath.Dir(disk)+":/out", fcToolsImage, "sh", "-c", `
d=/out/$1
command -v resize2fs >/dev/null || { echo "resize2fs is missing from the tools image"; exit 1; }
orig=$(stat -c %s "$d")
truncate -s "$2" "$d" || exit 1
e2fsck -fy "$d" >/dev/null 2>&1
if [ $? -gt 1 ] || ! resize2fs "$d" >/dev/null 2>&1; then
  truncate -s "$orig" "$d"
  echo "the filesystem could not be grown; the disk is unchanged"
  exit 1
fi`, "sh", filepath.Base(disk), strconv.FormatInt(size, 10))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("growing %s to %s: %s", disk, humanBytes(size), strings.TrimSpace(string(out)))
	}
	return nil
}

// fcSnapshot boots the image once, waits for its agent, pauses it and
// snapshots it into dir.
func fcSnapshot(name string, s boxfile.Spec, dir string) error {
	shift, err := idShift(name, s)
	if err != nil {
		return err
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600); err != nil {
		return err
	}
	// A placeholder /data disk of the real size: the snapshot records the
	// device, never its contents, because the guest does not mount it until
	// a restored copy is prepared.
	placeholder := filepath.Join(dir, "placeholder.ext4")
	if f, err := os.Create(placeholder); err == nil {
		f.Truncate(fcDataBytes(s)) // the snapshot's disk must be the size every restore attaches
		f.Close()
	}
	defer os.Remove(placeholder)
	vm := "box-snap-" + name
	cmd, err := fcLaunchCmd(fcRun{
		Mode: "snapshot", Sandbox: name, VM: vm, Image: dir, Disk: placeholder,
		CPUs: s.CPUs, RAMMiB: s.RAMMiB, Shift: shift,
		ReadOnly: s.ReadOnlyRootfs, BootToken: token, Spec: s,
	})
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("snapshotting %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveFirecracker deletes a sandbox's Firecracker images and /data disk.
func RemoveFirecracker(name string, withData bool) {
	if dir, err := fcDir(); err == nil {
		inPodmanNS("rm", "-rf", "--", filepath.Join(dir, "images", name)).Run()
	}
	if withData {
		if h, err := sandbox.Home(); err == nil {
			os.Remove(filepath.Join(h, "disks", name+".ext4"))
		}
	}
}
