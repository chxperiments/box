package runtime

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// The krun backend boots from an exported copy of the image rather than
// podman's storage: a plain directory crun can use as a root. It is made
// once per image and isolation mode, under
//
//	~/.box/rootfs/<name>/<image id>-s<shift>/      the files
//	~/.box/rootfs/<name>/<image id>-s<shift>.json  the image's config
//
// and every VM overlays it, so VMs never write it. The export runs inside
// podman's user namespace, where file owners can be set to any mapped ID.
// shift is how far owners move: 0 under standard isolation (container root
// is you), 1 under strict (container root is the first subordinate UID, as
// the VM's own mapping says).

// rootfsFor returns the exported rootfs for a sandbox, exporting it first if
// this image has not been yet.
func rootfsFor(name string, s boxfile.Spec) (dir string, img imageConfig, err error) {
	id, err := imageID(name)
	if err != nil {
		return "", img, err
	}
	shift := 0
	if s.Isolation == "strict" {
		shift = 1
	}
	base, err := sandbox.RootfsDir(name)
	if err != nil {
		return "", img, err
	}
	dir = filepath.Join(base, fmt.Sprintf("%s-s%d", id[:12], shift))
	b, err := os.ReadFile(dir + ".json")
	if err == nil {
		if err := json.Unmarshal(b, &img); err == nil {
			return dir, img, nil
		}
	}
	self, err := os.Executable()
	if err != nil {
		return "", img, err
	}
	cmd := inPodmanNS(self, "__export", sandbox.ImageTag(name), dir, fmt.Sprint(shift))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", img, fmt.Errorf("exporting %s's image: %s", name, strings.TrimSpace(string(out)))
	}
	b, err = os.ReadFile(dir + ".json")
	if err != nil {
		return "", img, err
	}
	return dir, img, json.Unmarshal(b, &img)
}

// imageID is the built image's id, recorded by Build so a launch does not
// pay for `podman image inspect` (~250ms); inspected when the record is
// missing, e.g. an image built by an older box.
func imageID(name string) (string, error) {
	if b, err := os.ReadFile(imageIDPath(name)); err == nil && len(strings.TrimSpace(string(b))) >= 12 {
		return strings.TrimSpace(string(b)), nil
	}
	return RecordImageID(name)
}

func imageIDPath(name string) string {
	d, _ := sandbox.Dir(name)
	return filepath.Join(d, "image-id")
}

// RecordImageID notes the current image's id beside the Boxfile.
func RecordImageID(name string) (string, error) {
	out, err := exec.Command("podman", "image", "inspect", "--format", "{{.Id}}", sandbox.ImageTag(name)).Output()
	if err != nil {
		return "", fmt.Errorf("no image for %s; build it first: box build %s", name, name)
	}
	id := strings.TrimSpace(string(out))
	os.WriteFile(imageIDPath(name), []byte(id+"\n"), 0o644) // best effort: a miss costs one inspect
	return id, nil
}

// RemoveRootfs deletes a sandbox's exported images. Running VMs have them
// mounted as their lower layer, so callers remove the VMs first.
func RemoveRootfs(name string) error {
	dir, err := sandbox.RootfsDir(name)
	if err != nil {
		return err
	}
	if out, err := exec.Command("podman", "unshare", "rm", "-rf", "--", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("removing %s: %s", dir, strings.TrimSpace(string(out)))
	}
	return nil
}

// ExportRootfs is the `box __export` command: it writes image's filesystem
// to dest with owners shifted, and the image's config beside it. It must run
// inside podman unshare.
func ExportRootfs(image, dest string, shift int) error {
	if os.Getenv("_CONTAINERS_USERNS_CONFIGURED") == "" {
		return errors.New("__export runs inside podman unshare")
	}
	cfg, err := inspectImage(image)
	if err != nil {
		return err
	}
	ctr, err := exec.Command("podman", "create", "--pull=never", image).Output()
	if err != nil {
		return fmt.Errorf("podman create: %w", err)
	}
	id := strings.TrimSpace(string(ctr))
	defer exec.Command("podman", "rm", "-f", id).Run()

	tmp := dest + ".partial"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	export := exec.Command("podman", "export", id)
	stream, err := export.StdoutPipe()
	if err != nil {
		return err
	}
	if err := export.Start(); err != nil {
		return err
	}
	if err := untar(stream, tmp, shift); err != nil {
		export.Process.Kill()
		export.Wait()
		os.RemoveAll(tmp)
		return err
	}
	if err := export.Wait(); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("podman export: %w", err)
	}
	// Container root must own and be able to enter the root directory.
	if err := os.Lchown(tmp, shift, shift); err != nil {
		return err
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(dest+".json.partial", b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return os.Rename(dest+".json.partial", dest+".json")
}

func inspectImage(image string) (imageConfig, error) {
	out, err := exec.Command("podman", "image", "inspect", "--format", "{{json .Config}}", image).Output()
	if err != nil {
		return imageConfig{}, fmt.Errorf("podman image inspect %s: %w", image, err)
	}
	var raw struct {
		Env        []string
		Entrypoint []string
		Cmd        []string
		WorkingDir string
		User       string
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return imageConfig{}, err
	}
	return imageConfig{Env: raw.Env, Entrypoint: raw.Entrypoint, Cmd: raw.Cmd, WorkingDir: raw.WorkingDir, User: raw.User}, nil
}

// untar extracts a container filesystem. Owners shift by shift. Device
// nodes are skipped: crun populates /dev itself, and nothing else in an image
// should be one. Hard links are made as links; a link to a skipped entry is
// skipped too.
func untar(r io.Reader, dest string, shift int) error {
	tr := tar.NewReader(r)
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	type late struct {
		path string
		mode os.FileMode
	}
	var dirs []late // directory modes are applied last, so ro dirs can be filled
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := filepath.Clean(h.Name)
		if rel == "." || rel == "/" {
			continue
		}
		rel = strings.TrimPrefix(rel, "/")
		// Images carry overlay whiteouts for files deleted in upper layers;
		// podman export has already applied them, but be safe.
		if strings.Contains(rel, ".wh.") {
			continue
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		mode := os.FileMode(h.Mode) & os.ModePerm
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			dirs = append(dirs, late{rel, mode | os.FileMode(h.Mode)&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)})
		case tar.TypeReg:
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			if err := root.Chmod(rel, mode|os.FileMode(h.Mode)&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			root.Remove(rel)
			if err := root.Symlink(h.Linkname, rel); err != nil {
				return err
			}
		case tar.TypeLink:
			target := strings.TrimPrefix(filepath.Clean(h.Linkname), "/")
			root.Remove(rel)
			if err := root.Link(target, rel); err != nil {
				continue // the target was a device or otherwise skipped
			}
			continue // a hard link shares its target's owner and mode
		default:
			continue // devices, fifos, sockets
		}
		if err := root.Lchown(rel, h.Uid+shift, h.Gid+shift); err != nil {
			return fmt.Errorf("chown %s: %w", rel, err)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := root.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}
