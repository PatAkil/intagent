package board

import (
	"errors"
	"fmt"
	"slices"
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

// A check covers every path it names, up to maxCheckPaths, and refuses more
// rather than leave some unchecked without saying so.
func TestCheckCoversEveryPath(t *testing.T) {
	for _, tc := range []struct{ n, at int }{{1, 0}, {maxCheckPaths, maxCheckPaths - 1}} {
		b, now, paths := reservedBoard(t, tc.n, tc.at)
		cs, err := b.Check(now, CheckRequest{Member: "bob", Where: bob, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != 1 || cs[0].Severity != SeverityBlock {
			t.Errorf("%d paths, the reserved one at %d: conflicts %+v", tc.n, tc.at, cs)
		}
	}
	b, now, paths := reservedBoard(t, maxCheckPaths+1, 0)
	if _, err := b.Check(now, CheckRequest{Member: "bob", Where: bob, Paths: paths}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d paths: %v", maxCheckPaths+1, err)
	}
}

// An edit is checked on its first maxCheckPaths paths; past that, the agent
// is told how many were not checked, and the conflict's activity still
// counts every file the edit named.
func TestEditsAreCheckedUpToTheCap(t *testing.T) {
	edit := func(b *Board, now time.Time, paths []PathRef) HookResult {
		t.Helper()
		res, err := b.Hook(now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "b", Where: bob, Tool: "apply_patch", Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	b, now, paths := reservedBoard(t, maxCheckPaths, maxCheckPaths-1)
	if res := edit(b, now, paths); res.Decision != DecisionRefuse || strings.Contains(res.Context, "checked only") {
		t.Fatalf("the reserved path last of %d: %s %q", maxCheckPaths, res.Decision, res.Context)
	}
	b, now, paths = reservedBoard(t, 1500, maxCheckPaths/2)
	if res := edit(b, now, paths); res.Decision != DecisionRefuse {
		t.Fatalf("the reserved path at %d of 1500: %s", maxCheckPaths/2, res.Decision)
	}
	var conflict *Activity
	for _, a := range b.View(now, "r").Recent {
		if a.Kind == ActivityConflict {
			conflict = &a
		}
	}
	if conflict == nil || !slices.Contains(conflict.Paths, "svc/pay/retry.go") || len(conflict.Paths)+conflict.MorePaths != 1500 {
		t.Fatalf("the refused edit's activity: %+v", conflict)
	}
	for _, n := range []int{maxCheckPaths + 1, 2100} {
		b, now, paths = reservedBoard(t, n, n-1)
		res := edit(b, now, paths)
		if res.Decision != DecisionAllow || !strings.Contains(res.Context, fmt.Sprintf("names %d files", n)) ||
			!strings.Contains(res.Context, fmt.Sprintf("first %d", maxCheckPaths)) {
			t.Fatalf("the reserved path last of %d: %s %q", n, res.Decision, res.Context)
		}
	}
}

// What an edit costs under the lock grows with the paths it checks, not with
// those it names: on a board where every path is a teammate's, a pre_edit
// naming 2000 meets as many conflicts as one naming maxCheckPaths.
func TestEditCostIsBounded(t *testing.T) {
	b, now, paths := reservedBoard(t, 2000, 0)
	alice := Where{Repo: "r", Host: "ha", Worktree: "/a"}
	if _, err := b.Hook(now, HookEvent{Kind: KindPostEdit, Member: "alice", Agent: AgentClaudeCode, SessionID: "a", Where: alice, Paths: paths}); err != nil {
		t.Fatal(err)
	}
	res, err := b.Hook(now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "b", Where: bob, Tool: "apply_patch", Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != maxCheckPaths {
		t.Fatalf("a pre_edit of 2000 of alice's files met %d conflicts, want %d", len(res.Conflicts), maxCheckPaths)
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
