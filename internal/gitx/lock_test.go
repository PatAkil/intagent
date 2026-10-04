package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeGit puts a shell script named git first on PATH.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake git is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A git stopped at its deadline gets to remove its lock files: one killed
// while it rewrites the index leaves .git/index.lock behind, and every later
// git add and commit in the worktree fails until someone deletes it.
func TestStoppedGitRemovesItsLock(t *testing.T) {
	dir := t.TempDir()
	lock, log := filepath.Join(dir, "index.lock"), filepath.Join(dir, "log")
	t.Setenv("LOCK", lock)
	t.Setenv("LOG", log)
	// Like git, the script removes its lock file when it is told to stop.
	fakeGit(t, `trap 'rm -f "$LOCK"; echo stopped >>"$LOG"; exit 143' TERM INT
: >"$LOCK"
while :; do sleep 0.01; done
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !exists(lock) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := run(ctx, dir, "diff"); err == nil {
		t.Fatal("a git stopped half way reported success")
	}
	if exists(lock) {
		t.Fatal("the stopped git left its lock file behind")
	}
	if got, _ := os.ReadFile(log); string(got) != "stopped\n" {
		t.Fatalf("the git was not told to stop: %q", got)
	}
}

// A git that does not stop when told is killed shortly after.
func TestGitThatDoesNotStopIsKilled(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	t.Setenv("LOCK", lock)
	fakeGit(t, `trap '' TERM INT
: >"$LOCK"
while :; do sleep 0.01; done
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !exists(lock) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	if _, err := run(ctx, dir, "diff"); err == nil {
		t.Fatal("a killed git reported success")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("run took %s to give up on a git that ignores its deadline", took)
	}
}

// statDirty gives the committed files under dir new timestamps, a second
// after those of its previous call, and the same content, as a formatter or
// code generator leaves them: git diff then refreshes the index it reads and
// writes it back under index.lock.
func statDirty(t *testing.T, root, dir string) {
	t.Helper()
	dirtied++
	old := time.Date(2001, 1, 1, 0, 0, dirtied, 0, time.UTC)
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(root, dir, e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

var dirtied int

// generated commits n files of a few kilobytes under gen/ in clone.
func generated(t *testing.T, clone string, n int) {
	t.Helper()
	for i := range n {
		write(t, clone, fmt.Sprintf("gen/f%04d.go", i), strings.Repeat(fmt.Sprintf("package gen // %d\n", i), 200))
	}
	git(t, clone, "add", "gen")
	git(t, clone, "commit", "-q", "-m", "generated")
	git(t, clone, "push", "-q", "origin", "HEAD:main")
	git(t, clone, "fetch", "-q", "origin")
}

// Listing the changes never writes the real index, so it never takes
// .git/index.lock, which would refuse the person's and other agents' git
// add and commit while held; and its answer is git diff's own.
func TestChangesLeaveTheIndexAlone(t *testing.T) {
	ctx := context.Background()
	_, clone := newRepo(t)
	generated(t, clone, 50)
	write(t, clone, "services/payments/retry.go", "package pay // changed\n")
	write(t, clone, "notes/todo.txt", "untracked\n")
	statDirty(t, clone, "gen")
	index := filepath.Join(clone, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(index)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	w, err := Open(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	files, base, err := w.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(index); !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) || !bytes.Equal(now, content) {
		t.Fatal("listing the changes rewrote the real index")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("the index copy was left behind: %v", left)
	}

	// The same answer as git diff on the real index, which it then refreshes.
	plain := strings.Split(git(t, clone, "diff", "--name-only", "--no-renames", base, "--"), "\n")
	plain = append(plain, strings.Split(git(t, clone, "ls-files", "--others", "--exclude-standard"), "\n")...)
	if got, want := strings.Join(files, ","), "notes/todo.txt,services/payments/retry.go"; got != want || len(plain) != 2 {
		t.Fatalf("changes = %s, want %s; git diff and ls-files say %q", got, want, plain)
	}
	if again, _ := os.Stat(index); os.SameFile(before, again) {
		t.Fatal("the test's own git diff did not refresh the index: the files were not stat-dirty")
	}
}

// Stopped at any point, a footprint scan leaves no lock and no copy behind
// but the one it kept.
func TestChangesStoppedAnywhereLeaveNoLock(t *testing.T) {
	_, clone := newRepo(t)
	generated(t, clone, 1000)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	w, err := Open(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	w.Cache = t.TempDir()
	statDirty(t, clone, "gen")
	start := time.Now()
	if _, _, err := w.Changes(context.Background()); err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)
	stopped := 0
	for i := range 24 {
		statDirty(t, clone, "gen")
		ctx, cancel := context.WithTimeout(context.Background(), full*time.Duration(i)/20)
		if _, _, err := w.Changes(ctx); err != nil {
			stopped++
		}
		cancel()
		if exists(filepath.Join(clone, ".git", "index.lock")) {
			t.Fatalf("a scan stopped after %s of %s left .git/index.lock behind", full*time.Duration(i)/20, full)
		}
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Fatalf("a scan stopped after %s left %v behind", full*time.Duration(i)/20, left)
		}
		if left := copies(t, w.Cache); len(left) != 1 || !strings.HasPrefix(left[0], keptPrefix) {
			t.Fatalf("a scan stopped after %s left %v in the cache", full*time.Duration(i)/20, left)
		}
	}
	if stopped == 0 {
		t.Fatalf("no scan was stopped (a full one takes %s)", full)
	}
}

// The index copy keeps the original's modification time, which git compares
// with each file's to know whether it may trust what the index records.
func TestIndexCopyKeepsItsTime(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "index"), filepath.Join(dir, "copy")
	write(t, dir, "index", "DIRC")
	at := time.Date(2020, 2, 2, 2, 2, 2, 123456789, time.UTC)
	if err := os.Chtimes(src, at, at); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(src)
	b, err := os.Stat(dst)
	if err != nil || !b.ModTime().Equal(a.ModTime()) {
		t.Fatalf("copy's time %v, original's %v (%v)", b.ModTime(), a.ModTime(), err)
	}
	if copyFile(src, dst) == nil {
		t.Fatal("copyFile replaced an existing file")
	}
}

// indexWrites counts the times git writes an index, from its trace.
func indexWrites(t *testing.T) func() int {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	return func() int {
		data, _ := os.ReadFile(trace)
		return bytes.Count(data, []byte(`"key":"write/cache_nr"`))
	}
}

// After a formatter, the first scan checks the content of the files it
// touched and refreshes its copy of the index; the scans after it start
// from that copy, while the real index is as it was, and check nothing.
// Without a Cache, each scan starts from the real index and checks again.
func TestKeptIndexSparesLaterScans(t *testing.T) {
	ctx := context.Background()
	_, clone := newRepo(t)
	generated(t, clone, 50)
	writes := indexWrites(t)
	for _, c := range []struct {
		cache  string
		writes []int // by each of three scans
	}{
		{"", []int{1, 1, 1}},
		{t.TempDir(), []int{1, 0, 0}},
	} {
		w, err := Open(ctx, clone)
		if err != nil {
			t.Fatal(err)
		}
		w.Cache = c.cache
		statDirty(t, clone, "gen")
		var got []int
		for range 3 {
			before := writes()
			files, _, err := w.Changes(ctx)
			if err != nil || len(files) != 0 {
				t.Fatalf("changes = %v, %v", files, err)
			}
			got = append(got, writes()-before)
		}
		if !slices.Equal(got, c.writes) {
			t.Errorf("cache %q: scans wrote an index %v times, want %v", c.cache, got, c.writes)
		}
		if c.cache != "" {
			if left := copies(t, c.cache); len(left) != 1 || !strings.HasPrefix(left[0], keptPrefix) {
				t.Errorf("cache holds %v, want one kept copy", left)
			}
		}
	}
}

// copies lists the index copies in a Cache.
func copies(t *testing.T, cache string) []string {
	t.Helper()
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A kept copy serves only while the real index is as it was when the copy
// was made: once the person stages a new file, the next scan starts from
// the real index again, and a kept copy git cannot read is replaced.
func TestKeptIndexFollowsTheRealOne(t *testing.T) {
	ctx := context.Background()
	_, clone := newRepo(t)
	w, err := Open(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	w.Cache = t.TempDir()
	write(t, clone, "services/payments/new.go", "package pay\n")
	scan := func(want string) {
		t.Helper()
		files, _, err := w.Changes(ctx)
		if got := strings.Join(files, ","); err != nil || got != want {
			t.Fatalf("changes = %s, %v; want %s", got, err, want)
		}
	}
	scan("services/payments/new.go")
	// Staged, the file is no longer untracked: only an index that has it
	// shows it.
	git(t, clone, "add", "services/payments/new.go")
	scan("services/payments/new.go")

	kept := copies(t, w.Cache)
	if len(kept) != 1 {
		t.Fatalf("cache holds %v, want one kept copy", kept)
	}
	write(t, w.Cache, kept[0], "not an index")
	scan("services/payments/new.go")
	if data, err := os.ReadFile(filepath.Join(w.Cache, kept[0])); err != nil || !bytes.HasPrefix(data, []byte("DIRC")) {
		t.Fatalf("the unreadable copy was not replaced: %q, %v", data, err)
	}
}

// A scan prunes what scans killed outright left in the Cache, this
// worktree's copies of an index it no longer has, and copies of other
// worktrees not refreshed for a day; it leaves the rest alone.
func TestScansPruneOldCopies(t *testing.T) {
	ctx := context.Background()
	_, clone := newRepo(t)
	w, err := Open(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	w.Cache = t.TempDir()
	mine := keptPrefix + hashKey(filepath.Join(w.Root, ".git", "index")) + "-"
	now := time.Now()
	for name, age := range map[string]time.Duration{
		copyPrefix + "killed/index":  2 * copyFor,
		copyPrefix + "running/index": 0,
		mine + "0123456789abcdef":    0,
		keptPrefix + "other-recent":  time.Hour,
		keptPrefix + "other-gone":    2 * keptFor,
		"hook.log":                   2 * keptFor,
		"scan-0123-1":                2 * keptFor,
	} {
		write(t, w.Cache, name, "")
		for p := filepath.Join(w.Cache, name); p != w.Cache; p = filepath.Dir(p) {
			if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := w.Changes(ctx); err != nil {
		t.Fatal(err)
	}
	got := copies(t, w.Cache)
	var kept string
	for _, n := range got {
		if strings.HasPrefix(n, mine) {
			kept = n
		}
	}
	want := []string{"hook.log", keptPrefix + "other-recent", "scan-0123-1", copyPrefix + "running", kept}
	slices.Sort(want)
	if !slices.Equal(got, want) || kept == mine+"0123456789abcdef" {
		t.Fatalf("cache holds %v, want %v", got, want)
	}
}
