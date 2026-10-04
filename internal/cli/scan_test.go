package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A shell command scans a worktree at most once per window, whichever of its
// sessions ran it, and the hook that claims a window clears the stamps of
// windows past, a killed hook's included, those of older versions, and sent
// and unscanned stamps too old to matter.
func TestScanStampsPerWorktree(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache) // where macOS keeps its cache
	dir := cacheDir()
	writeFile(t, filepath.Join(dir, "scan-0123456789abcdef"), "")  // an older version's
	writeFile(t, filepath.Join(dir, "hook.log"), "")               // not a stamp
	writeFile(t, filepath.Join(dir, "unchecked-0123456789ab"), "") // not a stamp either
	t0 := time.Unix(0, 0).Add(1000*footprintEvery + time.Second)
	a, b := "/work/a", "/work/b"
	footprintSent(b, t0.Add(-footprintEvery))
	stamp(a, "unscanned-", t0.Add(-footprintEvery))
	for _, c := range []struct {
		root string
		at   time.Duration
		due  bool
	}{
		{a, 0, true},
		{a, 5 * time.Second, false},
		{b, 5 * time.Second, true},
		{a, footprintEvery - 2*time.Second, false},
		{a, footprintEvery, true},
		{b, footprintEvery + time.Second, true},
		{b, footprintEvery + 2*time.Second, false},
	} {
		if got := footprintDue(c.root, t0.Add(c.at)); got != c.due {
			t.Errorf("%s at +%s: due %v, want %v", c.root, c.at, got, c.due)
		}
	}
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"hook.log", "scan-" + worktreeKey(a) + "-1001", "scan-" + worktreeKey(b) + "-1001", "unchecked-0123456789ab"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("stamps left: %v, want %v", names, want)
	}
	// A stamp a killed hook left in a window past does not hold the next.
	writeFile(t, filepath.Join(dir, "scan-"+worktreeKey(a)+"-1002"), "")
	if footprintDue(a, t0.Add(3*footprintEvery)) != true {
		t.Error("a stamp of a window past held a scan back")
	}
}

// A session's end skips the scan when a footprint of the worktree that
// reached the server began within footprintEvery, and after the last shell
// command that did not scan.
func TestSentStampsExpire(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t0 := time.Now().Truncate(time.Second)
	footprintSent("/work/a", t0)
	footprintSent("/work/c", t0)
	stamp("/work/c", "unscanned-", t0.Add(-time.Second)) // before the scan began
	footprintSent("/work/d", t0)
	stamp("/work/d", "unscanned-", t0) // as it began
	footprintSent("/work/e", t0)
	stamp("/work/e", "unscanned-", t0.Add(time.Second)) // after
	for _, c := range []struct {
		root   string
		at     time.Duration
		recent bool
	}{
		{"/work/a", 0, true},
		{"/work/a", footprintEvery - time.Second, true},
		{"/work/a", footprintEvery, false},
		{"/work/a", -time.Minute, false},
		{"/work/b", time.Second, false},
		{"/work/c", 2 * time.Second, true},
		{"/work/d", 2 * time.Second, false},
		{"/work/e", 2 * time.Second, false},
	} {
		if got := sentRecently(c.root, t0.Add(c.at)); got != c.recent {
			t.Errorf("%s at +%s: recent %v, want %v", c.root, c.at, got, c.recent)
		}
	}
}

// tickingClock starts in the middle of a footprintEvery window and moves on
// a millisecond each time it is read, so hooks in a test never straddle a
// window's end, and each sees a later time than the one before.
func tickingClock() func() time.Time {
	var mu sync.Mutex
	now := time.Unix(0, 0).Add(1000*footprintEvery + footprintEvery/2)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Millisecond)
		return now
	}
}

// countScans puts a git on PATH that counts the footprint scans it makes.
func countScans(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "git.log")
	writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\necho \"$*\" >>'"+log+"'\nexec '"+real+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(log)
		return strings.Count(string(data), "ls-files --others --exclude-standard -z\n")
	}
}

// Sessions sharing a worktree share its footprint, and so its scans; and a
// session's end does not scan again right after a Stop sent the footprint,
// unless the Stop's never reached the server.
func TestHooksScanTheWorktreeOnce(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	tm.now = tickingClock()
	scans := countScans(t)
	hook := func(session, event string, extra map[string]any) {
		t.Helper()
		if _, errOut, code := tm.as("alice", a, claudeEvent(session, a, event, extra), "hook", "claude-code"); code != 0 {
			t.Fatalf("%s: %d %s", event, code, errOut)
		}
	}
	bash := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make"}}
	for _, s := range []string{"a1", "a2", "a3"} {
		hook(s, "PostToolUse", bash)
	}
	if n := scans(); n != 1 {
		t.Errorf("three sessions' shell commands in one worktree made %d scans, want 1", n)
	}
	hook("a1", "Stop", map[string]any{})
	before := scans()
	hook("a1", "SessionEnd", map[string]any{"reason": "exit"})
	if n := scans() - before; n != 0 {
		t.Errorf("the session's end scanned %d times right after its Stop", n)
	}

	// A Stop whose footprint the server refused leaves the session's end to scan.
	if _, err := os.Stat(filepath.Join(cacheDir(), "sent-"+worktreeKey(a))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cacheDir(), "sent-"+worktreeKey(a))); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INTAGENT_TOKEN", "not-a-token")
	hook("a2", "Stop", map[string]any{})
	t.Setenv("INTAGENT_TOKEN", "")
	before = scans()
	hook("a2", "SessionEnd", map[string]any{"reason": "exit"})
	if n := scans() - before; n != 1 {
		t.Errorf("after a Stop the server refused, the session's end scanned %d times, want 1", n)
	}
}

// A shell command that ends in a window another one already scanned may
// have changed files that scan did not see; if the session then ends with
// no Stop, as an interrupted turn does, its end scans for them.
func TestSessionEndScansAfterAnUnscannedCommand(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	tm.now = tickingClock()
	scans := countScans(t)
	hook := func(event string, extra map[string]any) {
		t.Helper()
		if _, errOut, code := tm.as("alice", a, claudeEvent("a1", a, event, extra), "hook", "claude-code"); code != 0 {
			t.Fatalf("%s: %d %s", event, code, errOut)
		}
	}
	bash := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make"}}
	hook("SessionStart", map[string]any{"source": "startup"})
	hook("PostToolUse", bash)
	writeFile(t, filepath.Join(a, "svc/pay/new.go"), "package pay\n")
	before := scans()
	hook("PostToolUse", bash)
	if n := scans() - before; n != 0 {
		t.Fatalf("a second shell command in the window scanned %d times", n)
	}
	hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
	for _, r := range tm.srv.Board().Repos(time.Now()) {
		for _, cl := range tm.srv.Board().View(time.Now(), r.Repo).Claims {
			for _, f := range cl.Files {
				if f.Path == "svc/pay/new.go" {
					return
				}
			}
		}
	}
	t.Fatal("svc/pay/new.go, written by the session's last shell command, never reached the board")
}
