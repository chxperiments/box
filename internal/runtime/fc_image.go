package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	shift := 0
	if s.Isolation == "strict" {
		shift = 1
	}
	// The agent is baked into the root disk and the snapshot, so a new
	// box means a new image; the key includes its hash.
	agentHash, err := selfHash()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "images", name,
		fmt.Sprintf("%s-a%s-s%d-r%d-c%d-%s", id[:12], agentHash, shift, s.RAMMiB, s.CPUs, roFlag(s))), nil
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
	return dir, nil
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
func fcDataDisk(name string) (string, error) {
	h, err := sandbox.Home()
	if err != nil {
		return "", err
	}
	if err := sandbox.ValidName(name); err != nil {
		return "", err
	}
	disk := filepath.Join(h, "disks", name+".ext4")
	if _, err := os.Stat(disk); err == nil {
		return disk, nil
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
		"sh", "-c", fmt.Sprintf("truncate -s %dG /out/%s && mkfs.ext4 -q -F -L data /out/%s", fcDataDiskGiB, tmp, tmp))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating %s's /data disk: %s", name, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(filepath.Join(filepath.Dir(disk), tmp), disk); err != nil {
		return "", err
	}
	return disk, nil
}

// fcSnapshot boots the image once, waits for its agent, pauses it and
// snapshots it into dir.
func fcSnapshot(name string, s boxfile.Spec, dir string) error {
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
		f.Truncate(int64(fcDataDiskGiB) << 30)
		f.Close()
	}
	defer os.Remove(placeholder)
	vm := "box-snap-" + name
	cmd, err := fcLaunchCmd(fcRun{
		Mode: "snapshot", Sandbox: name, VM: vm, Image: dir, Disk: placeholder,
		CPUs: s.CPUs, RAMMiB: s.RAMMiB, Strict: s.Isolation == "strict",
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
