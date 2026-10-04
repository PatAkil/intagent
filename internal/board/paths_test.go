package board

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// reservedBoard has alice, live, holding svc/pay/** exclusively, and returns
// n of bob's paths with svc/pay/retry.go at position at (0-based).
func reservedBoard(t *testing.T, n, at int) (*Board, time.Time, []PathRef) {
	t.Helper()
	b := New(Config{})
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	alice := Where{Repo: "r", Host: "ha", Worktree: "/a"}
	if _, err := b.Hook(now, HookEvent{Kind: KindPrompt, Member: "alice", Agent: AgentClaudeCode, SessionID: "a", Where: alice}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Declare(now, DeclareRequest{Member: "alice", Where: alice, Patterns: []string{"svc/pay/**"}, Mode: ModeExclusive}); err != nil {
		t.Fatal(err)
	}
	paths := make([]PathRef, n)
	for i := range paths {
		paths[i] = PathRef{Path: fmt.Sprintf("docs/%04d.md", i)}
	}
	paths[at] = PathRef{Path: "svc/pay/retry.go"}
	return b, now, paths
}

var bob = Where{Repo: "r", Host: "hb", Worktree: "/b"}

// A check covers every path it names, up to the most a claim keeps, and
// refuses more rather than leave some unchecked without saying so.
func TestCheckCoversEveryPath(t *testing.T) {
	for _, tc := range []struct{ n, at int }{{1, 0}, {201, 200}, {2000, 1999}} {
		b, now, paths := reservedBoard(t, tc.n, tc.at)
		cs, err := b.Check(now, CheckRequest{Member: "bob", Where: bob, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != 1 || cs[0].Severity != SeverityBlock {
			t.Errorf("%d paths, the reserved one at %d: conflicts %+v", tc.n, tc.at, cs)
		}
	}
	b, now, paths := reservedBoard(t, 2001, 0)
	if _, err := b.Check(now, CheckRequest{Member: "bob", Where: bob, Paths: paths}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("2001 paths: %v", err)
	}
}

// An edit is checked on every path it names, up to the most a claim keeps;
// past that, the agent is told what was not checked.
func TestEditsAreCheckedInFull(t *testing.T) {
	edit := func(b *Board, now time.Time, paths []PathRef) HookResult {
		t.Helper()
		res, err := b.Hook(now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "b", Where: bob, Tool: "apply_patch", Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	b, now, paths := reservedBoard(t, 1500, 1400)
	if res := edit(b, now, paths); res.Decision != DecisionRefuse {
		t.Fatalf("the reserved path at 1400 of 1500: %+v", res.Decision)
	}
	b, now, paths = reservedBoard(t, 2100, 0)
	res := edit(b, now, paths)
	if res.Decision != DecisionRefuse {
		t.Fatalf("the reserved path first of 2100: %+v", res.Decision)
	}
	b, now, paths = reservedBoard(t, 2100, 2050)
	res = edit(b, now, paths)
	if res.Decision != DecisionAllow || !strings.Contains(res.Context, "names 2100 files") || !strings.Contains(res.Context, "first 2000") {
		t.Fatalf("the reserved path past the limit: %s %q", res.Decision, res.Context)
	}
}

// An activity lists at most maxShownPaths paths, and counts the rest; the
// claim keeps them all.
func TestActivitiesListSomePathsAndCountTheRest(t *testing.T) {
	b, now, paths := reservedBoard(t, 250, 0)
	paths[0] = PathRef{Path: "docs/x.md"}
	if _, err := b.Hook(now, HookEvent{Kind: KindPostEdit, Member: "bob", Agent: AgentCodex, SessionID: "b", Where: bob, Paths: paths}); err != nil {
		t.Fatal(err)
	}
	v := b.View(now, "r")
	var changed *Activity
	for i, a := range v.Recent {
		if a.Kind == ActivityFileChanged {
			changed = &v.Recent[i]
		}
	}
	if changed == nil || len(changed.Paths) != maxShownPaths || changed.MorePaths != 50 || changed.Paths[0] != "docs/x.md" {
		t.Fatalf("file.changed = %+v", changed)
	}
	for _, c := range v.Claims {
		if c.Member == "bob" && c.FileCount != 250 {
			t.Fatalf("bob's claim keeps %d files", c.FileCount)
		}
	}
}
