package board

import (
	"bytes"
	"fmt"
	"maps"
	"math"
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

// A note to a member goes to their claims still listening. One to a member
// none of whose claims listens waits in the one they were last active in,
// not in every worktree they left; so does one to whoever changed a path.
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
	note := func(to string) []string {
		t.Helper()
		h.advance(time.Minute)
		res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: to, Text: "go.mod is mine today"})
		if err != nil {
			t.Fatalf("note to %s: %v", to, err)
		}
		return res.Delivered
	}
	w1, w2, w3 := h.claimIn("bob", "w1").ID, h.claimIn("bob", "w2").ID, h.claimIn("bob", "w3").ID
	carol := h.claimIn("carol", "c").ID
	if got := note("bob"); !slices.Equal(got, []string{w2, w3}) {
		t.Errorf("a note to bob went to %v, want his claims quiet for less than a day, %v", got, []string{w2, w3})
	}
	h.advance(15 * time.Hour)
	if got := note("bob"); !slices.Equal(got, []string{w3}) {
		t.Errorf("a note to bob, away from every claim, went to %v, want only the newest, %s", got, w3)
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
