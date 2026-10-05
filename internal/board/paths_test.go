package board

import (
	"fmt"
	"maps"
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

// A check covers every path it names, up to maxCheckPaths. Past that it
// checks the first maxCheckPaths against all of teammates' work and the
// rest, up to MaxFootprint, against their reservations, and its answer
// counts the others and says so: an older guard sends a whole commit in one
// check, and refused, it would let the commit through unchecked, while a
// reservation among the rest stops it.
func TestCheckCoversEveryPath(t *testing.T) {
	maxFootprint := DefaultConfig().MaxFootprint
	for _, tc := range []struct {
		n, at, conflicts, unchecked int
		rest                        string
	}{
		{1, 0, 1, 0, ""},
		{maxCheckPaths, maxCheckPaths - 1, 1, 0, ""},
		{maxCheckPaths + 50, maxCheckPaths - 1, 1, 50, "the rest"},
		{maxCheckPaths + 50, maxCheckPaths, 1, 50, "the rest"},
		{maxFootprint + 50, maxFootprint - 1, 1, maxFootprint + 50 - maxCheckPaths, "the rest up to the first 2000"},
		{maxFootprint + 50, maxFootprint, 0, maxFootprint + 50 - maxCheckPaths, "the rest up to the first 2000"},
	} {
		b, now, paths := reservedBoard(t, tc.n, tc.at)
		res, err := b.Check(now, CheckRequest{Member: "bob", Where: bob, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Conflicts) != tc.conflicts || (tc.conflicts > 0 && res.Conflicts[0].Severity != SeverityBlock) || res.Unchecked != tc.unchecked {
			t.Errorf("%d paths, the reserved one at %d: conflicts %+v, %d unchecked", tc.n, tc.at, res.Conflicts, res.Unchecked)
		}
		told := fmt.Sprintf("intagent checked only the first %d of these %d paths against all of teammates' work, and %s "+
			"against their reservations alone. Check the other %d", maxCheckPaths, tc.n, tc.rest, tc.unchecked)
		if strings.Contains(res.Text, told) != (tc.unchecked > 0) {
			t.Errorf("%d paths: %q", tc.n, res.Text)
		}
	}
}

// editOf is bob's pre_edit of paths, as one Codex apply_patch.
func editOf(t *testing.T, b *Board, now time.Time, paths []PathRef) HookResult {
	t.Helper()
	res, err := b.Hook(now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "b", Where: bob, Tool: "apply_patch", Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// An edit is checked against everything on its first maxCheckPaths paths,
// and against teammates' reservations on all it names up to MaxFootprint:
// a file a teammate holds exclusively is refused wherever the edit names it.
// Past maxCheckPaths, the agent is told what was not checked, and the
// conflict's activity still counts every file the edit named.
func TestEditsAreCheckedUpToTheCap(t *testing.T) {
	edit := func(b *Board, now time.Time, paths []PathRef) HookResult {
		t.Helper()
		return editOf(t, b, now, paths)
	}
	b, now, paths := reservedBoard(t, maxCheckPaths, maxCheckPaths-1)
	if res := edit(b, now, paths); res.Decision != DecisionRefuse || strings.Contains(res.Context, "checked only") {
		t.Fatalf("the reserved path last of %d: %s %q", maxCheckPaths, res.Decision, res.Context)
	}
	// The last path checked, past those an activity lists if the cap ever
	// exceeds them: the activity lists it first.
	n, at := maxCheckPaths+300, maxCheckPaths-1
	b, now, paths = reservedBoard(t, n, at)
	if res := edit(b, now, paths); res.Decision != DecisionRefuse {
		t.Fatalf("the reserved path at %d of %d: %s", at, n, res.Decision)
	}
	var conflict *Activity
	for _, a := range b.View(now, "r").Recent {
		if a.Kind == ActivityConflict {
			conflict = &a
		}
	}
	if conflict == nil {
		t.Fatal("no conflict activity")
	}
	if conflict.Paths[0] != "svc/pay/retry.go" || len(conflict.Paths)+conflict.MorePaths != min(n, DefaultConfig().MaxFootprint) {
		t.Fatalf("the refused edit's activity lists %d paths, %q first, and %d more; svc/pay/retry.go listed: %v",
			len(conflict.Paths), conflict.Paths[0], conflict.MorePaths, slices.Contains(conflict.Paths, "svc/pay/retry.go"))
	}
	// Past maxCheckPaths, the reservation still refuses the edit, up to
	// the MaxFootprint paths the board reads of it.
	maxFootprint := DefaultConfig().MaxFootprint
	for _, tc := range []struct{ n, at int }{
		{maxCheckPaths + 1, maxCheckPaths},
		{250, 249},
		{maxFootprint, maxFootprint - 1},
		{maxFootprint + 100, maxFootprint - 1},
	} {
		b, now, paths = reservedBoard(t, tc.n, tc.at)
		res := edit(b, now, paths)
		if res.Decision != DecisionRefuse || !strings.Contains(res.Reason, "svc/pay/retry.go") {
			t.Fatalf("the reserved path at %d of %d: %s %q", tc.at, tc.n, res.Decision, res.Reason)
		}
	}
	// Each such edit is told what was checked of it.
	for _, tc := range []struct {
		n    int
		told string
	}{
		{maxCheckPaths + 1, "checked them all against teammates' reservations, but only the first 200 against the rest"},
		{maxFootprint + 100, "checked only the first 2000 against teammates' reservations, but only the first 200 against the rest"},
	} {
		b, now, paths = reservedBoard(t, tc.n, 0)
		paths[0] = PathRef{Path: "docs/x.md"}
		res := edit(b, now, paths)
		if res.Decision != DecisionAllow || !strings.Contains(res.Context, fmt.Sprintf("names %d files", tc.n)) ||
			!strings.Contains(res.Context, tc.told) {
			t.Fatalf("%d paths: %s %q", tc.n, res.Decision, res.Context)
		}
	}
	// The board reads no more than MaxFootprint of an edit's paths, and lets
	// the rest through, telling the agent.
	b, now, paths = reservedBoard(t, maxFootprint+100, maxFootprint)
	if res := edit(b, now, paths); res.Decision != DecisionAllow || !strings.Contains(res.Context, "only the first 2000") {
		t.Fatalf("the reserved path at %d of %d: %s %q", maxFootprint, maxFootprint+100, res.Decision, res.Context)
	}
}

// The paths of an edit past maxCheckPaths are compared with teammates'
// reservations alone, and a pattern rooted in a directory only with the
// paths under it: on top of the first maxCheckPaths, an edit of 2000 paths
// costs a match for each path under a reserved directory, not one for each
// path and pattern.
func TestLargeEditReservationsCostLittle(t *testing.T) {
	b, now, paths := reservedBoard(t, 2000, 1999)
	trace = &workTrace{}
	t.Cleanup(func() { trace = nil })
	if res := editOf(t, b, now, paths); res.Decision != DecisionRefuse {
		t.Fatalf("the reserved path last of 2000: %s", res.Decision)
	}
	// One match for each of the first maxCheckPaths paths against alice's
	// one intent, and one for the reserved file.
	if trace.conflictsFor != maxCheckPaths || trace.globMatch != maxCheckPaths+1 {
		t.Fatalf("an edit of 2000 paths judged %d in full and made %d matches, want %d and %d",
			trace.conflictsFor, trace.globMatch, maxCheckPaths, maxCheckPaths+1)
	}
}

// Every path an edit names is compared with every teammate's reservations
// that could cover it, and refused as conflictsFor would refuse it: by the
// first of each holder's exclusive intents that covers it, rooted or not,
// whoever holds it, and not by a holder that is not live or an intent that is
// shared.
func TestLateReservedPathsAreRefusedAsEarlyOnes(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	b := New(Config{})
	declare := func(member, worktree string, live bool, mode Mode, patterns ...string) {
		t.Helper()
		w := Where{Repo: "r", Host: "h" + member, Worktree: worktree}
		if live {
			if _, err := b.Hook(now, HookEvent{Kind: KindPrompt, Member: member, Agent: AgentClaudeCode, SessionID: member + worktree, Where: w}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := b.Declare(now, DeclareRequest{Member: member, Where: w, Patterns: patterns, Mode: mode})
		if err != nil || len(res.Rejected) > 0 {
			t.Fatalf("%s's declaration: %v %+v", member, err, res.Rejected)
		}
	}
	declare("alice", "/a", true, ModeExclusive, "svc/pay/**", "*.sql")
	declare("carol", "/c", true, ModeShared, "docs/**")
	declare("dave", "/d", true, ModeExclusive, "web/**")
	declare("erin", "/e", false, ModeExclusive, "ops/**") // no session: not live
	named := []string{"svc/pay/retry.go", "schema.sql", "docs/a.md", "web/x.ts", "ops/run.sh", "svc/payroll.go", "db/x.sql"}
	want := map[string]string{"svc/pay/retry.go": "alice", "schema.sql": "alice", "web/x.ts": "dave"}
	for _, late := range []bool{false, true} {
		paths := make([]PathRef, 0, maxCheckPaths+len(named))
		if late {
			for i := range maxCheckPaths {
				paths = append(paths, PathRef{Path: fmt.Sprintf("lib/%04d.go", i)})
			}
		}
		for _, p := range named {
			paths = append(paths, PathRef{Path: p})
		}
		// A fresh session each time, which no bump has been spent on.
		res, err := b.Hook(now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: fmt.Sprint("b", late), Where: bob,
			Tool: "apply_patch", Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, cf := range res.Conflicts {
			if cf.Severity == SeverityBlock {
				got[cf.Path] = cf.Member
			}
		}
		if res.Decision != DecisionRefuse || !maps.Equal(got, want) {
			t.Errorf("named after %d others: %s, blocked %v, want %v", len(paths)-len(named), res.Decision, got, want)
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

// An edit its agent made without hearing a check is judged when its
// post_edit comes as its pre_edit would have been: on its first
// maxCheckPaths paths in full, though the claim keeps up to MaxFootprint of
// them, and on all of them against teammates' reservations.
func TestEditCheckedAfterwardsCostIsBounded(t *testing.T) {
	for _, at := range []int{0, 1999} {
		b, now, paths := reservedBoard(t, 2000, at)
		trace = &workTrace{}
		t.Cleanup(func() { trace = nil })
		res, err := b.Hook(now, HookEvent{Kind: KindPostEdit, Member: "bob", Agent: AgentCodex, SessionID: "b", Where: bob, Tool: "apply_patch",
			ToolUseID: "call-1", Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if !res.CheckedAfter || !strings.Contains(res.Context, "svc/pay/retry.go was not checked") {
			t.Fatalf("the edit, the reserved file at %d, was not checked afterwards: %+v", at, res)
		}
		if trace.conflictsFor != maxCheckPaths {
			t.Fatalf("a post_edit of 2000 paths, checked afterwards, judged %d of them, want %d", trace.conflictsFor, maxCheckPaths)
		}
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
