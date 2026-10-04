package board

import (
	"bytes"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// at runs a hook from member's worktree wt, which whereOf does not name.
func (h *harness) at(kind Kind, member, wt, session string, paths ...string) HookResult {
	h.t.Helper()
	w := whereOf(member)
	w.Worktree = "/work/" + member + "/" + wt
	ev := HookEvent{Kind: kind, Member: member, Agent: AgentClaudeCode, SessionID: session, Where: w, Paths: refs(paths...)}
	if kind == KindPreEdit || kind == KindPostEdit {
		ev.Tool = "Edit"
	}
	res, err := h.b.Hook(h.now, ev)
	if err != nil {
		h.t.Fatalf("Hook(%s, %s in %s): %v", kind, member, wt, err)
	}
	return res
}

// claimIn is member's claim in worktree wt, as at names it.
func (h *harness) claimIn(member, wt string) *claim {
	h.t.Helper()
	w := whereOf(member)
	w.Worktree = "/work/" + member + "/" + wt
	c := h.b.findClaim(member, w)
	if c == nil {
		h.t.Fatalf("%s has no claim in %s", member, wt)
	}
	return c
}

// A claim nobody has worked in for longer than DormantFor is told nothing:
// no alert of a teammate's change to its files, no intent over them, and
// nothing remembered of either. One quiet for less is told as before.
func TestOnlyListeningClaimsAreTold(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bob", "old", "b1", "go.mod", "api/user.go")
	h.at(KindSessionEnd, "bob", "old", "b1")
	h.advance(2 * time.Hour)
	h.at(KindPostEdit, "carol", "recent", "c1", "go.mod", "api/user.go")
	h.at(KindSessionEnd, "carol", "recent", "c1")
	// bob's claim has been quiet for a day and two hours, carol's for a day.
	h.advance(24 * time.Hour)
	h.hook(KindPostEdit, "alice", "a1", "go.mod")
	h.declare("alice", ModeShared, "Rework the user API", "api/**")

	// What alice's work queued for each, and the alerts of it each remembers.
	alice := h.b.findClaim("alice", whereOf("alice")).ID
	fromAlice := func(c *claim) (kinds []string, alerts int) {
		for _, it := range c.Inbox {
			if it.From == "alice" {
				kinds = append(kinds, it.Kind)
			}
		}
		for k := range c.Alerted {
			if strings.Contains(k, "|"+alice+"|") {
				alerts++
			}
		}
		return kinds, alerts
	}
	if kinds, alerts := fromAlice(h.claimIn("bob", "old")); len(kinds) != 0 || alerts != 0 {
		t.Errorf("bob's claim, quiet for 26h, was queued %v and remembers %d alerts", kinds, alerts)
	}
	if kinds, alerts := fromAlice(h.claimIn("carol", "recent")); !slices.Equal(kinds, []string{"overlap", "intent"}) || alerts != 1 {
		t.Errorf("carol's claim, quiet for 24h, was queued %v and remembers %d alerts; want an overlap and an intent, and one alert",
			kinds, alerts)
	}
	// The quiet claim still counts as nearby work for an edit.
	res := h.hook(KindPreEdit, "alice", "a1", "api/user.go")
	if len(res.Conflicts) != 2 {
		t.Fatalf("alice's edit of api/user.go met %d conflicts, want bob's and carol's", len(res.Conflicts))
	}
}

// A note to a member goes to their claims still listening, not to every
// worktree they left. One to a member none of whose claims listens waits for
// their next session (mail.go). One to whoever changed a path goes to those
// still listening, and for each member none of whose claims there listens,
// to the one they were last active in.
func TestNotesGoToClaimsStillListening(t *testing.T) {
	h := newHarness(t)
	for _, wt := range []string{"w1", "w2", "w3"} {
		h.at(KindPostEdit, "bob", wt, "b-"+wt, "go.mod")
		h.at(KindSessionEnd, "bob", wt, "b-"+wt)
		h.advance(10 * time.Hour)
	}
	h.at(KindPostEdit, "carol", "c", "c1", "go.mod")
	h.at(KindSessionEnd, "carol", "c", "c1")
	// bob's claims have been quiet for 30, 20 and 10 hours.
	var held string
	note := func(to string) []string {
		t.Helper()
		h.advance(time.Minute)
		res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: to, Text: "go.mod is mine today"})
		if err != nil {
			t.Fatalf("note to %s: %v", to, err)
		}
		held = res.HeldFor
		return res.Delivered
	}
	w1, w2, w3 := h.claimIn("bob", "w1").ID, h.claimIn("bob", "w2").ID, h.claimIn("bob", "w3").ID
	carol := h.claimIn("carol", "c").ID
	if got := note("bob"); !slices.Equal(got, []string{w2, w3}) {
		t.Errorf("a note to bob went to %v, want his claims quiet for less than a day, %v", got, []string{w2, w3})
	}
	h.advance(15 * time.Hour)
	if got := note("bob"); len(got) != 0 || held != "bob" {
		t.Errorf("a note to bob, away from every claim, went to %v, held for %q; want it held for bob", got, held)
	}
	if got := note("go.mod"); !slices.Equal(got, []string{w3, carol}) {
		t.Errorf("a note to whoever changed go.mod went to %v, want bob's newest claim and carol's, %v", got, []string{w3, carol})
	}
	// A claim named by ID hears it, listening or not.
	if got := note(w1); !slices.Equal(got, []string{w1}) {
		t.Errorf("a note to claim %s went to %v", w1, got)
	}
}

// scanAt reports a git scan of member's worktree wt with a heartbeat.
func (h *harness) scanAt(member, wt, session string, files ...string) {
	h.t.Helper()
	w := whereOf(member)
	w.Worktree = "/work/" + member + "/" + wt
	ev := HookEvent{Kind: KindHeartbeat, Member: member, Agent: AgentClaudeCode, SessionID: session, Where: w,
		Footprint: &Footprint{Files: refs(files...)}}
	if _, err := h.b.Hook(h.now, ev); err != nil {
		h.t.Fatalf("scan of %s's %s: %v", member, wt, err)
	}
}

// Sweep drops inbox items no session can be shown any more, from every
// claim, and the alerts a claim no longer listening remembers.
func TestSweepTidiesInboxesAndAlerts(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bob", "w", "b1", "go.mod")
	h.at(KindSessionEnd, "bob", "w", "b1")
	h.at(KindPostEdit, "carol", "w", "c1", "go.mod")
	h.hook(KindPostEdit, "alice", "a1", "go.mod")
	bob, carol := h.claimIn("bob", "w"), h.claimIn("carol", "w")
	if len(bob.Inbox) != 2 || len(bob.Alerted) != 2 || len(carol.Inbox) != 1 || len(carol.Alerted) != 1 {
		t.Fatalf("before: bob %d items, %d alerts; carol %d items, %d alerts", len(bob.Inbox), len(bob.Alerted), len(carol.Inbox), len(carol.Alerted))
	}
	// carol's agent goes on working; bob's left a day ago.
	for range 24 {
		h.advance(time.Hour)
		h.at(KindPrompt, "carol", "w", "c1")
	}
	h.advance(time.Minute)
	v := h.b.Version()
	h.b.Sweep(h.now)
	if len(bob.Inbox) != 0 || bob.Alerted != nil {
		t.Errorf("bob's claim, quiet for a day, keeps %d items and %d alerts", len(bob.Inbox), len(bob.Alerted))
	}
	if len(carol.Inbox) != 0 || len(carol.Alerted) != 1 {
		t.Errorf("carol's claim keeps %d expired items and %d alerts, want none and 1", len(carol.Inbox), len(carol.Alerted))
	}
	if h.b.Version() == v {
		t.Error("the board was not marked changed, so it would not be saved")
	}
}

// Alerts of a claim no longer on the board are dropped by the next sweep, in
// the claims that remember them, and in a board restored from a snapshot
// that still holds them: a claim that lives for weeks would otherwise keep
// one for every short-lived claim that touched its files.
func TestSweepPrunesAlertsOfRemovedClaims(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Rework retries", "retry/**")
	h.hook(KindPostEdit, "alice", "a1", "x.go")
	h.scanAt("bob", "w", "b1", "x.go", "retry/r.go") // the second breaches alice's reservation
	h.scanAt("carol", "w", "c1", "x.go")
	alice, bob := h.b.findClaim("alice", whereOf("alice")), h.claimIn("bob", "w")
	carolID := h.claimIn("carol", "w").ID
	keys := func(c *claim) []string { return slices.Sorted(maps.Keys(c.Alerted)) }
	want := func(c *claim, ids ...string) {
		t.Helper()
		var got []string
		for _, k := range keys(c) {
			got = append(got, alertedClaim(k))
		}
		slices.Sort(got)
		if !slices.Equal(got, ids) {
			t.Errorf("%s's claim remembers alerts of %v, want %v (keys %v)", c.Member, got, ids, keys(c))
		}
	}
	want(bob, alice.ID, carolID)
	want(alice, bob.ID, bob.ID, carolID)

	// carol's work is merged: her claim is released.
	h.scanAt("carol", "w", "c1")
	h.at(KindSessionEnd, "carol", "w", "c1")
	if h.b.claims[carolID] != nil {
		t.Fatal("carol's claim was not released")
	}
	h.b.Sweep(h.now)
	want(bob, alice.ID)
	want(alice, bob.ID, bob.ID)

	// A snapshot that still holds alerts of a claim removed long ago heals.
	bob.Alerted["touch|c_gone|x.go"] = true
	bob.Alerted["unchecked|deny|c_gone|retry/**|1|retry/r.go"] = true
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig())
	if err := b.Restore(bytes.NewReader(data), h.now); err != nil {
		t.Fatal(err)
	}
	b.Sweep(h.now)
	want(b.claims[bob.ID], alice.ID)
}

// Over a month of short-lived worktrees touching the files of claims that
// live on, what the long-lived claims remember stays as it was after a
// week: the alerts of claims since removed go with them. Before, they grew
// by one key a day for each short-lived claim and long-lived one that
// shared a file, and no restart dropped them.
func TestAlertsPlateauOverAMonth(t *testing.T) {
	h := newHarness(t)
	const long, freshPerDay = 10, 150
	hot := []string{"go.mod", "go.sum"}
	keys := func() int {
		n := 0
		for _, c := range h.b.claims {
			n += len(c.Alerted)
		}
		return n
	}
	day8, fresh := 0, 0
	for day := range 30 {
		for hour := range 24 {
			if hour%12 == 0 {
				for l := range long {
					wt := fmt.Sprintf("main%d", l)
					h.at(KindPrompt, fmt.Sprintf("l%d", l), wt, "s-"+wt)
					h.at(KindPostEdit, fmt.Sprintf("l%d", l), wt, "s-"+wt, hot[l%2], fmt.Sprintf("own%d/f.go", l))
				}
			}
			for range freshPerDay / 24 {
				fresh++
				m, wt, s := fmt.Sprintf("f%d", fresh%40), fmt.Sprintf("task%d", fresh), fmt.Sprintf("t%d", fresh)
				h.scanAt(m, wt, s, hot[fresh%2], fmt.Sprintf("pkg%d/f.go", fresh%17))
				h.advance(10 * time.Minute)
				h.scanAt(m, wt, s) // merged
				h.at(KindSessionEnd, m, wt, s)
			}
			h.advance(time.Hour - time.Duration(freshPerDay/24)*10*time.Minute)
			h.b.Sweep(h.now)
		}
		if day == 7 {
			day8 = keys()
		}
	}
	day30 := keys()
	t.Logf("alerted keys after 8 days: %d; after 30: %d; %d short-lived claims", day8, day30, fresh)
	if day8 == 0 || math.Abs(float64(day30-day8)) > 0.01*float64(day8) {
		t.Fatalf("alerted keys after 30 days %d, after 8 %d: want them within 1%%", day30, day8)
	}
}

// A claim remembers being told of at most maxToldPaths files a teammate's
// claim changed, and an alert's item lists as many: the text counts the
// rest. A file it was told of and does not remember is told again when it
// comes back into the teammate's changes.
func TestAlertsRememberAFewFilesPerTeammate(t *testing.T) {
	h := newHarness(t)
	files := make([]string, 10)
	for i := range files {
		files[i] = fmt.Sprintf("api/f%02d.go", i)
	}
	h.scanAt("alice", "w", "a1", files...)
	h.scanAt("bob", "w", "b1", files...)
	alice, bob := h.claimIn("alice", "w"), h.claimIn("bob", "w")
	if len(alice.Inbox) != 1 || !slices.Equal(alice.Inbox[0].Paths, files[:5]) ||
		!strings.Contains(alice.Inbox[0].Text, "api/f04.go and 5 more") {
		t.Fatalf("alice's item: %+v", alice.Inbox)
	}
	if len(alice.Alerted) != maxToldPaths || alice.Told[bob.ID] != maxToldPaths {
		t.Fatalf("alice remembers %d alerts, and counts %d of bob's claim; want %d", len(alice.Alerted), alice.Told[bob.ID], maxToldPaths)
	}
	// bob's worktree drops the files, then changes them again.
	h.scanAt("bob", "w", "b1")
	h.scanAt("bob", "w", "b1", files...)
	if len(alice.Inbox) != 2 || !slices.Equal(alice.Inbox[1].Paths, files[5:]) || len(alice.Alerted) != maxToldPaths {
		t.Fatalf("after bob changed them again: items %+v, %d alerts", alice.Inbox, len(alice.Alerted))
	}
}

// Claims that share a large footprint, as branches stacked on a long-lived
// one do, remember a few alerts per pair, not one per pair and file. Before,
// 76 such claims of 2000 files held 5.7 million keys, and inbox items that
// held every path, 627 MB.
func TestSharedFootprintsAreRememberedPerPair(t *testing.T) {
	const claims, files = 76, 2000
	fp := make([]string, files)
	for i := range fp {
		fp[i] = fmt.Sprintf("services/svc%02d/pkg/file%04d.go", i%40, i)
	}
	h := newHarness(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range claims {
		h.advance(time.Second)
		h.scanAt(fmt.Sprintf("m%02d", i), "stack", "s", fp...)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	keys, paths, items := 0, 0, 0
	for _, c := range h.b.claims {
		keys += len(c.Alerted)
		for _, it := range c.Inbox {
			items++
			paths += len(it.Paths)
		}
	}
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d claims sharing %d files: %d alerted keys, %d items holding %d paths; heap +%.1f MB",
		claims, files, keys, items, paths, float64(heap)/(1<<20))
	if keys > claims*(claims-1)/2*maxToldPaths || paths > items*maxToldPaths {
		t.Errorf("%d keys and %d paths in %d items, want at most %d and %d", keys, paths, items,
			claims*(claims-1)/2*maxToldPaths, items*maxToldPaths)
	}
	if heap > 50<<20 {
		t.Errorf("the board grew by %.1f MB, want under 50", float64(heap)/(1<<20))
	}
}

// inboxRig is a waiting agent sent a note, then a flood of other news
// before its next hook: the scale review's inbox scenarios.
type inboxRig struct {
	*harness
	victim Where
}

const floodNote = "please do not merge retry.go before 5pm, migration running"

func floodWhere(m string, i int) Where {
	return Where{Repo: repo, Host: m + "-host", Worktree: fmt.Sprintf("/w/%s/%d", m, i), Branch: "feat/" + m}
}

func (r *inboxRig) post(m, session string, w Where, paths ...string) HookResult {
	r.t.Helper()
	res, err := r.b.Hook(r.now, HookEvent{Kind: KindPostEdit, Member: m, Agent: AgentClaudeCode, SessionID: session, Where: w,
		Tool: "Edit", Paths: refs(paths...)})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// newInboxRig: the victim's agent changed files (and may have declared a
// shared intent), stopped and waits for its person; carol sends it a note.
func newInboxRig(t *testing.T, files []string, intent string) *inboxRig {
	r := &inboxRig{harness: newHarness(t), victim: floodWhere("victim", 0)}
	for _, f := range files {
		r.post("victim", "v", r.victim, f)
	}
	if intent != "" {
		if _, err := r.b.Declare(r.now, DeclareRequest{Member: "victim", Where: r.victim, Patterns: []string{intent}, Mode: ModeShared}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.b.Hook(r.now, HookEvent{Kind: KindStop, Member: "victim", Agent: AgentClaudeCode, SessionID: "v", Where: r.victim}); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	if res, err := r.b.Note(r.now, NoteRequest{Member: "carol", Where: floodWhere("carol", 0), To: "victim", Text: floodNote}); err != nil || len(res.Delivered) != 1 {
		t.Fatalf("note: %+v, %v", res, err)
	}
	return r
}

// heard runs the victim's next 20 prompts and reports whether its agent
// heard the note.
func (r *inboxRig) heard() bool {
	r.advance(time.Minute)
	for range 20 {
		res, err := r.b.Hook(r.now, HookEvent{Kind: KindPrompt, Member: "victim", Agent: AgentClaudeCode, SessionID: "v", Where: r.victim})
		if err != nil {
			r.t.Fatal(err)
		}
		if strings.Contains(res.Context, "migration running") {
			return true
		}
	}
	return false
}

// A note waiting for an agent survives whatever else fills its inbox before
// the agent's next hook: a flood from one teammate's many worktrees, from
// one worktree, of intents, of notes, and the ordinary news of fifty
// teammates. Each used to push it out of the 50-item inbox unheard.
func TestNoteSurvivesAFullInbox(t *testing.T) {
	files := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("services/payments/f%02d.go", i)
		}
		return out
	}
	retry := []string{"services/payments/retry.go"}
	for _, sc := range []struct {
		name   string
		files  []string
		intent string
		flood  func(r *inboxRig)
	}{
		{"A: one post_edit of retry.go from each of 50 worktrees of one member", retry, "", func(r *inboxRig) {
			for i := range 50 {
				r.post("evil", fmt.Sprint("e", i), floodWhere("evil", i), retry...)
			}
		}},
		{"B: 50 post_edits of the victim's 50 files from one worktree", files(50), "", func(r *inboxRig) {
			for _, f := range files(50) {
				r.post("evil", "e", floodWhere("evil", 0), f)
			}
		}},
		{"C: 50 post_edits of new files under the victim's shared intent", retry, "services/payments/**", func(r *inboxRig) {
			for i := range 50 {
				r.post("evil", "e", floodWhere("evil", 0), fmt.Sprintf("services/payments/new%02d.go", i))
			}
		}},
		{"D: the same shared intent declared 50 times", retry, "", func(r *inboxRig) {
			for range 50 {
				if _, err := r.b.Declare(r.now, DeclareRequest{Member: "evil", Where: floodWhere("evil", 0),
					Patterns: []string{"services/payments/**"}, Mode: ModeShared}); err != nil {
					r.t.Fatal(err)
				}
			}
		}},
		{"E: 50 notes from one member, within the rate limit", retry, "", func(r *inboxRig) {
			for i := range 50 {
				r.advance(3 * time.Second)
				if _, err := r.b.Note(r.now, NoteRequest{Member: "evil", Where: floodWhere("evil", 0), To: "victim", Text: fmt.Sprint("ping ", i)}); err != nil {
					r.t.Fatal(err)
				}
			}
		}},
		{"F: 50 members' agents each change go.mod once over 8 hours", append(retry, "go.mod"), "", func(r *inboxRig) {
			for i := range 50 {
				r.advance(8 * time.Hour / 50)
				m := fmt.Sprintf("m%02d", i)
				r.post(m, "s", floodWhere(m, 0), "go.mod")
			}
		}},
		{"H: one teammate refactors 50 files under the victim's shared intent", retry, "services/payments/**", func(r *inboxRig) {
			w := floodWhere("bob", 0)
			for i := range 50 {
				r.advance(2 * time.Hour / 50)
				f := fmt.Sprintf("services/payments/ledger/l%02d.go", i)
				ev := HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentClaudeCode, SessionID: "b", Where: w, Tool: "Edit", Paths: refs(f)}
				for range 2 { // a refusal, then its retry
					if res, err := r.b.Hook(r.now, ev); err != nil || res.Decision == DecisionAllow {
						break
					}
				}
				r.post("bob", "b", w, f)
			}
		}},
	} {
		r := newInboxRig(t, sc.files, sc.intent)
		sc.flood(r)
		if !r.heard() {
			t.Errorf("%s: the victim's agent never heard carol's note", sc.name)
		}
	}
}

// A full inbox lets go first of an item some session was shown, then of the
// oldest item of the sender holding the most, an alert before a note when
// senders tie.
func TestInboxEvictionOrder(t *testing.T) {
	item := func(from, kind string, shown bool) InboxItem {
		it := InboxItem{From: from, Kind: kind}
		if shown {
			it.DeliveredTo = map[string]bool{"s": true}
		}
		return it
	}
	for _, tc := range []struct {
		in   []InboxItem
		want int
	}{
		{[]InboxItem{item("a", "note", false), item("b", "overlap", false), item("b", "overlap", true)}, 2},
		{[]InboxItem{item("a", "note", false), item("b", "overlap", false), item("b", "note", false)}, 1},
		{[]InboxItem{item("a", "note", false), item("b", "overlap", false)}, 1},
		{[]InboxItem{item("a", "intent", false), item("b", "overlap", false)}, 0},
		{[]InboxItem{item("a", "note", false), item("b", "note", false)}, 0},
	} {
		if got := evictIndex(tc.in); got != tc.want {
			t.Errorf("evictIndex(%+v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// fleetDay runs one working day of a fleet: each slot's agent does runs
// of an hour or so, half of them in a fresh worktree of their own, most left
// unmerged when the run ends; some edit files everyone touches, and some
// send a note to a teammate.
func fleetDay(h *harness, rng *rand.Rand, day, members, slots, runs int) {
	hot := []string{"go.mod", "go.sum", "CHANGELOG.md"}
	for run := range runs {
		for s := range members * slots {
			m := fmt.Sprintf("m%02d", s%members)
			wt := fmt.Sprintf("slot%02d", s)
			if rng.Intn(2) == 0 {
				wt = fmt.Sprintf("task-d%d-r%d-s%02d", day, run, s)
			}
			sid := fmt.Sprintf("%s-d%d-r%d", wt, day, run)
			area := fmt.Sprintf("svc%02d", (s*7+run)%40)
			var files []string
			for f := range 4 + rng.Intn(8) {
				files = append(files, fmt.Sprintf("%s/pkg%d/file%02d.go", area, f%3, rng.Intn(30)))
			}
			if rng.Intn(4) == 0 {
				files = append(files, hot[rng.Intn(len(hot))])
			}
			h.scanAt(m, wt, sid, files...)
			h.at(KindPrompt, m, wt, sid)
			for _, f := range files[:3] {
				h.at(KindPostEdit, m, wt, sid, f)
			}
			if rng.Intn(10) == 0 {
				w := whereOf(m)
				w.Worktree = "/work/" + m + "/" + wt
				_, _ = h.b.Note(h.now, NoteRequest{Member: m, Where: w, To: fmt.Sprintf("m%02d", rng.Intn(members)), Text: "heads up"})
			}
			if rng.Intn(5) == 0 {
				h.scanAt(m, wt, sid) // merged: nothing left to track
			}
			h.at(KindSessionEnd, m, wt, sid)
			h.advance(time.Duration(rng.Intn(20)) * time.Second)
		}
		h.advance(time.Hour)
		h.b.Sweep(h.now)
	}
	for range 24 - runs { // the night
		h.advance(time.Hour)
		h.b.Sweep(h.now)
	}
}

// Nine days of a fleet: a claim no agent listens on is queued nothing and
// remembers no alert, no item is kept past its day, the claims with no agent
// running stay under the bound, and what the board holds per claim stays
// small. Before,
// this fleet left 525 claims holding 4,105 items and 9,975 alerts, 6.7 KB of
// heap a claim; the scale review's soak, 11.3 KB.
func TestFleetPlateausOverNineDays(t *testing.T) {
	const maxDormant = 400
	h := newHarness(t, func(c *Config) { c.MaxDormantClaims = maxDormant })
	h.b.notify = nil // the harness would keep every activity
	rng := rand.New(rand.NewSource(1))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for day := range 9 {
		fleetDay(h, rng, day, 8, 3, 8)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	live := h.b.liveAt(h.now)
	items, alerts, dormant := 0, 0, 0
	for _, c := range h.b.claims {
		items += len(c.Inbox)
		alerts += len(c.Alerted)
		if !live.claim(c.ID) {
			dormant++
		}
		if !h.b.listening(live, c) && len(c.Alerted) > 0 {
			t.Errorf("claim %s, quiet since %s, holds %d alerts", c.ID, c.UpdatedAt, len(c.Alerted))
		}
		for _, it := range c.Inbox {
			if h.now.Sub(it.At) >= inboxTTL {
				t.Errorf("claim %s holds an item from %s", c.ID, it.At)
			}
		}
	}
	perClaim := float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)) / float64(len(h.b.claims))
	t.Logf("after 9 days: %d claims, %d with no agent running, %d inbox items, %d alerts; %.0f bytes of heap a claim",
		len(h.b.claims), dormant, items, alerts, perClaim)
	if dormant > maxDormant || perClaim > 5.5*1024 {
		t.Fatalf("%d claims with no agent running, %.0f bytes a claim; want at most %d and 5.5 KB", dormant, perClaim, maxDormant)
	}
}
