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
		p := filepath.Join(dir, "."+name+"."+rand.Text()[:12]+".tmp")
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
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
