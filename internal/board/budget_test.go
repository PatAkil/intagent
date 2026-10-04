package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	if err := twin.Restore(bytes.NewReader(data)); err != nil {
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
	if err := b.Restore(bytes.NewReader(data)); err != nil {
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

// changesOf is a footprint of n files under dir, then the extra ones.
func changesOf(n int, dir string, extra ...string) *Footprint {
	files := make([]string, 0, n+len(extra))
	for i := range n {
		files = append(files, fmt.Sprintf("%s/pkg%02d/internal/util/file%04d.go", dir, i%40, i))
	}
	return &Footprint{Files: refs(append(files, extra...)...)}
}

// A worktree arriving with many changes, on a repository where teammates
// declared patterns rooted nowhere, costs more matching than one call may
// do. Its changes are compared with reservations first, so the one inside
// alice's is reported whatever the alerts to teammates cost; and the session
// is told that the call stopped short: at once, or after a stop, whose
// answer has no context, in its next answer, once.
func TestBreachesAreFoundBeforeAlertsRunShort(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Retry rework", "svc/pay/**")
	for i := range 60 {
		m := fmt.Sprintf("m%02d", i)
		h.hook(KindPrompt, m, m+"s")
		h.declare(m, ModeShared, "generated code", "**/*_mock.go", "**/*.pb.go", "**/testdata/*.golden")
	}
	bob := func(kind Kind, fp *Footprint) HookResult {
		t.Helper()
		res, err := h.b.Hook(h.now, HookEvent{Kind: kind, Member: "bob", Agent: AgentCodex, SessionID: "b1", Where: whereOf("bob"),
			Tool: "Bash", Footprint: fp})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	bob(KindPrompt, nil)
	reconcile := func(kind Kind, fp *Footprint) HookResult {
		t.Helper()
		res := bob(kind, fp)
		if !res.Partial {
			t.Fatalf("a %s with %d new files did not stop short", kind, len(fp.Files))
		}
		return res
	}
	res := reconcile(KindToolEnd, changesOf(1999, "lib", "svc/pay/retry.go"))
	acts := h.activities(ActivityConflict)
	if len(acts) != 1 || !acts[0].Breach || !slices.Equal(acts[0].Paths, []string{"svc/pay/retry.go"}) {
		t.Fatalf("the change inside alice's reservation was not reported: %+v", acts)
	}
	mustContain(t, res.Context, "svc/pay/retry.go, which alice's agent holds exclusively", shortChangesNote)

	// Two stops that run short, then a prompt: told once.
	if res := reconcile(KindStop, changesOf(2000, "lib2")); res.Context != "" {
		t.Fatalf("a stop answered %q", res.Context)
	}
	reconcile(KindStop, changesOf(2000, "lib3"))
	next := bob(KindPrompt, nil)
	if n := strings.Count(next.Context, shortChangesNote); n != 1 {
		t.Fatalf("after two stops that ran short, the next answer says so %d times:\n%s", n, next.Context)
	}
	if st := h.b.View(h.now, repo).Stats; st.Partial != 3 {
		t.Fatalf("stats count %d calls stopped short, want 3", st.Partial)
	}
}

// A worktree arriving with 2000 changed files, on a repository of 300
// claims with 4 intents each, is compared in full and well inside the bound:
// each intent is matched against the files under the directory it is rooted
// in, not against every file. Every teammate whose intent covers a file is
// alerted, and the change inside a reservation is reported.
func TestArrivalOnABusyRepositoryIsComparedInFull(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Retry rework", "svc/pay/**")
	for i := range 300 {
		m := fmt.Sprintf("m%03d", i)
		h.hook(KindPrompt, m, m+"s")
		h.declare(m, ModeShared, "work", fmt.Sprintf("svc/a%03d/**", i), fmt.Sprintf("svc/b%03d/**", i), fmt.Sprintf("lib/c%03d/**", i),
			fmt.Sprintf("docs/d%03d.md", i))
	}
	h.hook(KindPrompt, "bob", "b1")
	files := []string{"svc/pay/retry.go"}
	for i := range 30 {
		files = append(files, fmt.Sprintf("lib/c%03d/x.go", 10*i))
	}
	for i := len(files); i < 2000; i++ {
		files = append(files, fmt.Sprintf("web/pkg%02d/file%04d.ts", i%40, i))
	}
	res, err := h.b.Hook(h.now, HookEvent{Kind: KindToolEnd, Member: "bob", Agent: AgentCodex, SessionID: "b1", Where: whereOf("bob"),
		Tool: "Bash", Footprint: &Footprint{Files: refs(files...)}})
	if err != nil {
		t.Fatal(err)
	}
	used := maxGlobWork - h.b.work.Left()
	t.Logf("an arrival of %d files on 300 claims of 4 intents matched %d units", len(files), used)
	if res.Partial || used > 4*len(files) {
		t.Fatalf("the arrival matched %d units (partial %t), more than 4 for each of its %d files", used, res.Partial, len(files))
	}
	if acts := h.activities(ActivityConflict); len(acts) != 1 || !acts[0].Breach {
		t.Fatalf("the change inside alice's reservation was not reported: %+v", acts)
	}
	bob := h.b.findClaim("bob", whereOf("bob"))
	for i := range 300 {
		m := fmt.Sprintf("m%03d", i)
		var alerts []string
		for _, it := range h.b.findClaim(m, whereOf(m)).Inbox {
			if it.Kind == "overlap" && it.FromClaim == bob.ID {
				alerts = append(alerts, it.Paths...)
			}
		}
		if want := []string(nil); i%10 == 0 {
			want = []string{fmt.Sprintf("lib/c%03d/x.go", i)}
			if !slices.Equal(alerts, want) {
				t.Fatalf("%s was alerted of %q, want %q", m, alerts, want)
			}
		} else if len(alerts) > 0 {
			t.Fatalf("%s was alerted of %q, which it did not declare", m, alerts)
		}
	}
}

// Once a call has done all the matching it may, it stops: it compares no
// further claims, reservation holders or files, however many there are. On
// a board where one member holds 400 worktrees of costly patterns, each
// kind of call that matches makes no more glob calls after it ran out than
// the intents of the claim it was comparing then.
func TestCallsStopOnceTheWorkIsSpent(t *testing.T) {
	board := func(patterns func(int) []string, mode Mode) *harness {
		h := newHarness(t)
		for k := range 400 {
			c := &claim{ID: fmt.Sprintf("c_eve%05d", k), Repo: repo, Member: "eve", Host: "h", Worktree: fmt.Sprintf("/work/eve%d", k),
				CreatedAt: h.now, UpdatedAt: h.now}
			for _, p := range patterns(k) {
				c.Intents = append(c.Intents, Intent{Pattern: p, Mode: mode, DeclaredAt: h.now})
			}
			h.b.addClaim(c)
			// Live, so that its reservations count.
			h.b.attachSession(&session{Key: c.ID, ID: "e", Member: "eve", Agent: AgentCodex, LastSeen: h.now, Phase: phaseWorking}, c.ID)
		}
		return h
	}
	wideK := func(k int) []string { return widePatterns(k % 90) } // two digits for k, as glob.CleanPattern requires
	chains, reserved := board(chainPatterns, ModeShared), board(chainPatterns, ModeExclusive)
	wide, wideReserved := board(wideK, ModeShared), board(wideK, ModeExclusive)
	bob := whereOf("bob")
	deep := refs(deepPaths(maxCheckPaths, "d")...)
	for _, tc := range []struct {
		name string
		h    *harness
		call func(h *harness) error
	}{
		{"a check", chains, func(h *harness) error {
			_, err := h.b.Check(h.now, CheckRequest{Member: "bob", Where: bob, Paths: deep})
			return err
		}},
		{"an edit", chains, func(h *harness) error {
			_, err := h.b.Hook(h.now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "b1", Where: bob, Paths: deep})
			return err
		}},
		{"an arrival", chains, func(h *harness) error {
			_, err := h.b.Hook(h.now, HookEvent{Kind: KindSessionStart, Member: "bob", Agent: AgentCodex, SessionID: "b2", Where: bob,
				Footprint: changesOf(2000, "lib")})
			return err
		}},
		{"an arrival among reservations", reserved, func(h *harness) error {
			_, err := h.b.Hook(h.now, HookEvent{Kind: KindSessionStart, Member: "bob", Agent: AgentCodex, SessionID: "b2", Where: bob,
				Footprint: changesOf(2000, "lib")})
			return err
		}},
		{"a note to a path", chains, func(h *harness) error {
			_, err := h.b.Note(h.now, NoteRequest{Member: "bob", Where: bob, To: deep[0].Path, Text: "hello"})
			if errors.Is(err, ErrNoTarget) {
				err = nil
			}
			return err
		}},
		{"a declaration", wide, func(h *harness) error {
			_, err := h.b.Declare(h.now, DeclareRequest{Member: "bob", Where: bob, Patterns: widePatterns(95), Mode: ModeShared})
			return err
		}},
		{"an exclusive declaration", wideReserved, func(h *harness) error {
			_, err := h.b.Declare(h.now, DeclareRequest{Member: "bob", Where: bob, Patterns: widePatterns(95), Mode: ModeExclusive})
			return err
		}},
	} {
		var err error
		w := counted(t, func() { err = tc.call(tc.h) })
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		t.Logf("%s: %d matches and %d overlaps, %d once out of work", tc.name, w.globMatch, w.globOverlap, w.spentMatches)
		if !tc.h.b.work.Short() {
			t.Fatalf("%s did not run out of work", tc.name)
		}
		if w.spentMatches > 3*maxIntents {
			t.Fatalf("%s made %d glob calls after it ran out of work, more than one claim's intents", tc.name, w.spentMatches)
		}
	}
}

// An exclusive intent the call could not finish comparing with teammates'
// reservations is rejected, not accepted: it could overlap one of them, and
// two claims would then hold the same files. Declared shared, it is
// accepted.
func TestShortClashCheckRejectsAReservation(t *testing.T) {
	h := newHarness(t)
	const retry = "services/payments/internal/retry/**"
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Retry rework", retry)
	// Eve's live reservations, costly to compare with retry and never
	// overlapping it, come first in ID order.
	seg := func(w string) string { return strings.Repeat("*", 60-len(w)) + w }
	costly := seg("services") + "/" + seg("payments") + "/" + seg("internal") + "/" + seg("retrx")
	for k := range 700 {
		c := &claim{ID: fmt.Sprintf("c_0eve%05d", k), Repo: repo, Member: "eve", Host: "h", Worktree: fmt.Sprintf("/work/eve%d", k),
			CreatedAt: h.now, UpdatedAt: h.now}
		for i := range 50 {
			c.Intents = append(c.Intents, Intent{Pattern: fmt.Sprintf("%s%d/z", costly, i), Mode: ModeExclusive, DeclaredAt: h.now})
		}
		h.b.addClaim(c)
		h.b.attachSession(&session{Key: c.ID, ID: "e", Member: "eve", Agent: AgentCodex, LastSeen: h.now, Phase: phaseWorking}, c.ID)
	}
	h.hook(KindPrompt, "bob", "b1")
	res := h.declare("bob", ModeExclusive, "mine now", retry)
	if !res.Partial || len(res.Accepted) != 0 || len(res.Rejected) != 1 {
		t.Fatalf("an exclusive intent over alice's, not compared with hers: partial %t, accepted %v, rejected %v",
			res.Partial, res.Accepted, res.Rejected)
	}
	mustContain(t, res.Text, "Not declared: "+retry+". intagent could not finish comparing it with teammates' reservations")
	if res := h.declare("bob", ModeShared, "mine too", retry); len(res.Accepted) != 1 {
		t.Fatalf("the same intent, shared, was not accepted: %+v", res)
	}
}
