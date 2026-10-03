//go:build unix

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A new file gets the umask, as os.WriteFile gives it; a file replaced keeps
// the mode it is given.
func TestWriteFileHonoursTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("a new file under umask 077: %v", fi.Mode())
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("{}"), ModeOr(p, 0o600)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("a replaced file: %v", fi.Mode())
	}
}

// Run as root (sudo intagent init), a rewrite leaves the file its owner's.
func TestWriteFileKeepsTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give a file to someone else")
	}
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(p, 65532, 65532); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 65532 || st.Gid != 65532 {
		t.Fatalf("owner after the rewrite: %d:%d", st.Uid, st.Gid)
	}
}
