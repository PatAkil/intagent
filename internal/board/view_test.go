package board

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A view orders and caps what it shows after letting go of the board's
// lock, from what it copied while it held it: hooks can run meanwhile, and
// what they change is not in the view, not even in part.
func TestViewIsBuiltOutsideTheLock(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPostEdit, "bob", "b1", "a/x.go", "a/y.go")
	h.hook(KindPostEdit, "carol", "c1", "a/x.go")
	h.hook(KindPrompt, "carol", "c2")
	at := h.now
	want := jsonOf(h.b.View(at, repo))
	built := false
	h.b.viewCopied = func() {
		h.b.viewCopied = nil
		if !h.b.mu.TryLock() {
			t.Error("the board's lock is held while the view is built")
			return
		}
		h.b.mu.Unlock()
		built = true
		h.advance(time.Minute)
		h.hook(KindPostEdit, "bob", "b1", "a/x.go", "a/z.go")
		h.hook(KindSessionEnd, "carol", "c2")
	}
	got := jsonOf(h.b.View(at, repo))
	if !built {
		t.Fatal("the view was not built outside the lock")
	}
	if got != want {
		t.Fatalf("a view shows changes made after it read the board:\n got: %s\nwant: %s", got, want)
	}
	if after := jsonOf(h.b.View(at, repo)); after == want {
		t.Fatal("the changes made while the view was built are not on the board")
	}
}

// Views read the touches they copied after letting go of the lock, while
// hooks change the same files, and their areas, again and again: a touch is
// replaced, never changed, so the race detector finds nothing.
func TestViewWhileHooksRun(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := fmt.Sprintf("m%d", g%2)
			for i := range 60 {
				now := t0.Add(time.Duration(i) * time.Second)
				file := PathRef{Path: fmt.Sprintf("p/%d.go", i%4), Area: fmt.Sprintf("p%d", i%3)}
				_, _ = h.b.Hook(now, HookEvent{Kind: KindPostEdit, Member: m, Agent: AgentCodex, SessionID: fmt.Sprint(g), Where: whereOf(m),
					Paths: []PathRef{file}})
				file.Area = fmt.Sprintf("q%d", i%3)
				_, _ = h.b.Hook(now, HookEvent{Kind: KindHeartbeat, Member: m, Agent: AgentCodex, SessionID: fmt.Sprint(g), Where: whereOf(m),
					Footprint: &Footprint{Files: []PathRef{file, {Path: "p/9.go", Area: file.Area}}}})
			}
		}()
	}
	for range 100 {
		for _, c := range h.b.View(t0, repo).Claims {
			for _, f := range c.Files {
				_ = f.Area
			}
		}
	}
	wg.Wait()
}

// The board shows a repository while it has claims in it, counts for it or
// activities of it in the feed, and a name it has none of not at all.
func TestShows(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KeepActivities = 2 })
	if h.b.Shows(repo) {
		t.Fatal("an empty board shows a repository")
	}
	h.hook(KindPostEdit, "bob", "b1", "a/x.go")
	if !h.b.Shows(repo) || h.b.Shows("github.com/acme/other") {
		t.Fatal("a claim's repository is not shown, or another is")
	}
	// The claim goes; its activities are still in the feed.
	h.b.mu.Lock()
	for _, c := range h.b.claims {
		h.b.removeClaim(c)
	}
	h.b.mu.Unlock()
	if !h.b.Shows(repo) {
		t.Fatal("a repository with activities in the feed is not shown")
	}
	// The feed lets go of them, for another repository's.
	other := HookEvent{Kind: KindPostEdit, Member: "carol", Agent: AgentCodex, SessionID: "c1",
		Where: Where{Repo: "github.com/acme/other", Host: "h", Worktree: "/w"}, Paths: refs("b/y.go")}
	for range 2 {
		if _, err := h.b.Hook(h.now, other); err != nil {
			t.Fatal(err)
		}
	}
	if h.b.Shows(repo) {
		t.Fatal("a repository the board has nothing of any more is shown")
	}
	h.b.mu.Lock()
	h.b.statsOf(repo, h.now).Checks++
	h.b.mu.Unlock()
	if !h.b.Shows(repo) {
		t.Fatal("a repository with counts is not shown")
	}
}
