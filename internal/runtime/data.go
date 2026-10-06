package runtime

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// ExportData streams a sandbox's /data as a tar archive to w. On the podman
// and krun backends /data is a host directory and is read directly (through
// podman's namespace if a strict sandbox owns it); on the firecracker
// backend it is a disk, read by the guest. Either way the archive is
// untrusted, guest-written content, and must be unpacked with SafeUntar.
func ExportData(name string, s boxfile.Spec, w io.Writer) error {
	if sandbox.IsFork(name) {
		return fmt.Errorf("%s is a fork; apply it and export its parent", name)
	}
	if s.Backend == "firecracker" {
		return guestData(name, s, []string{"/.box/box", "__tarout", "/data"}, nil, w)
	}
	data, err := sandbox.DataDir(name)
	if err != nil {
		return err
	}
	if mine, err := sandbox.OwnedByMe(data); err == nil && !mine {
		return hostHelper(nil, w, "__tarout", data)
	}
	return sandbox.WriteTar(w, data)
}

// ImportData unpacks a tar archive from r into a sandbox's /data, merging
// with what is there.
func ImportData(name string, s boxfile.Spec, r io.Reader) error {
	if sandbox.IsFork(name) {
		return fmt.Errorf("%s is a fork; import into its parent", name)
	}
	if s.Backend == "firecracker" {
		return guestData(name, s, []string{"/.box/box", "__tarin", "/data"}, r, io.Discard)
	}
	data, err := sandbox.DataDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if mine, err := sandbox.OwnedByMe(data); err == nil && !mine {
		return hostHelper(r, io.Discard, "__tarin", data)
	}
	_, err = sandbox.SafeUntar(r, data)
	return err
}

// guestData runs a tar helper inside the sandbox: in the running VM if it is
// up, otherwise in a fresh one.
func guestData(name string, s boxfile.Spec, argv []string, in io.Reader, out io.Writer) error {
	streams := Streams{Stdin: in, Stdout: out, Stderr: os.Stderr}
	var err error
	if sandbox.IsUp(name) {
		err = Exec(name, s, argv, streams)
	} else {
		err = Run(name, s, argv, streams)
	}
	if code := ExitCode(err); code > 0 {
		return fmt.Errorf("%s inside the sandbox exited %d", argv[1], code)
	}
	return err
}

// hostHelper runs a tar helper inside podman's namespace, where a strict
// sandbox's files are reachable.
func hostHelper(in io.Reader, out io.Writer, verb, dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := inPodmanNS(self, verb, dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if ok := asExit(err, &ee); ok {
			return fmt.Errorf("%s %s failed", verb, dir)
		}
		return err
	}
	return nil
}

func asExit(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
