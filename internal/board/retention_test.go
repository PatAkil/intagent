package board

import (
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
