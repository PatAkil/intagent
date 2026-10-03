// Package fsutil writes files so that a crash or a concurrent reader never
// sees half of one.
package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile replaces the file at path with data, atomically: it writes a
// temporary file in the same directory, syncs it and renames it over path, so
// a reader sees the old contents or the new ones, never a torn mix. A symbolic
// link at path is written through, so a file kept in a dotfiles repository
// stays a link. The file gets perm; the directory must exist.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op after the rename
	if err := write(f, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // best effort: make the rename durable
		_ = d.Close()
	}
	return nil
}

func write(f *os.File, data []byte, perm fs.FileMode) error {
	err := f.Chmod(perm)
	if err == nil {
		_, err = f.Write(data)
	}
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
