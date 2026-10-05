package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// WriteFileFunc writes what fill writes, piece by piece; when fill fails,
// the file is left as it was, with no temporary file beside it.
func TestWriteFileFuncFillsOrLeavesTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "board.json")
	if err := WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileFunc(p, 0o600, func(w io.Writer) error {
		for _, s := range []string{"n", "e", "w"} {
			if _, err := io.WriteString(w, s); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "new" {
		t.Fatalf("contents = %q", got)
	}
	broken := errors.New("abandoned")
	err := WriteFileFunc(p, 0o600, func(w io.Writer) error {
		_, _ = io.WriteString(w, "half")
		return broken
	})
	if !errors.Is(err, broken) {
		t.Fatalf("WriteFileFunc = %v, want fill's error", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "new" {
		t.Fatalf("a failed fill left %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// RemoveTemps removes what writes of a file left behind, and nothing else:
// not the file, nor another file's temporary files.
func TestRemoveTempsLeavesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "board.json")
	if err := WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := []string{tempName(dir, "board.json"), tempName(dir, "board.json")}
	keep := []string{p, tempName(dir, "board.json.prev"), tempName(dir, "ui.key"), filepath.Join(dir, ".board.json.short.tmp"),
		filepath.Join(dir, ".board.json.abcdefghijkl.tmp"), filepath.Join(dir, "ABCDEFGHIJKL.tmp")}
	for _, f := range append(stale, keep[1:]...) {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := RemoveTemps(p)
	if err != nil || n != len(stale) {
		t.Fatalf("RemoveTemps = %d, %v; want %d", n, err, len(stale))
	}
	for _, f := range stale {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", f)
		}
	}
	for _, f := range keep {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// LinkAside keeps the file that a WriteFile then replaces, and replaces
// what it kept before; with no file, it keeps what it had.
func TestLinkAsideKeepsTheFileReplaced(t *testing.T) {
	dir := t.TempDir()
	p, prev := filepath.Join(dir, "board.json"), filepath.Join(dir, "board.json.prev")
	if err := LinkAside(p, prev); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"one", "two", "three"} {
		if err := LinkAside(p, prev); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(prev); string(got) != "two" {
		t.Fatalf("aside = %q, want the file before the last write", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := LinkAside(p, prev); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(prev); string(got) != "two" {
		t.Fatalf("aside = %q after its file went", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("files left behind: %v", entries)
	}
}

// LinkAside leaves no temporary file behind when the file is already kept
// aside: as when a save of the board fails, and the next one links the same
// board.json aside again. Renaming a temporary link onto another name for
// the same file does nothing, and left the link behind on every failed
// save, each holding a whole snapshot's disk until the next start.
func TestLinkAsideAgainLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	p, prev := filepath.Join(dir, "board.json"), filepath.Join(dir, "board.json.prev")
	if err := WriteFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 { // the saves that fail after it leave p as it was
		if err := LinkAside(p, prev); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("files in the directory: %v, want board.json and board.json.prev", names)
	}
	if got, _ := os.ReadFile(prev); string(got) != "one" {
		t.Fatalf("aside = %q", got)
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

// A relative link inside a linked directory names a file relative to where
// the link really is: ~/.codex -> /opt/shared/codex, whose config.toml links
// to ../dotfiles/config.toml, means /opt/shared/dotfiles/config.toml.
func TestWriteFileResolvesRelativeLinksWhereTheyAre(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := t.TempDir()
	shared := filepath.Join(dir, "opt", "shared")
	for _, d := range []string{filepath.Join(shared, "codex"), filepath.Join(shared, "dotfiles"), filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(shared, "dotfiles", "config.toml")
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "dotfiles", "config.toml"), filepath.Join(shared, "codex", "config.toml")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home", ".codex")
	if err := os.Symlink(filepath.Join(shared, "codex"), home); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(home, "config.toml"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(real); string(got) != "new" {
		t.Fatalf("the real config = %q", got)
	}
	// A link into a directory that does not exist says so.
	dangling := filepath.Join(dir, "home", "settings.json")
	if err := os.Symlink(filepath.Join(dir, "missing", "settings.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(dangling, nil, 0o600); err == nil || !strings.Contains(err.Error(), "links to") {
		t.Fatalf("a link into a missing directory: %v", err)
	}
}
