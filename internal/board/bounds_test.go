package board

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// A claim keeps the 20 sessions that ended last, and up to twice as many
// between trims, so a client that starts a session for every event leaves no
// more on it than that: a teammate's edit that asks whether the claim is live
// reads them, not 20,000.
func TestEndedSessionsPerClaimAreBounded(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bot", "w", "s-first", "svc/x.go")
	for i := range 20_000 {
		s := fmt.Sprint("s", i)
		h.at(KindHeartbeat, "bot", "w", s)
		h.at(KindSessionEnd, "bot", "w", s)
	}
	// Besides those: the live one, and the last to end, which the next
	// session to start makes room for.
	c := h.claimIn("bot", "w")
	if n := len(h.b.claimSessions[c.ID]); n > 2*maxEndedSessions+2 {
		t.Fatalf("the claim keeps %d sessions", n)
	}
	w := counted(t, func() { h.hook(KindPreEdit, "alice", "a1", "svc/x.go") })
	if w.sessionVisits > 2*maxEndedSessions+2 {
		t.Fatalf("alice's edit read %d sessions to learn whether bot's claim is live", w.sessionVisits)
	}
}

// A new session that finds the board full is stored all the same: the
// board makes room, down to nine tenths of MaxSessions, from the sessions
// silent longest. A flood of fresh session ids, each heard from once and
// live for two hours, otherwise kept every new session off the board for
// that long: bob's edit was never recorded, and alice was not warned of it.
func TestAFloodOfSessionsLeavesRoomForNewOnes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSessions = 100 })
	for i := range 100 {
		h.at(KindSessionStart, "mallory", "w", fmt.Sprint("flood", i))
		h.advance(time.Second)
	}
	for range 6 {
		h.advance(10 * time.Minute)
		h.b.Sweep(h.now)
	}
	// An hour on, the flood's sessions are still live, waiting.
	h.hook(KindSessionStart, "bob", "b1")
	h.hook(KindPostEdit, "bob", "b1", "svc/x.go")
	if res := h.hook(KindPreEdit, "alice", "a1", "svc/x.go"); len(res.Conflicts) != 1 || res.Conflicts[0].Member != "bob" {
		t.Fatalf("alice's edit of the file bob changed met %+v", res.Conflicts)
	}
	for i, kept := range []bool{false, false, true} {
		k := sessionKey("mallory", AgentClaudeCode, fmt.Sprint("flood", []int{0, 9, 10}[i]))
		if (h.b.sessions[k] != nil) != kept {
			t.Errorf("session %s kept: %v", k, !kept)
		}
	}
	if len(h.b.sessions) != 92 || h.b.statsFor(repo).Evicted != 10 {
		t.Fatalf("the board holds %d sessions, %d counted evicted; want 90 and bob's and alice's, and 10",
			len(h.b.sessions), h.b.statsFor(repo).Evicted)
	}
}

// A board makes room for a new session from those that ended or went gone
// first, then those that stalled, then live ones, each the longest silent
// first, as each new session arrives: which session goes depends on its
// state before when it was last heard from.
func TestAFullBoardMakesRoomInOrder(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSessions = 10 })
	for i := range 4 { // waiting for their people, heard from first
		h.at(KindSessionStart, "w", fmt.Sprint("w", i), fmt.Sprint("w", i))
	}
	h.advance(time.Minute)
	for i := range 4 { // at work, until they stall
		h.at(KindPrompt, "s", fmt.Sprint("s", i), fmt.Sprint("s", i))
		h.advance(time.Second)
	}
	h.advance(time.Minute)
	for i := range 2 { // ended, heard from last
		h.at(KindPrompt, "e", fmt.Sprint("e", i), fmt.Sprint("e", i))
		h.at(KindSessionEnd, "e", fmt.Sprint("e", i), fmt.Sprint("e", i))
	}
	h.advance(h.b.cfg.StallAfter)
	var gone []string
	for i := range 8 { // each of a member of their own, holding fewer than w
		before := maps.Clone(h.b.sessions)
		h.at(KindPrompt, fmt.Sprint("n", i), "w", fmt.Sprint("n", i))
		for k, s := range before {
			if h.b.sessions[k] == nil {
				gone = append(gone, s.ID)
			}
		}
	}
	if want := []string{"e0", "e1", "s0", "s1", "s2", "s3", "w0", "w1"}; !slices.Equal(gone, want) {
		t.Fatalf("new sessions made room from %v, want %v", gone, want)
	}
	if n := h.b.statsFor(repo).Evicted; n != 2 {
		t.Errorf("%d live sessions counted evicted, want 2", n)
	}
}

// Among sessions in the same state, a full board makes room from the member
// holding the most of them first, and only between members holding as many
// from the session silent longest: a member never loses a session while
// another holds more. Here b's sessions are the oldest, but a holds more.
func TestAFullBoardMakesRoomFromWhoeverHoldsMost(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSessions = 10 })
	for _, ms := range []struct {
		member string
		n      int
	}{{"b", 3}, {"a", 5}, {"c", 1}} {
		for i := range ms.n {
			h.advance(time.Second)
			h.at(KindPrompt, ms.member, "w", fmt.Sprint(ms.member, i))
		}
	}
	var gone []string
	for i := range 8 {
		h.advance(time.Second)
		before := maps.Clone(h.b.sessions)
		h.at(KindPrompt, fmt.Sprint("n", i), "w", fmt.Sprint("n", i))
		for k, s := range before {
			if h.b.sessions[k] == nil {
				gone = append(gone, s.ID)
			}
		}
	}
	if want := []string{"a0", "a1", "b0", "a2", "b1", "a3", "b2"}; !slices.Equal(gone, want) {
		t.Fatalf("new sessions made room from %v, want %v", gone, want)
	}
	if n := h.b.statsFor(repo).Evicted; n != 7 {
		t.Errorf("%d live sessions counted evicted, want 7", n)
	}
}

// A flood of one member's fresh session ids into a full board costs that
// member's own sessions, not teammates': alice's reservation keeps blocking
// while she runs a long command, and no honest agent's session is dropped.
// Before, the board let go of the live sessions silent longest, which were
// the honest agents deep in tool calls: in the scale re-attack, 19,500 fresh
// ids against a board of 20,000 dropped alice's and all 999 other honest
// agents' sessions, and bob's retry went through alice's reservation.
func TestAFloodCostsOnlyTheFloodersSessions(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSessions = 200 })
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Retry rework", "svc/pay/**")
	if _, err := h.b.Hook(h.now, HookEvent{Kind: KindToolStart, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1",
		Where: whereOf("alice"), Tool: "Bash", ToolUseID: "long"}); err != nil {
		t.Fatal(err)
	}
	honest := []string{sessionKey("alice", AgentClaudeCode, "a1")}
	for i := range 40 { // ten teammates, four agents each, heard from since
		h.advance(100 * time.Millisecond)
		m, wt := fmt.Sprint("m", i%10), fmt.Sprint("w", i)
		h.at(KindPrompt, m, wt, wt)
		honest = append(honest, sessionKey(m, AgentClaudeCode, wt))
	}
	// Fresh ids a millisecond apart from 50 worktrees, each live, waiting,
	// for IdleAfter: two and a half boards' worth.
	for i := range 500 {
		h.advance(time.Millisecond)
		h.at(KindStop, "ci", fmt.Sprint("ci", i%50), fmt.Sprint("job", i))
	}
	h.advance(time.Second)
	for try := range 2 {
		if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
			t.Fatalf("bob's edit %d inside alice's reservation: %s, want deny", try+1, res.Decision)
		}
	}
	for _, k := range honest {
		if h.b.sessions[k] == nil {
			t.Errorf("session %s was dropped for the flood", k)
		}
	}
	// Every session dropped was the flood's, each live and counted.
	if evicted, created := h.b.statsFor(repo).Evicted, 1+40+500+1; evicted == 0 || evicted != created-len(h.b.sessions) {
		t.Errorf("%d sessions counted evicted; %d created, %d kept", evicted, created, len(h.b.sessions))
	}
}

// Past MaxDormantClaims claims with no live session, a sweep forgets those
// quiet longest that no longer listen and hold no intent, at most 200 at a
// time, in one activity per repository.
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
	h.advance(h.b.cfg.DormantFor + time.Hour)
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

// The dormant cap forgets only claims that no longer listen: those that hold
// an intent, however many, do not make it forget a claim whose agent left a
// minute ago. Once none of them listens, it forgets those that hold no
// intent first, then those that do, each quiet longest first. Before, 60
// claims holding intents, over a cap of 50, had bob's claim forgotten at
// the first sweep after his session ended.
func TestDormantCapSparesClaimsStillListening(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxDormantClaims = 50 })
	for i := range 60 {
		h.advance(time.Minute)
		m, wt := fmt.Sprintf("f%02d", i), fmt.Sprintf("task%02d", i)
		h.at(KindPrompt, m, wt, wt)
		w := whereOf(m)
		w.Worktree = "/work/" + m + "/" + wt
		if _, err := h.b.Declare(h.now, DeclareRequest{Member: m, Where: w, Patterns: []string{fmt.Sprintf("pkg%02d/**", i)}, Mode: ModeShared}); err != nil {
			t.Fatal(err)
		}
		h.at(KindSessionEnd, m, wt, wt)
	}
	h.advance(time.Hour)
	h.b.Sweep(h.now)
	h.at(KindPostEdit, "bob", "w", "b1", "svc/billing.go")
	h.at(KindSessionEnd, "bob", "w", "b1")
	h.advance(time.Minute)
	h.b.Sweep(h.now)
	if len(h.b.claims) != 61 {
		t.Fatalf("%d claims after the sweeps, want all 61", len(h.b.claims))
	}
	if res := h.hook(KindPreEdit, "alice", "a1", "svc/billing.go"); len(res.Conflicts) != 1 {
		t.Fatalf("alice's edit of the file bob changed a minute ago met %+v", res.Conflicts)
	}
	h.advance(h.b.cfg.DormantFor)
	h.b.Sweep(h.now)
	bob := whereOf("bob")
	bob.Worktree = "/work/bob/w"
	if h.b.findClaim("bob", bob) != nil {
		t.Error("bob's claim, which holds no intent, was kept")
	}
	for i := range 60 {
		w := whereOf(fmt.Sprintf("f%02d", i))
		w.Worktree = fmt.Sprintf("/work/f%02d/task%02d", i, i)
		if kept := h.b.findClaim(fmt.Sprintf("f%02d", i), w) != nil; kept != (i >= 10) {
			t.Errorf("claim %d, which holds an intent, kept: %v", i, kept)
		}
	}
}

// A footprint keeps what its byte budgets take, the first files in the
// order the client sent them, and says it was cut: a claim's own budget,
// and what its member's other claims leave of theirs. The answer is the
// same as ever.
func TestFootprintsKeepWithinTheirByteBudgets(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxFootprintBytes, c.MemberFootprintBytes = 10_000, 25_000 })
	files := make([]string, 100)
	for i := range files {
		files[i] = fmt.Sprintf("svc/%s/f%03d.go", strings.Repeat("d", 90), i) // 208 bytes each
	}
	cost := footprintCost(files[0], &touch{Area: "svc/" + strings.Repeat("d", 90)})
	for i, want := range []int{10_000 / cost, 10_000 / cost, (25_000 - 2*(10_000/cost)*cost) / cost} {
		wt := fmt.Sprint("w", i)
		h.scanAt("bot", wt, wt, files...)
		c := h.claimIn("bot", wt)
		if len(c.Footprint) != want || !c.FootprintTruncated {
			t.Fatalf("claim %d kept %d files, truncated %t; want the first %d", i, len(c.Footprint), c.FootprintTruncated, want)
		}
		for _, f := range files[:want] {
			if c.Footprint[f] == nil {
				t.Fatalf("claim %d did not keep %s, one of the first", i, f)
			}
		}
	}
	// An edit the agent makes past the budget is kept: a file git found
	// makes room for it.
	if res := h.at(KindPostEdit, "bot", "w0", "w0", "svc/new.go"); res.Decision != DecisionAllow {
		t.Fatalf("a post_edit past the budget: %s", res.Decision)
	}
	if c := h.claimIn("bot", "w0"); c.Footprint["svc/new.go"] == nil || c.Footprint[files[10_000/cost-1]] != nil || c.fpBytes > 10_000 {
		t.Fatalf("at its budget, an edit is kept in place of the last file git found: %d files, %d bytes", len(c.Footprint), c.fpBytes)
	}
	// Another member's budget is their own.
	h.scanAt("alice", "w", "a", files[:10]...)
	if n := len(h.claimIn("alice", "w").Footprint); n != 10 {
		t.Fatalf("alice's claim kept %d of 10 files", n)
	}
}

// One member's footprints, sent to one fresh worktree after another, hold
// at most the member's budget, however many come, and each at most its
// own: in the scale review, 218 heartbeats of a megabyte each took a 2 GB
// server past 3.5 GB. The budgets here are a sixteenth of the defaults.
func TestOneMembersFootprintsAreBounded(t *testing.T) {
	const claimBytes, memberBytes = defaultFootprintBytes / 16, defaultMemberFootprintBytes / 16
	h := newHarness(t, func(c *Config) { c.MaxFootprintBytes, c.MemberFootprintBytes = claimBytes, memberBytes })
	files := make([]string, 300)
	for i := range files {
		files[i] = fmt.Sprintf("x/%s/%04d", strings.Repeat("y", 140), i)
	}
	fp := &Footprint{Files: refs(files...)}
	for i := range 80 {
		// Each in a repository of its own, so what it costs is the footprint.
		w := Where{Repo: fmt.Sprintf("github.com/acme/r%02d", i), Host: "h", Worktree: "/w"}
		if _, err := h.b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "mallory", Agent: AgentCodex, SessionID: "s", Where: w, Footprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	most, sum := 0, 0
	for _, c := range h.b.claims {
		most, sum = max(most, c.fpBytes), sum+c.fpBytes
	}
	t.Logf("80 footprints of %d bytes: the member holds %d bytes, the largest claim %d", 300*footprintCost(fp.Files[0].Path, &touch{Area: fp.Files[0].Area}), sum, most)
	if sum > memberBytes || most > claimBytes || sum < memberBytes-claimBytes {
		t.Fatalf("the member holds %d bytes, a claim %d", sum, most)
	}
}

// A member's claims that no agent works in give way to their new work: a
// sweep forgets the oldest of them, never one with an agent running, until
// the member's footprints hold three quarters of their budget. Before, a
// fleet's finished worktrees kept their footprints for a week, and once
// they filled the budget nothing the fleet changed was kept: its new claim
// held no file, and a teammate's edit of a file it had just changed was not
// warned. The budgets here are small, so that a few claims fill them.
func TestAMembersOldWorkMakesRoomForTheirNew(t *testing.T) {
	const claimBytes, memberBytes = 16_000, 64_000
	h := newHarness(t, func(c *Config) { c.MaxFootprintBytes, c.MemberFootprintBytes = claimBytes, memberBytes })
	task := func(i int, session string, last Kind) {
		t.Helper()
		w := Where{Repo: fmt.Sprintf("github.com/acme/task%02d", i), Host: "h", Worktree: "/w"}
		paths := make([]PathRef, 40)
		for j := range paths {
			paths[j] = PathRef{Path: fmt.Sprintf("svc/p%02d/handler_%03d.go", i, j), Area: fmt.Sprintf("svc/p%02d", i)}
		}
		for _, kind := range []Kind{KindPostEdit, last} {
			ev := HookEvent{Kind: kind, Member: "fleet", Agent: AgentCodex, SessionID: session, Where: w, Tool: "Edit", Paths: paths}
			if _, err := h.b.Hook(h.now, ev); err != nil {
				t.Fatal(err)
			}
		}
		h.advance(time.Minute)
	}
	task(0, "running", KindStop) // the oldest, its agent waiting for its person
	for i := 1; i <= 15; i++ {
		task(i, fmt.Sprint("done", i), KindSessionEnd)
	}
	if held := h.b.memberBytes["fleet"]; held <= memberBytes {
		t.Fatalf("the fleet holds %d bytes, which leaves room", held)
	}
	h.advance(time.Hour)
	h.b.Sweep(h.now)
	if held := h.b.memberBytes["fleet"]; held > memberBytes*3/4 || held < memberBytes*3/4-claimBytes {
		t.Fatalf("after the sweep the fleet holds %d bytes, want about three quarters of %d", held, memberBytes)
	}
	kept := func(i int) bool {
		return h.b.findClaim("fleet", Where{Repo: fmt.Sprintf("github.com/acme/task%02d", i), Host: "h", Worktree: "/w"}) != nil
	}
	if !kept(0) || kept(1) || !kept(15) {
		t.Fatalf("kept the running claim %v, the oldest finished one %v, the newest %v", kept(0), kept(1), kept(15))
	}
	forgot := h.activities(ActivityClaimForgotten)
	if len(forgot) == 0 || forgot[0].Member != "fleet" || !strings.Contains(forgot[0].Text, "1 of fleet's claims nobody had worked in") {
		t.Fatalf("announced %+v", forgot)
	}
	// A new task in the team's repository: the file the fleet's agent
	// changes is kept, and a teammate is warned before editing it.
	h.hook(KindPostEdit, "fleet", "new", "svc/hot.go")
	if c := h.b.findClaim("fleet", whereOf("fleet")); c.Footprint["svc/hot.go"] == nil {
		t.Fatalf("the fleet's new claim holds %d files", len(c.Footprint))
	}
	if res := h.hook(KindPreEdit, "alice", "a1", "svc/hot.go"); len(res.Conflicts) != 1 {
		t.Fatalf("alice's edit of the file the fleet just changed met %+v", res.Conflicts)
	}
}

// A change a hook reported is never lost to a footprint's bounds: a file
// git found makes room for it, the greatest path first, and once none is
// left the footprint keeps a quarter more; a scan cut short keeps what
// hooks reported that it does not list. So a teammate's edit of the file is
// still bumped, whether the edit came before or after the scan that filled
// the footprint. Before, the edit was dropped once the footprint held 2000
// files, and a scan cut short dropped it too.
func TestHookChangesAreKeptPastTheBounds(t *testing.T) {
	bulk := make([]string, 2000)
	for i := range bulk {
		bulk[i] = fmt.Sprintf("gen/f%04d.go", i)
	}
	scan := func(h *harness, truncated bool) {
		w := whereOf("alice")
		ev := HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: w,
			Footprint: &Footprint{Files: refs(bulk...), Truncated: truncated}}
		if _, err := h.b.Hook(h.now, ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []string{"edit then bulk", "bulk then edit"} {
		h := newHarness(t)
		if order == "edit then bulk" {
			h.hook(KindPostEdit, "alice", "a1", "retry.go")
			h.advance(time.Second)
			scan(h, true) // the client cut 2050 changes to 2000, leaving the edit out
		} else {
			scan(h, false)
			h.advance(time.Second)
			h.hook(KindPostEdit, "alice", "a1", "retry.go")
		}
		c := h.b.findClaim("alice", whereOf("alice"))
		if c.Footprint["retry.go"] == nil || len(c.Footprint) > 2000 {
			t.Fatalf("%s: alice's footprint of %d files lost retry.go", order, len(c.Footprint))
		}
		if res := h.hook(KindPreEdit, "bob", "b1", "retry.go"); res.Decision != DecisionRefuse {
			t.Errorf("%s: bob's edit of retry.go: %s", order, res.Decision)
		}
	}
	// With no file git found left, a footprint keeps a quarter more.
	h := newHarness(t, func(c *Config) { c.MaxFootprint = 12 })
	for i := range 20 {
		h.hook(KindPostEdit, "alice", "a1", fmt.Sprintf("x/f%02d.go", i))
	}
	c := h.b.findClaim("alice", whereOf("alice"))
	if len(c.Footprint) != 15 || !c.FootprintTruncated {
		t.Fatalf("20 edits at a cap of 12: %d kept, truncated %t; want 15", len(c.Footprint), c.FootprintTruncated)
	}
	// A scan that lists every change still drops what it does not list.
	if _, err := h.b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1",
		Where: whereOf("alice"), Footprint: &Footprint{Files: refs("x/f00.go", "y.go")}}); err != nil {
		t.Fatal(err)
	}
	if len(c.Footprint) != 2 || c.Footprint["x/f01.go"] != nil {
		t.Fatalf("a full scan left %d files, x/f00.go %v", len(c.Footprint), c.Footprint["x/f00.go"])
	}
}

// A scan keeps the changes hooks reported after it began, which git may not
// have seen: those reported within its age (Footprint.AgeMS), up to 8
// seconds, before the client sent it, or since, while the request was on
// its way and waited in the server to be read and admitted
// (HookEvent.Waited). An age of 0 from a request that did not wait keeps
// none, as before; an edit a hook reported before the scan began and the
// scan does not list is gone from the worktree, and is dropped.
func TestScanKeepsChangesNewerThanItself(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		age      time.Duration // the scan's age when the client sends it
		waited   time.Duration // in the server, before the board takes it
		editedAt time.Duration // before the board takes the scan
		kept     bool
	}{
		{0, 0, time.Second, false},
		{5 * time.Second, 0, 3 * time.Second, true},
		{5 * time.Second, 0, 5 * time.Second, false}, // made as the scan began: git saw it
		{2 * time.Second, 0, 3 * time.Second, false},
		{time.Minute, 0, 7 * time.Second, true},
		{time.Minute, 0, 9 * time.Second, false}, // past the 8 s the board reads
		{-time.Second, 0, time.Second, false},
		{100 * ms, 300 * ms, 250 * ms, true}, // made 150 ms after the scan began
		{100 * ms, 300 * ms, 400 * ms, false},
		{0, 300 * ms, 200 * ms, true}, // from a client that does not say: made after it sent the scan
		{time.Minute, 2 * time.Second, 9 * time.Second, true},
		{time.Minute, 2 * time.Second, 11 * time.Second, false},
	} {
		h := newHarness(t)
		h.hook(KindPostEdit, "alice", "a1", "svc/a.go")
		h.advance(tc.editedAt)
		_, err := h.b.Hook(h.now, HookEvent{Kind: KindToolEnd, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
			Footprint: &Footprint{Files: refs("svc/b.go"), AgeMS: tc.age.Milliseconds()}, Waited: tc.waited})
		if err != nil {
			t.Fatal(err)
		}
		c := h.b.findClaim("alice", whereOf("alice"))
		if kept := c.Footprint["svc/a.go"] != nil; kept != tc.kept || c.Footprint["svc/b.go"] == nil {
			t.Errorf("a scan %s old, which waited %s, an edit %s before it was taken: kept %v, want %v",
				tc.age, tc.waited, tc.editedAt, kept, tc.kept)
		}
	}
}

// A directory a worktree added whole, which its footprint names in place
// of the files under it it leaves out, counts as nearby work for a
// teammate's edit of a file under it, warned of once per directory, until a
// scan no longer names it.
func TestDirectoriesAddedWholeAreNearbyWork(t *testing.T) {
	h := newHarness(t)
	if _, err := h.b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
		Footprint: &Footprint{Files: refs("web/app.ts"), Truncated: true,
			Dirs: []PathRef{{Path: ".venv"}, {Path: "web/new", Area: "web/new"}}}}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	for _, tc := range []struct {
		path, why string
	}{
		{"web/new/src/app.ts", "added the directory web/new whole"},
		{".venv/lib/site.py", "added the directory .venv whole"},
		{"lib/new/x.go", ""},
		{".venv2/x.py", ""},
	} {
		res := h.hook(KindPreEdit, "bob", "b1", tc.path)
		if tc.why == "" {
			if len(res.Conflicts) != 0 || res.Context != "" {
				t.Errorf("%s: %+v", tc.path, res)
			}
			continue
		}
		if res.Decision != DecisionAllow || len(res.Conflicts) != 1 || res.Conflicts[0].Severity != SeverityNearby ||
			!strings.Contains(res.Context, tc.why) {
			t.Errorf("%s: %s, %q, %+v; want a nearby warning that alice %s", tc.path, res.Decision, res.Context, res.Conflicts, tc.why)
		}
	}
	if res := h.hook(KindPreEdit, "bob", "b1", ".venv/lib/other.py"); res.Context != "" {
		t.Errorf("warned again of .venv: %q", res.Context)
	}
	// A scan that names no directory leaves none.
	if _, err := h.b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
		Footprint: &Footprint{Files: refs("web/app.ts")}}); err != nil {
		t.Fatal(err)
	}
	if res := h.hook(KindPreEdit, "carol", "c1", "web/new/src/app.ts"); len(res.Conflicts) != 0 {
		t.Errorf("after a scan with no directories: %+v", res.Conflicts)
	}
}

// The board keeps stats for a bounded number of repositories: past it, a
// new one takes the place of the repository with no claims counted in
// longest ago. A client can name any repository, and each kept its stats
// for good, in every snapshot.
func TestStatsAreKeptForBoundedRepositories(t *testing.T) {
	h := newHarness(t)
	h.edit("alice", "a1", "x.go") // a repository with a claim, counted in first
	for i := range 2000 {
		h.advance(time.Second)
		w := Where{Repo: fmt.Sprintf("github.com/acme/r%04d", i), Host: "h", Worktree: "/w"}
		ev := HookEvent{Kind: KindPreEdit, Member: "mallory", Agent: AgentCodex, SessionID: "s", Where: w, Tool: "Edit", Paths: refs("a.go")}
		if _, err := h.b.Hook(h.now, ev); err != nil {
			t.Fatal(err)
		}
		ev.Kind = KindSessionEnd
		if _, err := h.b.Hook(h.now, ev); err != nil {
			t.Fatal(err)
		}
	}
	_, kept := h.b.stats[repo]
	_, oldest := h.b.stats["github.com/acme/r0000"]
	_, newest := h.b.stats["github.com/acme/r1999"]
	if len(h.b.stats) != maxStatsRepos || !kept || oldest || !newest {
		t.Fatalf("stats for %d repositories; the one with a claim %v, the first %v, the last %v", len(h.b.stats), kept, oldest, newest)
	}
}

// Past the bound, the stats that go are those of the repository counted in
// longest ago, not the one first counted: a repository the team still
// works in keeps its stats, though it was the first.
func TestStatsGoForTheRepositoryCountedInLongestAgo(t *testing.T) {
	h := newHarness(t)
	count := func(r string) {
		t.Helper()
		h.advance(time.Second)
		w := Where{Repo: r, Host: "h", Worktree: "/w"}
		for _, kind := range []Kind{KindPreEdit, KindSessionEnd} {
			if _, err := h.b.Hook(h.now, HookEvent{Kind: kind, Member: "m", Agent: AgentCodex, SessionID: "s", Where: w, Tool: "Edit", Paths: refs("a.go")}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range maxStatsRepos {
		count(fmt.Sprintf("github.com/acme/r%04d", i))
	}
	count("github.com/acme/r0000")
	count("github.com/acme/new")
	_, first := h.b.stats["github.com/acme/r0000"]
	_, second := h.b.stats["github.com/acme/r0001"]
	if !first || second {
		t.Fatalf("kept the first repository's stats %v, the second's %v; want the first's, counted in since", first, second)
	}
}

// A flood of live sessions into one worktree, a client that sends each hook
// with a fresh session id, costs each new session a few looks at the claim's
// sessions, not a look at all of them: none of them can be dropped, and
// looking at every one for each made the flood quadratic (20,000 sessions:
// 865 µs a hook at the end, against 6 µs).
func TestALiveFloodIntoOneClaimCostsLittleEach(t *testing.T) {
	h := newHarness(t)
	h.b.notify = nil
	const flood = 5000
	w := counted(t, func() {
		for i := range flood {
			h.at(KindHeartbeat, "bot", "w", fmt.Sprint("s", i))
		}
	})
	if w.sessionVisits > 4*flood {
		t.Fatalf("%d sessions joining one claim read its sessions %d times", flood, w.sessionVisits)
	}
}

// Sessions that went silent for good are dropped by the next sweep, past the
// newest maxEndedSessions of their claim: a flood's sessions that go gone
// together, with none joining after them, otherwise stay an hour, read by
// every teammate's edit that asks whether the claim is live.
func TestSweepBoundsTheSessionsAClaimKeeps(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bot", "w", "s-first", "svc/x.go")
	for i := range 500 {
		h.at(KindHeartbeat, "bot", "w", fmt.Sprint("s", i))
	}
	h.advance(h.b.cfg.IdleAfter + time.Minute)
	h.b.Sweep(h.now)
	c := h.claimIn("bot", "w")
	if n := len(h.b.claimSessions[c.ID]); n != maxEndedSessions {
		t.Fatalf("the claim keeps %d sessions gone, want %d", n, maxEndedSessions)
	}
	w := counted(t, func() { h.hook(KindPreEdit, "alice", "a1", "svc/x.go") })
	if w.sessionVisits > maxEndedSessions+1 {
		t.Fatalf("alice's edit read %d sessions to learn whether bot's claim is live", w.sessionVisits)
	}
	// Sessions that stalled are kept, up to maxStalledSessions: the
	// dashboard shows them, and one that comes back is announced as
	// recovered.
	for i := range 30 {
		h.at(KindPrompt, "fleet", "w", fmt.Sprint("f", i))
	}
	h.advance(h.b.cfg.StallAfter + time.Minute)
	h.b.Sweep(h.now)
	if n := len(h.b.claimSessions[h.claimIn("fleet", "w").ID]); n != 30 {
		t.Fatalf("the fleet's claim keeps %d of its 30 stalled sessions", n)
	}
}

// A flood of sessions into one worktree that stall together costs a
// teammate's edit one read of the claim's sessions, not one for each path
// the edit names; and the next sweep keeps the maxStalledSessions heard from
// last. Before, each of 17,000 stalled sessions stayed on the claim for two
// hours, and a 200-file edit read 3.4 million sessions under the board's
// lock, about 170 ms.
func TestAStalledFloodCostsATeammateLittle(t *testing.T) {
	h := newHarness(t)
	h.b.notify = nil
	files := make([]string, 200)
	for i := range files {
		files[i] = fmt.Sprintf("svc/f%03d.go", i)
	}
	h.scanAt("bot", "w", "s-first", files...)
	h.at(KindSessionEnd, "bot", "w", "s-first")
	const flood = 2000
	for i := range flood {
		h.at(KindPrompt, "bot", "w", fmt.Sprint("s", i))
		h.advance(time.Millisecond)
	}
	h.advance(h.b.cfg.StallAfter + time.Minute)
	c := h.claimIn("bot", "w")
	w := counted(t, func() { h.hook(KindPreEdit, "alice", "a1", files...) })
	if w.sessionVisits > flood+1+len(files) {
		t.Fatalf("alice's %d-file edit read %d sessions of a claim that holds %d", len(files), w.sessionVisits, flood+1)
	}
	h.b.Sweep(h.now)
	if n := len(h.b.claimSessions[c.ID]); n != maxStalledSessions+1 {
		t.Fatalf("after a sweep, bot's claim keeps %d sessions; want %d stalled and the one that ended", n, maxStalledSessions)
	}
	for i := flood - maxStalledSessions; i < flood; i++ {
		if h.b.sessions[sessionKey("bot", AgentClaudeCode, fmt.Sprint("s", i))] == nil {
			t.Fatalf("session s%d, one of the last to stall, was dropped", i)
		}
	}
	w = counted(t, func() { h.hook(KindPreEdit, "carol", "c1", files...) })
	if w.sessionVisits > maxStalledSessions+maxEndedSessions+len(files) {
		t.Fatalf("carol's edit read %d sessions", w.sessionVisits)
	}
}

// The directories a footprint names in place of files count against its
// byte budgets as files do, after the files: a footprint of one file and 200
// directories of a kilobyte, sent to fresh worktrees, otherwise held 400 KB a
// claim outside every budget.
func TestDirectoriesCountAgainstTheByteBudgets(t *testing.T) {
	const claimBytes, memberBytes = 20_000, 100_000
	h := newHarness(t, func(c *Config) { c.MaxFootprintBytes, c.MemberFootprintBytes = claimBytes, memberBytes })
	dirs := make([]PathRef, maxFootprintDirs)
	for i := range dirs {
		d := fmt.Sprintf("gen/%s%03d", strings.Repeat(strings.Repeat("d", 240)+"/", 4), i)
		dirs[i] = PathRef{Path: d, Area: d}
	}
	for i := range 30 {
		w := Where{Repo: fmt.Sprintf("github.com/acme/r%02d", i), Host: "h", Worktree: "/w"}
		ev := HookEvent{Kind: KindHeartbeat, Member: "mallory", Agent: AgentCodex, SessionID: "s", Where: w,
			Footprint: &Footprint{Files: refs("a.go"), Dirs: dirs, Truncated: true}}
		if _, err := h.b.Hook(h.now, ev); err != nil {
			t.Fatal(err)
		}
	}
	held, most := 0, 0
	for _, c := range h.b.claims {
		n := len(c.Footprint) * footprintCost("a.go", &touch{})
		for d, t := range c.Dirs {
			n += footprintCost(d, t)
		}
		held, most = held+n, max(most, n)
		if len(c.Dirs) > 0 && (c.Footprint["a.go"] == nil || !c.FootprintTruncated) {
			t.Fatalf("claim %s kept directories but not its file, or did not say it was cut", c.ID)
		}
	}
	if held > memberBytes || most > claimBytes {
		t.Fatalf("the member's claims hold %d bytes, the largest %d; want at most %d and %d", held, most, memberBytes, claimBytes)
	}
	mustIndex(t, h.b, "the footprints")
}

// When the changes hooks reported are more than a footprint's bounds take,
// as in a footprint restored under a lower max_footprint, a scan cut short
// keeps the newest of them: those an agent made last.
func TestAScanKeepsTheNewestHookChanges(t *testing.T) {
	h := newHarness(t)
	for i := range 10 {
		h.advance(time.Second)
		h.hook(KindPostEdit, "alice", "a1", fmt.Sprintf("x/f%02d.go", i))
	}
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	b := New(Config{MaxFootprint: 4})
	if err := b.Restore(bytes.NewReader(data), h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
		Footprint: &Footprint{Files: refs("y.go"), Truncated: true}}); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(b.findClaim("alice", whereOf("alice")).Footprint))
	if want := []string{"x/f05.go", "x/f06.go", "x/f07.go", "x/f08.go", "x/f09.go"}; !slices.Equal(got, want) {
		t.Fatalf("the footprint keeps %v, want the five changes made last, %v", got, want)
	}
}

// A directory a worktree added whole keeps when the board first heard of
// it, scan after scan, as a change to a file does. A teammate's edits under
// two such directories, of files the client found no area for, are each
// warned of, once per directory.
func TestDirectoriesAddedWholeKeepWhenTheyWereFirstHeardOf(t *testing.T) {
	h := newHarness(t)
	scan := func() {
		t.Helper()
		if _, err := h.b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
			Footprint: &Footprint{Files: refs("web/app.ts"), Truncated: true, Dirs: []PathRef{{Path: ".venv"}, {Path: "gen"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	scan()
	first := h.now
	h.advance(time.Hour)
	scan()
	if at := h.b.findClaim("alice", whereOf("alice")).Dirs[".venv"].At; !at.Equal(first) {
		t.Fatalf("the directory was first heard of at %s, and now says %s", first, at)
	}
	edit := func(path string) HookResult {
		t.Helper()
		res, err := h.b.Hook(h.now, HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentClaudeCode, SessionID: "b1", Where: whereOf("bob"),
			Tool: "Edit", Paths: []PathRef{{Path: path}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := edit(".venv/lib/a.py"); len(res.Conflicts) != 1 || !res.Conflicts[0].Since.Equal(first) || !strings.Contains(res.Context, ".venv") {
		t.Fatalf("bob's edit under .venv: %q, %+v", res.Context, res.Conflicts)
	}
	if res := edit("gen/x.go"); !strings.Contains(res.Context, "added the directory gen whole") {
		t.Fatalf("bob's edit under gen was told %q", res.Context)
	}
	if res := edit(".venv/bin/b.py"); res.Context != "" {
		t.Fatalf("bob was warned of .venv again: %q", res.Context)
	}
}

// A session that joins a claim holding as many that ended as it keeps
// stays on the board, though it was heard from before all of them: it is
// the one reporting, an agent that resumes its session in another worktree.
func TestASessionJoiningAClaimFullOfEndedOnesStays(t *testing.T) {
	h := newHarness(t)
	h.at(KindPrompt, "bob", "old", "b-old")
	h.at(KindSessionEnd, "bob", "old", "b-old")
	h.advance(time.Minute)
	h.at(KindPostEdit, "bob", "w", "e0", "x.go")
	for i := range 2 * maxEndedSessions {
		h.at(KindHeartbeat, "bob", "w", fmt.Sprint("e", i))
		h.at(KindSessionEnd, "bob", "w", fmt.Sprint("e", i))
	}
	h.at(KindSessionStart, "bob", "w", "b-old")
	if h.b.sessions[sessionKey("bob", AgentClaudeCode, "b-old")] == nil {
		t.Fatal("the session that joined was dropped to make room")
	}
	if n := len(h.b.claimSessions[h.claimIn("bob", "w").ID]); n != maxEndedSessions+1 {
		t.Fatalf("the claim keeps %d sessions, want %d that ended and the one that joined", n, maxEndedSessions)
	}
}

// A hook that reports changes to files a full footprint holds, from git,
// and to new ones keeps all of them, and makes room for the new ones from
// the other files git found: before, the room could be made by letting go
// of one of the hook's own files, which it then added back as new, past the
// footprint's bound, and told teammates of again.
func TestAHooksOwnFilesAreNotLetGoToMakeRoom(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxFootprint = 12 })
	var files []string
	for i := range 12 {
		files = append(files, fmt.Sprintf("a%02d.go", i))
	}
	h.scanAt("alice", "w", "a1", files...)
	h.at(KindPostEdit, "alice", "w", "a1", "a11.go", "b.go")
	c := h.claimIn("alice", "w")
	t11, kept := c.Footprint["a11.go"], c.Footprint["b.go"] != nil
	if len(c.Footprint) != 12 || t11 == nil || t11.FromGit || !kept || c.Footprint["a10.go"] != nil {
		t.Fatalf("the footprint holds %d files; a11.go %+v, b.go %v, a10.go %v; want 12, both the hook's, a10.go let go",
			len(c.Footprint), t11, kept, c.Footprint["a10.go"] != nil)
	}
}

// A board at MaxSessions makes room from the sessions that ended or went
// gone, the longest silent first, before it lets go of one that stalled,
// which the dashboard shows and whose agent may yet come back.
func TestAFullBoardMakesRoomFromEndedSessionsFirst(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSessions = 10 })
	for i := range 4 { // agents that stall, heard from first
		h.at(KindPrompt, "fleet", fmt.Sprint("s", i), fmt.Sprint("s", i))
	}
	h.advance(time.Minute)
	for i := range 6 { // sessions that end, heard from later
		h.at(KindPrompt, "bot", fmt.Sprint("e", i), fmt.Sprint("e", i))
		h.at(KindSessionEnd, "bot", fmt.Sprint("e", i), fmt.Sprint("e", i))
	}
	h.advance(h.b.cfg.StallAfter + time.Minute)
	h.b.Sweep(h.now)
	stalled := 0
	for _, s := range h.b.sessions {
		if h.b.state(h.now, s) == StateStalled {
			stalled++
		}
	}
	if len(h.b.sessions) != 9 || stalled != 4 {
		t.Fatalf("the board keeps %d sessions, %d of the 4 stalled; want 9, all 4", len(h.b.sessions), stalled)
	}
}
