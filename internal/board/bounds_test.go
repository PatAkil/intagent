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
// seconds, before it arrived. An age of 0 keeps none, as before; an edit a
// hook reported before the scan began and the scan does not list is gone
// from the worktree, and is dropped.
func TestScanKeepsChangesNewerThanItself(t *testing.T) {
	for _, tc := range []struct {
		age      time.Duration // the scan's age when it arrives
		editedAt time.Duration // before the scan arrives
		kept     bool
	}{
		{0, time.Second, false},
		{5 * time.Second, 3 * time.Second, true},
		{5 * time.Second, 5 * time.Second, false}, // made as the scan began: git saw it
		{2 * time.Second, 3 * time.Second, false},
		{time.Minute, 7 * time.Second, true},
		{time.Minute, 9 * time.Second, false}, // past the 8 s the board reads
		{-time.Second, time.Second, false},
	} {
		h := newHarness(t)
		h.hook(KindPostEdit, "alice", "a1", "svc/a.go")
		h.advance(tc.editedAt)
		_, err := h.b.Hook(h.now, HookEvent{Kind: KindToolEnd, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
			Footprint: &Footprint{Files: refs("svc/b.go"), AgeMS: tc.age.Milliseconds()}})
		if err != nil {
			t.Fatal(err)
		}
		c := h.b.findClaim("alice", whereOf("alice"))
		if kept := c.Footprint["svc/a.go"] != nil; kept != tc.kept || c.Footprint["svc/b.go"] == nil {
			t.Errorf("a scan %s old, an edit %s before it arrived: kept %v, want %v", tc.age, tc.editedAt, kept, tc.kept)
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
