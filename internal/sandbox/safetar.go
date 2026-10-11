package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WriteTar streams dir as a tar archive. It never follows a symlink: links
// are archived as links, so a source tree cannot pull in files from outside
// itself. Only directories, regular files and symlinks are written.
func WriteTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		link := ""
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		case fi.IsDir(), fi.Mode().IsRegular():
		default:
			return nil // devices, fifos, sockets do not travel
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.Mode &^= int64(fs.ModeSetuid | fs.ModeSetgid)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// SafeUntar extracts an archive the guest produced into dest. The archive is
// untrusted: every operation goes through an os.Root, so neither an entry
// like ../../x nor a symlink planted by an earlier entry can place a file
// outside dest. Devices, fifos and sockets are skipped, setuid and setgid
// bits are dropped, and files belong to whoever extracts them. An entry that
// cannot be placed inside dest is skipped, not fatal, so one hostile entry
// cannot hold back the rest. It returns how many entries it skipped.
func SafeUntar(r io.Reader, dest string) (skipped int, err error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return skipped, nil
		}
		if err != nil {
			return skipped, err
		}
		rel := strings.TrimPrefix(filepath.Clean("/"+h.Name), "/")
		if rel == "" {
			continue
		}
		if err := extractEntry(root, tr, h, rel); err != nil {
			skipped++
		}
	}
}

var errSkip = errors.New("entry type not extracted")

// extractEntry places one entry. Any error, including an os.Root refusal for
// a path that would leave dest, means the entry was not extracted.
func extractEntry(root *os.Root, tr *tar.Reader, h *tar.Header, rel string) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	perm := fs.FileMode(h.Mode) & fs.ModePerm
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.Mkdir(rel, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		// Keep it enterable for us whatever the guest set.
		return root.Chmod(rel, perm|0o700)
	case tar.TypeReg:
		if fi, err := root.Lstat(rel); err == nil && !fi.Mode().IsRegular() {
			if err := root.RemoveAll(rel); err != nil {
				return err
			}
		}
		f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
		return root.Chmod(rel, perm)
	case tar.TypeSymlink:
		if err := root.RemoveAll(rel); err != nil {
			return err
		}
		return root.Symlink(h.Linkname, rel)
	default:
		return errSkip // hard links, devices, fifos
	}
}

// ExtractArchive unpacks a gzip-compressed tar into dest, which must be an
// empty directory. Unlike SafeUntar it is all or nothing: the first entry it
// cannot place exactly is an error, and the caller discards dest. It is the
// reader for restore, whose archives may come from anywhere, so the format is
// fixed to gzip+tar in Go rather than left to whichever tar the host ships.
//
// Every write goes through an os.Root on dest. On top of that, entry names
// and hard link targets must be relative and stay inside the archive root,
// no entry may pass through or replace a symlink an earlier entry planted,
// and only directories, regular files, symlinks and hard links are accepted.
// Symlink targets are kept as written: they are resolved by the guest, inside
// its own filesystem, and nothing here ever follows one. Setuid, setgid and
// sticky bits are dropped, and everything belongs to whoever extracts it.
func ExtractArchive(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return errors.New("not a gzip-compressed archive")
	}
	defer gz.Close()
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()

	type dirMode struct {
		rel  string
		mode fs.FileMode
		mod  time.Time
	}
	var dirs []dirMode
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		rel, err := archivePath(h.Name)
		if err != nil {
			return err
		}
		if rel == "." {
			if h.Typeflag != tar.TypeDir {
				return fmt.Errorf("entry %q: the archive root must be a directory", h.Name)
			}
			dirs = append(dirs, dirMode{rel, fs.FileMode(h.Mode), h.ModTime})
			continue
		}
		if err := noSymlinkOnPath(root, rel); err != nil {
			return fmt.Errorf("entry %q: %w", h.Name, err)
		}
		if fi, err := root.Lstat(rel); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("entry %q would replace a symlink", h.Name)
		}
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
		}
		perm := fs.FileMode(h.Mode) & fs.ModePerm
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
			// Applied once everything is in, or a read-only directory
			// would refuse its own contents.
			dirs = append(dirs, dirMode{rel, perm, h.ModTime})
		case tar.TypeReg:
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = root.Chmod(rel, perm)
			}
			if err == nil {
				err = root.Chtimes(rel, h.ModTime, h.ModTime)
			}
			if err != nil {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
		case tar.TypeSymlink:
			if err := root.Symlink(h.Linkname, rel); err != nil {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
		case tar.TypeLink:
			target, err := archivePath(h.Linkname)
			if err != nil || target == "." {
				return fmt.Errorf("entry %q: hard link to %q leaves the archive root", h.Name, h.Linkname)
			}
			if err := noSymlinkOnPath(root, target); err != nil {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
			if err := root.Link(target, rel); err != nil {
				return fmt.Errorf("entry %q: %w", h.Name, err)
			}
		default:
			return fmt.Errorf("entry %q: %s entries are not restored", h.Name, typeName(h.Typeflag))
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- { // tar lists parents first, so children go first here
		d := dirs[i]
		if err := root.Chmod(d.rel, d.mode|0o700); err != nil {
			return err
		}
		root.Chtimes(d.rel, d.mod, d.mod) // best effort, as tar does
	}
	return nil
}

// archivePath turns an archive name into a path relative to the extraction
// root, or refuses it if it is absolute or climbs out.
func archivePath(name string) (string, error) {
	if err := checkMember(name); err != nil {
		return "", err
	}
	return filepath.Clean(filepath.FromSlash(name)), nil
}

// noSymlinkOnPath refuses a path whose parent directories include a symlink.
// os.Root already stops a link from leading outside the root; this also
// stops one entry redirecting a later one elsewhere inside it.
func noSymlinkOnPath(root *os.Root, rel string) error {
	dir := filepath.Dir(rel)
	if dir == "." {
		return nil
	}
	p := ""
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing deeper exists either
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("path goes through the symlink %q", filepath.ToSlash(p))
		}
	}
	return nil
}

func typeName(t byte) string {
	switch t {
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "fifo"
	default:
		return fmt.Sprintf("type %q", t)
	}
}
