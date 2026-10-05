// Package fsutil writes files so that a crash or a concurrent reader never
// sees half of one.
package fsutil

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WriteFile replaces the file at path with data, atomically: it writes a
// temporary file in the same directory, syncs it and renames it over path, so
// a reader sees the old contents or the new ones, never a torn mix.
//
// A symbolic link at path is written through, even one whose target does not
// exist yet, so a file kept in a dotfiles repository stays a link. A new file
// gets perm less the umask, as os.WriteFile gives it; a file replaced gets
// perm, and keeps its owner where the process may give it back. The
// directory must exist.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	return WriteFileFunc(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteFileFunc is WriteFile for contents that fill writes, which need not
// be in memory at once. When fill fails, path is left as it was and fill's
// error is returned.
func WriteFileFunc(path string, perm fs.FileMode, fill func(io.Writer) error) error {
	target, err := linkTarget(path)
	if err != nil {
		return err
	}
	if target != path {
		if _, err := os.Stat(filepath.Dir(target)); err != nil {
			return fmt.Errorf("%s links to %s, in a directory that cannot be written: %w", path, target, err)
		}
	}
	path = target
	old, statErr := os.Stat(path)
	f, err := createTemp(filepath.Dir(path), filepath.Base(path), perm)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op after the rename
	if statErr == nil {
		if err := f.Chmod(perm); err != nil {
			_ = f.Close()
			return err
		}
		keepOwner(f, old)
	}
	if err := write(f, fill); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync() // best effort: make the rename durable
		_ = d.Close()
	}
	return nil
}

// linkTarget follows the symbolic links at path to the file they name, which
// need not exist.
func linkTarget(path string) (string, error) {
	for range maxLinks {
		fi, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return path, nil // a file to create
		case err != nil:
			return "", err
		case fi.Mode()&fs.ModeSymlink == 0:
			return path, nil
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			// Relative to the directory that holds the link, which may itself
			// be reached through a link.
			dir := filepath.Dir(path)
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				dir = real
			}
			dest = filepath.Join(dir, dest)
		}
		path = dest
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

const maxLinks = 40

// createTemp creates a new file beside name, with perm less the umask.
func createTemp(dir, name string, perm fs.FileMode) (*os.File, error) {
	for {
		f, err := os.OpenFile(tempName(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}

// tempName names a temporary file beside name, as isTemp recognises it.
func tempName(dir, name string) string {
	return filepath.Join(dir, "."+name+"."+rand.Text()[:tempRandom]+".tmp")
}

const tempRandom = 12 // characters of rand.Text in a temporary file's name

// isTemp reports whether file is the name of a temporary file beside name.
func isTemp(file, name string) bool {
	mid, ok := strings.CutPrefix(file, "."+name+".")
	if !ok {
		return false
	}
	if mid, ok = strings.CutSuffix(mid, ".tmp"); !ok || len(mid) != tempRandom {
		return false
	}
	return strings.Trim(mid, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") == "" // rand.Text's alphabet
}

// RemoveTemps removes the temporary files that writes of path left behind
// when their process was killed before it could, and reports how many it
// removed. It must not run while another process writes path.
func RemoveTemps(path string) (int, error) {
	dir, name := filepath.Split(path)
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, e := range entries {
		if !e.Type().IsRegular() || !isTemp(e.Name(), name) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		n++
	}
	return n, first
}

// LinkAside makes aside another name for the file at path, replacing what
// was at aside, so that the file outlives a WriteFile that replaces path. It
// links under a temporary name and renames that into place, so aside is
// always a whole file. A missing file at path leaves aside as it was. When
// aside already is another name for that file (a write that would have
// replaced path failed since the last LinkAside), the rename does nothing,
// as rename does for two names of one file, and the temporary name is
// removed.
func LinkAside(path, aside string) error {
	target, err := linkTarget(path) // WriteFile replaces the file a link names
	if err != nil {
		return err
	}
	tmp := tempName(filepath.Dir(aside), filepath.Base(aside))
	if err := os.Link(target, tmp); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Rename(tmp, aside); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func write(f *os.File, fill func(io.Writer) error) error {
	err := fill(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ModeOr returns the permissions of the file at path, or def when there is no
// such file, so that rewriting a file keeps the mode its owner gave it.
func ModeOr(path string, def fs.FileMode) fs.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return def
}
