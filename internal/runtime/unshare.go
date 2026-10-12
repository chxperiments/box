package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// inPodmanNS returns a command that runs argv inside rootless podman's user
// and mount namespace: where the overlays are mounted, and where the
// subordinate UID range is addressable. `podman unshare` does exactly this
// but costs ~240ms to start; its namespace is kept alive by a pause process
// that nsenter can join in a few milliseconds, so that is used whenever the
// pause process exists. The environment podman unshare would set is set the
// same way, so helpers that check for it behave identically.
func inPodmanNS(argv ...string) *exec.Cmd {
	// Already inside it -- `box __fc` and the other helpers run there --
	// the namespace cannot be entered again: setns into the user namespace
	// one is in fails with EINVAL. Run argv directly. Without this, a
	// Firecracker VM torn down from inside never got its crun kill, and every
	// run and snapshot left its VM running.
	if os.Getenv("_CONTAINERS_USERNS_CONFIGURED") != "" {
		return exec.Command(argv[0], argv[1:]...)
	}
	if pid := pausePID(); pid > 0 {
		args := []string{"--user", "--mount", "--preserve-credentials", "--target", strconv.Itoa(pid), "--"}
		cmd := exec.Command("nsenter", append(args, argv...)...)
		cmd.Env = append(os.Environ(), "_CONTAINERS_USERNS_CONFIGURED=done",
			"_CONTAINERS_ROOTLESS_UID="+strconv.Itoa(os.Getuid()))
		return cmd
	}
	return exec.Command("podman", append([]string{"unshare"}, argv...)...)
}

// pausePID is podman's rootless pause process, or 0 if there is none yet.
func pausePID() int {
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(run, "libpod", "tmp", "pause.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	// Alive, and really the pause process (catatonit) rather than a reused pid.
	if err := syscall.Kill(pid, 0); err != nil {
		return 0
	}
	if comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err != nil || !strings.Contains(string(comm), "catatonit") {
		return 0
	}
	return pid
}
