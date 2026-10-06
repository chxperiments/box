package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"box/internal/runtime"
	"box/internal/sandbox"
)

func dataCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "data", Short: "move files in and out of /data", GroupID: groupState,
		Long: "Copies files between a host directory and a sandbox's /data. On the\n" +
			"firecracker backend /data is a disk, and this is how files get in and\n" +
			"out; on the others it works the same way on the host directory.\n\n" +
			"Exported files are guest-written: symlinks are kept as symlinks and\n" +
			"never followed, devices are skipped, setuid bits dropped.",
		Args: cobra.NoArgs,
	}
	c.AddCommand(&cobra.Command{
		Use: "export <name> <dir>", Short: "copy /data out to a new or empty directory",
		Args: cobra.ExactArgs(2), ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name, dest := args[0], args[1]
			s, err := loadSpec(name)
			if err != nil {
				return err
			}
			if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
				return fmt.Errorf("%s is not empty; export into a new or empty directory", dest)
			}
			pr, pw := io.Pipe()
			done := make(chan error, 1)
			go func() {
				n, err := sandbox.SafeUntar(pr, dest)
				if n > 0 {
					fmt.Fprintf(os.Stderr, "box: skipped %s that cannot be exported safely\n", plural(n, "entry", "entries"))
				}
				pr.CloseWithError(err)
				done <- err
			}()
			err = runtime.ExportData(name, s, pw)
			pw.CloseWithError(err)
			if uerr := <-done; err == nil {
				err = uerr
			}
			if err != nil {
				return err
			}
			fmt.Printf("exported %s's /data to %s\n", name, dest)
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use: "import <name> <dir>", Short: "copy a directory's contents into /data",
		Args: cobra.ExactArgs(2), ValidArgsFunction: completeName,
		RunE: func(_ *cobra.Command, args []string) error {
			name, src := args[0], args[1]
			s, err := loadSpec(name)
			if err != nil {
				return err
			}
			if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", src)
			}
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(sandbox.WriteTar(pw, src)) }()
			if err := runtime.ImportData(name, s, pr); err != nil {
				return err
			}
			fmt.Printf("imported %s into %s's /data\n", src, name)
			return nil
		},
	})
	return c
}

// tarOutCmd and tarInCmd are the two ends of data export/import, run inside
// a guest or inside podman's namespace.
func tarOutCmd() *cobra.Command {
	return &cobra.Command{
		Use: "__tarout <dir>", Hidden: true, Args: cobra.ExactArgs(1), ValidArgsFunction: completeNothing,
		RunE: func(_ *cobra.Command, args []string) error { return sandbox.WriteTar(os.Stdout, args[0]) },
	}
}

func tarInCmd() *cobra.Command {
	return &cobra.Command{
		Use: "__tarin <dir>", Hidden: true, Args: cobra.ExactArgs(1), ValidArgsFunction: completeNothing,
		RunE: func(_ *cobra.Command, args []string) error {
			_, err := sandbox.SafeUntar(os.Stdin, args[0])
			return err
		},
	}
}
