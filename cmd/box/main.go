// Command box runs isolated, persistent sandboxes as microVMs.
package main

import (
	"os"
	"runtime"

	"box/internal/cli"
	"box/internal/guest"
)

func main() {
	// Inside a Firecracker guest the kernel starts box as init.
	if os.Getpid() == 1 && runtime.GOOS == "linux" {
		guest.Init()
		return
	}
	os.Exit(cli.Execute())
}
