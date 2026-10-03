package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteFileReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := WriteFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "two" {
		t.Fatalf("contents = %q", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode())
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestWriteFileWritesThroughLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "config.toml")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v, %v", fi, err)
	}
	if got, _ := os.ReadFile(real); string(got) != "new" {
		t.Fatalf("the link's target = %q", got)
	}
}

func TestWriteFileNeedsTheDirectory(t *testing.T) {
	if err := WriteFile(filepath.Join(t.TempDir(), "missing", "f"), nil, 0o600); err == nil {
		t.Fatal("wrote into a directory that does not exist")
	}
}

func TestModeOr(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if got := ModeOr(p, 0o644); got != 0o644 {
		t.Fatalf("ModeOr(missing) = %v", got)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ModeOr(p, 0o644); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("ModeOr(existing) = %v", got)
	}
}

func TestLockKeepsHoldersApart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "team.json.lock")
	unlock, err := Lock(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p, 50*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second Lock while held: %v, want ErrLocked", err)
	}
	unlock()
	again, err := Lock(p, time.Second)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	again()
}

func TestLockSerialisesWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "counter")
	var wg sync.WaitGroup
	inside := atomic.Int32{}
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := Lock(p+".lock", 10*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			if n := inside.Add(1); n != 1 {
				t.Errorf("%d holders at once", n)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
		}()
	}
	wg.Wait()
}

// A dotfiles link whose target does not exist yet is written through, and
// stays a link, whether it names its target absolutely or relatively.
func TestWriteFileFollowsDanglingLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	for name, relative := range map[string]bool{"absolute": false, "relative": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "dotfiles"), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "dotfiles", "config.toml")
			dest := target
			if relative {
				dest = filepath.Join("dotfiles", "config.toml")
			}
			link := filepath.Join(dir, "config.toml")
			if err := os.Symlink(dest, link); err != nil {
				t.Fatal(err)
			}
			if err := WriteFile(link, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the link was replaced: %v, %v", fi, err)
			}
			if got, _ := os.ReadFile(target); string(got) != "new" {
				t.Fatalf("the target = %q", got)
			}
		})
	}
}

func TestWriteFileStopsAtLinkLoops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(a, nil, 0o600); err == nil {
		t.Fatal("wrote through a loop of links")
	}
}
