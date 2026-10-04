package board

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A claim keeps the 20 sessions that ended last, so a client that starts a
// session for every event leaves no more on it than that: a teammate's edit
// that asks whether the claim is live reads them, not 20,000.
func TestEndedSessionsPerClaimAreBounded(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bot", "w", "s-first", "svc/x.go")
	for i := range 20_000 {
		s := fmt.Sprint("s", i)
		h.at(KindHeartbeat, "bot", "w", s)
		h.at(KindSessionEnd, "bot", "w", s)
	}
	// Besides those 20: the live one, and the last to end, which the next
	// session to start makes room for.
	c := h.claimIn("bot", "w")
	if n := len(h.b.claimSessions[c.ID]); n > maxEndedSessions+2 {
		t.Fatalf("the claim keeps %d sessions", n)
	}
	w := counted(t, func() { h.hook(KindPreEdit, "alice", "a1", "svc/x.go") })
	if w.sessionVisits > maxEndedSessions+2 {
		t.Fatalf("alice's edit read %d sessions to learn whether bot's claim is live", w.sessionVisits)
	}
}

// A session the board has no room for is checked but not stored: its edits
// are judged against teammates' work, a reservation still refuses them, and
// what would refuse once warns, since nothing remembers the warning was
// said. Its start says so. A sweep makes room by dropping the sessions not
// live that went silent longest.
func TestSessionsPastTheCapAreCheckedNotStored(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSessions = 10 })
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Rework retries", "retry/**")
	h.at(KindPostEdit, "bob", "w", "b1", "shared.go")
	for i := range 8 {
		h.at(KindPrompt, "fleet", fmt.Sprint("w", i), fmt.Sprint("f", i))
	}
	claims, acts := len(h.b.claims), len(h.acts)
	start := h.hook(KindSessionStart, "carol", "c1")
	if !strings.Contains(start.Context, "has no room for this one") {
		t.Errorf("carol's session start was told %q", start.Context)
	}
	if res := h.hook(KindPreEdit, "carol", "c1", "retry/r.go"); res.Decision != DecisionRefuse {
		t.Errorf("carol's edit in alice's reservation: %s", res.Decision)
	}
	res := h.hook(KindPreEdit, "carol", "c1", "shared.go")
	if res.Decision != DecisionAllow || !strings.Contains(res.Context, "bob's agent") {
		t.Errorf("carol's edit of bob's file: %s, %q; want a warning", res.Decision, res.Context)
	}
	if again := h.hook(KindPreEdit, "carol", "c1", "shared.go"); again.Decision != DecisionAllow {
		t.Errorf("carol's next edit of it: %s", again.Decision)
	}
	h.hook(KindPostEdit, "carol", "c1", "shared.go")
	if len(h.b.sessions) != 10 || len(h.b.claims) != claims || len(h.acts) != acts {
		t.Fatalf("the board stored %d sessions, %d claims and %d activities more", len(h.b.sessions)-10, len(h.b.claims)-claims, len(h.acts)-acts)
	}
	if n := h.b.statsFor(repo).Unstored; n != 5 {
		t.Errorf("%d hooks counted unstored, want 5", n)
	}
	// Two of the fleet's sessions end; the next sweep makes room.
	h.at(KindSessionEnd, "fleet", "w0", "f0")
	h.at(KindSessionEnd, "fleet", "w1", "f1")
	h.b.Sweep(h.now)
	h.hook(KindSessionStart, "carol", "c1")
	if h.b.sessions[sessionKey("carol", AgentClaudeCode, "c1")] == nil {
		t.Fatalf("carol's session was not stored after the sweep: %d sessions", len(h.b.sessions))
	}
}

// Past MaxDormantClaims claims with no live session, a sweep forgets those
// quiet longest that hold no intent, at most 200 at a time, in one activity
// per repository.
func TestDormantClaimsPastTheCapAreForgotten(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxDormantClaims = 50 })
	for i := range 300 {
		h.advance(time.Minute)
		m, wt := fmt.Sprintf("m%02d", i%40), fmt.Sprintf("w%03d", i)
		h.scanAt(m, wt, wt, fmt.Sprintf("svc/f%03d.go", i))
		if i%30 == 0 {
			w := whereOf(m)
			w.Worktree = "/work/" + m + "/" + wt
			if _, err := h.b.Declare(h.now, DeclareRequest{Member: m, Where: w, Patterns: []string{fmt.Sprintf("svc/f%03d.go", i)}, Mode: ModeShared}); err != nil {
				t.Fatal(err)
			}
		}
		h.at(KindSessionEnd, m, wt, wt)
	}
	h.advance(2 * time.Hour)
	h.hook(KindHeartbeat, "alice", "a1") // a live claim, which is not dormant
	for sweep, want := range []int{301 - 200, 51} {
		h.acts = nil
		h.b.Sweep(h.now)
		if len(h.b.claims) != want {
			t.Fatalf("after sweep %d: %d claims, want %d", sweep+1, len(h.b.claims), want)
		}
		forgot := h.activities(ActivityClaimForgotten)
		if len(forgot) != 1 || forgot[0].ClaimID != "" || !strings.Contains(forgot[0].Text, "forgotten") {
			t.Fatalf("after sweep %d, announced %+v", sweep+1, forgot)
		}
	}
	// Those left: alice's, the ten that hold an intent, and the 40 newest
	// of the rest.
	for i := range 300 {
		m, wt := fmt.Sprintf("m%02d", i%40), fmt.Sprintf("w%03d", i)
		w := whereOf(m)
		w.Worktree = "/work/" + m + "/" + wt
		if kept := h.b.findClaim(m, w) != nil; kept != (i%30 == 0 || i >= 259) {
			t.Errorf("claim %d kept: %v", i, kept)
		}
	}
}
