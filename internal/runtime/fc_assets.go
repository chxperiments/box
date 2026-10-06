package runtime

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"box/internal/sandbox"
)

// Firecracker and its guest kernel are pinned, downloaded on first use and
// checked against these hashes before they are ever run: the VMM is the
// security boundary, so it must be exactly the build that was tested.
const (
	fcVersion     = "v1.17.0"
	fcTarballURL  = "https://github.com/firecracker-microvm/firecracker/releases/download/" + fcVersion + "/firecracker-" + fcVersion + "-x86_64.tgz"
	fcBinarySHA   = "99ad0f5cd0514a88aad0e9ae8cfdb3cc3b4ab9d190e1194602406c786b5de7a5"
	fcKernelName  = "vmlinux-6.1.155"
	fcKernelURL   = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/" + fcKernelName
	fcKernelSHA   = "e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"
	fcToolsImage  = "localhost/box/fc-tools:latest"
	fcDataDiskGiB = 8 // sparse: only what /data actually holds is stored
)

// fcDir holds Firecracker's assets and every image's disk and snapshot.
func fcDir() (string, error) {
	h, err := sandbox.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "firecracker"), nil
}

// fcAssets returns the Firecracker binary and kernel, fetching them if this
// is the first use. A file whose hash does not match is never used.
func fcAssets() (binary, kernel string, err error) {
	if goruntime.GOARCH != "amd64" {
		return "", "", fmt.Errorf("the firecracker backend is x86_64-only for now")
	}
	dir, err := fcDir()
	if err != nil {
		return "", "", err
	}
	binary = filepath.Join(dir, "bin", "firecracker-"+fcVersion)
	kernel = filepath.Join(dir, "kernel", fcKernelName)
	if !hashOK(binary, fcBinarySHA) {
		if err := fetchFirecracker(binary); err != nil {
			return "", "", err
		}
	}
	if !hashOK(kernel, fcKernelSHA) {
		if err := fetchVerified(fcKernelURL, kernel, fcKernelSHA, 0o644); err != nil {
			return "", "", err
		}
	}
	return binary, kernel, nil
}

func hashOK(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

// fetchVerified downloads url to dest, keeping it only if its SHA-256 is want.
func fetchVerified(url, dest, want string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return writeVerified(resp.Body, dest, want, mode)
}

func writeVerified(r io.Reader, dest, want string, mode os.FileMode) error {
	tmp := dest + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("%s: sha256 %s, want %s; refusing to use it", filepath.Base(dest), got, want)
	}
	return os.Rename(tmp, dest)
}

// fetchFirecracker takes the firecracker binary out of the release tarball.
func fetchFirecracker(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(fcTarballURL)
	if err != nil {
		return fmt.Errorf("downloading firecracker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading firecracker: %s", resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("firecracker %s: binary not found in the release", fcVersion)
		}
		if err != nil {
			return err
		}
		if strings.HasSuffix(h.Name, "/firecracker-"+fcVersion+"-x86_64") {
			return writeVerified(tr, dest, fcBinarySHA, 0o755)
		}
	}
}

// fcTools builds the small helper image used to make ext4 disks, which needs
// mkfs.ext4 and no root: the host may have neither e2fsprogs nor sudo.
func fcTools() error {
	if exec.Command("podman", "image", "exists", fcToolsImage).Run() == nil {
		return nil
	}
	cmd := exec.Command("podman", "build", "-q", "-t", fcToolsImage, "-f", "-", ".")
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader("FROM docker.io/library/alpine:latest\nRUN apk add --no-cache e2fsprogs tar\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building %s: %s", fcToolsImage, strings.TrimSpace(string(out)))
	}
	return nil
}
