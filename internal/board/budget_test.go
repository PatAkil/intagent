package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/patakil/intagent/internal/glob"
)

// One member's patterns can make matching cost what they like, within the
// shapes glob.CleanPattern accepts: a call stops matching once it has done
// maxGlobWork, lets through what it did not check, counts it and says so.

func joined(n int, seg string) string { return strings.TrimSuffix(strings.Repeat(seg+"/", n), "/") }

// chainPatterns are 50 patterns at glob.CleanPattern's bounds, 32 segments
// of which 4 are **, each costly to match against a deep path and matching
// none of them.
func chainPatterns(k int) []string {
	out := make([]string, 50)
	for i := range out {
		out[i] = fmt.Sprintf("%s/%s/zz%d_%d", joined(3, "**/"+joined(7, "*")), "**/"+joined(6, "*"), k, i)
	}
	return out
}

// ladderPatterns are 50 patterns of a ** and 30 literal segments, which a
// path of the same segment over and over almost matches at every depth.
func ladderPatterns(k int) []string {
	out := make([]string, 50)
	for i := range out {
		out[i] = fmt.Sprintf("**/%s/z%d_%d", joined(30, "a"), k, i)
	}
	return out
}

// widePatterns are 50 patterns of 15 wildcard segments of 64 bytes, costly
// to compare with each other.
func widePatterns(k int) []string {
	out := make([]string, 50)
	for i := range out {
		out[i] = joined(14, "*"+strings.Repeat("a", 62)+"b") + fmt.Sprintf("/*%s%02d%02d", strings.Repeat("a", 59), k, i)
	}
	return out
}

// deepPaths are n paths 63 segments deep, under dir/.
func deepPaths(n int, dir string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s/f%d.go", joined(62, dir), i)
	}
	return out
}

// hostileBoard has eve's patterns in four worktrees of the repository.
func hostileBoard(t *testing.T, patterns func(int) []string) *harness {
	t.Helper()
	h := newHarness(t)
	for k := range 4 {
		w := whereOf("eve")
		w.Worktree = fmt.Sprintf("/work/eve%d", k)
		if _, err := h.b.Declare(h.now, DeclareRequest{Member: "eve", Where: w, Patterns: patterns(k)}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// unboundedWork is what checking paths for member's claim would cost with no
// bound.
func unboundedWork(b *Board, member string, paths []PathRef) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.work = glob.NewWork(1 << 50)
	self := b.findClaim(member, whereOf(member))
	if self == nil {
		self = &claim{Repo: repo, Member: member}
	}
	b.newCheck(t0, self, &session{}, paths)
	return 1<<50 - b.work.Left()
}

// A pre_edit that meets patterns too costly to match all of stops matching
// at the bound, lets the edit through, tells its agent and counts it; and
// the same edit on a copy of the board stops at the same place.
func TestCostlyPatternsStopAnEditShort(t *testing.T) {
	h := hostileBoard(t, chainPatterns)
	paths := refs(deepPaths(maxCheckPaths, "d")...)
	if cost := unboundedWork(h.b, "bob", paths); cost < 5*maxGlobWork {
		t.Fatalf("checking the edit in full costs %d units, not enough to reach the bound", cost)
	}
	twin := New(h.b.cfg)
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := twin.Restore(data); err != nil {
		t.Fatal(err)
	}
	ev := HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentClaudeCode, SessionID: "b1", Where: whereOf("bob"), Tool: "Edit", Paths: paths}
	var res HookResult
	w1 := counted(t, func() { res, err = h.b.Hook(h.now, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionAllow || !res.Partial || !strings.Contains(res.Context, partialNote) {
		t.Fatalf("the edit was not let through with a note: %s %t %q", res.Decision, res.Partial, res.Context)
	}
	if st := h.b.View(h.now, repo).Stats; st.Partial != 1 {
		t.Fatalf("stats count %d edits stopped short, want 1", st.Partial)
	}
	var again HookResult
	w2 := counted(t, func() { again, err = twin.Hook(h.now, ev) })
	if err != nil {
		t.Fatal(err)
	}
	res.ClaimID, again.ClaimID = "", "" // bob's new claim is named at random on the copy
	if a, b := jsonOf(res), jsonOf(again); a != b || !sameWork(w1, w2) {
		t.Fatalf("the same edit on a copy of the board was answered differently:\n%s, %+v\n%s, %+v", a, w1, b, w2)
	}
	// Edits the bound does not reach are checked in full.
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "deep work", "d/**")
	res = h.hook(KindPreEdit, "bob", "b1", "d/x.go")
	if res.Decision != DecisionRefuse || res.Partial {
		t.Fatalf("an edit of alice's reservation: %s, partial %t", res.Decision, res.Partial)
	}
}

// A check and a declaration that meet costly patterns stop at the bound
// too, and their answers say so.
func TestCostlyPatternsStopChecksAndDeclarationsShort(t *testing.T) {
	h := hostileBoard(t, chainPatterns)
	res, err := h.b.Check(h.now, CheckRequest{Member: "bob", Where: whereOf("bob"), Paths: refs(deepPaths(maxCheckPaths, "d")...)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Partial || !strings.Contains(res.Text, partialNote) {
		t.Fatalf("the check did not say it stopped short: %t %q", res.Partial, res.Text)
	}

	h = hostileBoard(t, widePatterns)
	before := h.b.View(h.now, repo).Stats.Partial
	dr := h.declare("bob", ModeShared, "", widePatterns(9)...)
	if !dr.Partial || !strings.Contains(dr.Text, partialNote) {
		t.Fatalf("the declaration did not say it stopped short: %t %q", dr.Partial, dr.Text)
	}
	v := h.b.View(h.now, repo)
	if v.Stats.Partial != before+1 {
		t.Fatalf("stats count %d calls stopped short, want %d", v.Stats.Partial, before+1)
	}
	if want := fmt.Sprintf("%s stopped before comparing every teammate's pattern", plural(before+1, "check")); !strings.Contains(v.Text(), want) {
		t.Fatalf("the board's text does not count them:\n%s", v.Text())
	}
}

// On a board of ordinary work, the heaviest calls stay well inside the
// bound: an edit of 200 files, and a worktree arriving with 2000 changed.
func TestOrdinaryWorkStaysInsideTheBound(t *testing.T) {
	sh := smallShape()
	sh.claims, sh.busy, sh.sessions, sh.intents, sh.files, sh.areas, sh.perArea = 80, 60, 80, 1, 40, 50, 60
	sb := buildBoard(t, sh)
	var paths []PathRef
	for k := range maxCheckPaths {
		paths = append(paths, sb.path(sb.baseArea(1)+k%7, k))
	}
	ev := sb.event(1, KindPreEdit, paths...)
	if res := sb.hook(t, ev); res.Partial {
		t.Fatal("an edit of 200 files stopped short")
	}
	edit := maxGlobWork - sb.work.Left()
	ev = sb.fresh(1, 0, KindSessionStart)
	ev.Footprint = sb.footprint(100_000, 2000)
	sb.hook(t, ev)
	arrival := maxGlobWork - sb.work.Left()
	t.Logf("an edit of 200 files matched %d units, an arrival of 2000 files %d", edit, arrival)
	if sb.work.Short() || max(edit, arrival) > maxGlobWork/4 {
		t.Fatalf("ordinary work matched %d and %d units, more than a quarter of the bound", edit, arrival)
	}
}

// A snapshot from an older server, which accepted patterns this one does
// not, is restored without them; the rest are kept, cleaned and once each.
func TestRestoreDropsPatternsNoLongerAccepted(t *testing.T) {
	chain := joined(409, "**/*")[:1023]
	c := &claim{ID: "c_1", Repo: repo, Member: "eve", Host: "h", Worktree: "/w", Intents: []Intent{
		{Pattern: "a/**"}, {Pattern: chain}, {Pattern: "b/**/**/c"}, {Pattern: "a/**"},
	}}
	data, err := json.Marshal(snapshot{Format: snapshotFormat, Claims: []*claim{c}})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	b := New(DefaultConfig(), WithLogger(slog.New(slog.NewTextHandler(&log, nil))))
	if err := b.Restore(data); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, in := range b.claims["c_1"].Intents {
		got = append(got, in.Pattern)
	}
	if strings.Join(got, " ") != "a/** b/**/c" {
		t.Fatalf("restored intents %.200q, want a/** and b/**/c", got)
	}
	if n := strings.Count(log.String(), "dropping an intent"); n != 1 {
		t.Fatalf("logged %d dropped intents, want 1:\n%s", n, log.String())
	}
	mustIndex(t, b, "restoring")
}
