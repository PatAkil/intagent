package board

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// workerHook sends an event from one worker (a subagent) of bob's session
// s1; worker "" is the session's main agent.
func (h *harness) workerHook(kind Kind, worker string, paths ...string) HookResult {
	h.t.Helper()
	ev := HookEvent{Kind: kind, Member: "bob", Agent: AgentClaudeCode, SessionID: "s1", Where: whereOf("bob"), Paths: refs(paths...),
		Worker: worker}
	if kind == KindPreEdit || kind == KindPostEdit {
		ev.Tool = "Edit"
	}
	res, err := h.b.Hook(h.now, ev)
	if err != nil {
		h.t.Fatalf("Hook(%s, worker %q): %v", kind, worker, err)
	}
	return res
}

// Parallel workers of one session each read their own context, so a bump
// one of them heard is not another's retry: each is refused once and told
// why, and its own retry goes through. Before, the first was bumped and the
// other three went through with nothing said. The collision is still counted
// and announced once.
func TestParallelWorkersEachHearTheirBump(t *testing.T) {
	h := newHarness(t)
	h.hook(KindSessionStart, "alice", "a1")
	h.edit("alice", "a1", "svc/pay/retry.go")
	h.workerHook(KindSessionStart, "")
	workers := []string{"agent-1", "agent-2", "agent-3", "agent-4"}
	for _, w := range workers {
		res := h.workerHook(KindPreEdit, w, "svc/pay/retry.go")
		if res.Decision != DecisionRefuse || !strings.Contains(res.Reason, "alice") {
			t.Fatalf("worker %s's first edit: %s %q, want a refusal that names alice", w, res.Decision, res.Reason)
		}
		h.advance(time.Second)
	}
	for _, w := range workers {
		if res := h.workerHook(KindPreEdit, w, "svc/pay/retry.go"); res.Decision != DecisionAllow {
			t.Fatalf("worker %s's retry: %s, want allow", w, res.Decision)
		}
	}
	// The session's main agent was not told either.
	if res := h.workerHook(KindPreEdit, "", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Fatalf("the main agent's first edit: %s, want a refusal", res.Decision)
	}
	if st := h.b.View(h.now, repo).Stats; st.Bumped != 1 {
		t.Errorf("stats count %d bumped, want the collision once", st.Bumped)
	}
	if n := len(h.activities(ActivityConflict)); n != 1 {
		t.Errorf("%d conflict activities, want 1", n)
	}
}

// Warnings and the question an agent that cannot ask is told to put are told
// to each worker once, as bumps are.
func TestParallelWorkersEachHearWarningsAndQuestions(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy.Overlap = ActionAsk })
	h.hook(KindSessionStart, "alice", "a1")
	h.edit("alice", "a1", "svc/pay/retry.go")
	ask := func(worker string) Decision {
		ev := HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "s1", Where: whereOf("bob"), Tool: "apply_patch",
			Paths: refs("svc/pay/retry.go"), NoAsk: true, Worker: worker}
		res, err := h.b.Hook(h.now, ev)
		if err != nil {
			t.Fatal(err)
		}
		return res.Decision
	}
	for _, w := range []string{"agent-1", "agent-2"} {
		if d := ask(w); d != DecisionAsk {
			t.Fatalf("worker %s's first edit: %s, want ask", w, d)
		}
	}
	for _, w := range []string{"agent-1", "agent-2"} {
		if d := ask(w); d != DecisionAllow {
			t.Fatalf("worker %s's retry: %s, want allow", w, d)
		}
	}

	// Nearby work: each worker is warned once.
	h.edit("alice", "a1", "svc/ledger/book.go")
	for _, w := range []string{"agent-1", "agent-2"} {
		res := h.workerHook(KindPreEdit, w, "svc/ledger/post.go")
		if res.Decision != DecisionAllow || !strings.Contains(res.Context, "svc/ledger") {
			t.Fatalf("worker %s's first edit nearby: %s %q, want a warning", w, res.Decision, res.Context)
		}
		if res := h.workerHook(KindPreEdit, w, "svc/ledger/post.go"); res.Context != "" {
			t.Fatalf("worker %s was warned twice: %q", w, res.Context)
		}
	}
}

// A worker that finished takes what it was told with it; the session's other
// workers, and its main agent, keep theirs. A worker run again is told again,
// and the collision is not counted or announced again.
func TestWorkerEndForgetsWhatTheWorkerWasTold(t *testing.T) {
	h := newHarness(t)
	h.hook(KindSessionStart, "alice", "a1")
	h.edit("alice", "a1", "svc/pay/retry.go")
	for _, w := range []string{"", "agent-1", "agent-2"} {
		h.workerHook(KindPreEdit, w, "svc/pay/retry.go")
	}
	s := h.b.sessions[sessionKey("bob", AgentClaudeCode, "s1")]
	before := len(s.Acked)
	h.workerHook(KindWorkerEnd, "agent-1")
	if len(s.Acked) != before-1 {
		t.Fatalf("the session remembers %d things after agent-1 ended, %d before; want one fewer", len(s.Acked), before)
	}
	for k := range s.Acked {
		if strings.HasSuffix(k, workerSep+"agent-1") {
			t.Errorf("kept %q", k)
		}
	}
	if res := h.workerHook(KindPreEdit, "agent-1", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Errorf("agent-1 run again: %s, want a refusal", res.Decision)
	}
	for _, w := range []string{"", "agent-2"} {
		if res := h.workerHook(KindPreEdit, w, "svc/pay/retry.go"); res.Decision != DecisionAllow {
			t.Errorf("worker %q after agent-1 ended: %s, want allow", w, res.Decision)
		}
	}
	if n := len(h.activities(ActivityConflict)); n != 1 {
		t.Errorf("%d conflict activities, want 1", n)
	}
	// An end without a worker, or of one never told anything, forgets nothing.
	before = len(s.Acked)
	h.workerHook(KindWorkerEnd, "")
	h.workerHook(KindWorkerEnd, "agent-9")
	if len(s.Acked) != before {
		t.Errorf("forgot %d things for no worker", before-len(s.Acked))
	}
}

// An edit a worker made without hearing its check is judged as that worker
// would have heard it: the session's main agent having been told is not the
// worker having been told.
func TestAnUnansweredEditIsJudgedForItsWorker(t *testing.T) {
	h := newHarness(t)
	h.hook(KindSessionStart, "alice", "a1")
	h.edit("alice", "a1", "svc/pay/retry.go")
	h.workerHook(KindPreEdit, "", "svc/pay/retry.go") // the main agent is bumped
	ev := HookEvent{Kind: KindPostEdit, Member: "bob", Agent: AgentClaudeCode, SessionID: "s1", Where: whereOf("bob"), Tool: "Edit",
		ToolUseID: "never-seen", Paths: refs("svc/pay/retry.go"), Worker: "agent-1"}
	res, err := h.b.Hook(h.now, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CheckedAfter || !strings.Contains(res.Context, "was not checked before it ran") {
		t.Fatalf("agent-1's unanswered edit: checked after %t, context %q", res.CheckedAfter, res.Context)
	}
}

// A worker's prompt starts its own turn, not the session's: the tools the
// session's other workers are running stay in flight, and the session keeps
// the longer stall threshold they give it.
func TestAWorkersPromptKeepsTheSessionsToolsRunning(t *testing.T) {
	h := newHarness(t)
	ev := HookEvent{Kind: KindToolStart, Member: "bob", Agent: AgentClaudeCode, SessionID: "s1", Where: whereOf("bob"), Tool: "Bash", ToolUseID: "t1"}
	if _, err := h.b.Hook(h.now, ev); err != nil {
		t.Fatal(err)
	}
	since := h.now
	h.advance(time.Minute)
	h.workerHook(KindPrompt, "agent-1")
	s := h.b.sessions[sessionKey("bob", AgentClaudeCode, "s1")]
	if !s.Calls["t1"] || s.Tool != "Bash" || !s.ToolSince.Equal(since) {
		t.Fatalf("after a worker's prompt the session's calls are %v, tool %q since %s", s.Calls, s.Tool, s.ToolSince)
	}
	// The main agent's prompt is a turn boundary, as before.
	h.workerHook(KindPrompt, "")
	if s := h.b.sessions[sessionKey("bob", AgentClaudeCode, "s1")]; len(s.Calls) != 0 || s.Tool != "" {
		t.Fatalf("the main agent's prompt left calls %v, tool %q", s.Calls, s.Tool)
	}
}

// A worker's ID is cleaned like a tool call's, and bounded.
func TestWorkerIsCleaned(t *testing.T) {
	ev, err := HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentClaudeCode, SessionID: "s1", Where: whereOf("bob"),
		Worker: "agent\n1" + strings.Repeat("x", 300)}.clean(DefaultConfig().MaxFootprint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(ev.Worker, '\n') || utf8.RuneCountInString(ev.Worker) > 201 {
		t.Fatalf("worker %q", ev.Worker)
	}
}

// worktree is one of alice's worktrees other than her own.
func worktree(name string) Where {
	w := whereOf("alice")
	w.Worktree, w.Branch = "/work/alice-"+name, "feat/alice-"+name
	return w
}

// aliceFrom sends an event of alice's session s1 from where.
func (h *harness) aliceFrom(kind Kind, where Where, paths ...string) HookResult {
	h.t.Helper()
	return h.aliceWorker(kind, where, "", paths...)
}

// aliceWorker sends an event of a worker of alice's session s1 from where.
func (h *harness) aliceWorker(kind Kind, where Where, worker string, paths ...string) HookResult {
	h.t.Helper()
	ev := HookEvent{Kind: kind, Member: "alice", Agent: AgentClaudeCode, SessionID: "s1", Where: where, Paths: refs(paths...),
		Worker: worker}
	if kind == KindPreEdit || kind == KindPostEdit {
		ev.Tool = "Edit"
	}
	res, err := h.b.Hook(h.now, ev)
	if err != nil {
		h.t.Fatalf("Hook(%s from %s): %v", kind, where.Worktree, err)
	}
	return res
}

// An agent that moves to another worktree under the same session (Claude
// Code's EnterWorktree) keeps the claim it left live while it is: the
// reservation it made there still refuses teammates every time, and a
// teammate cannot reserve the same files. Before, one event from the new
// worktree left the old claim with no session: teammates were bumped once
// and their retry went through, and their reservation was accepted.
func TestAWorktreeMoveKeepsItsReservation(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
	h.advance(time.Minute)
	h.aliceFrom(KindPrompt, worktree("b"))
	h.hook(KindSessionStart, "bob", "b1")
	for range 2 {
		if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); res.Decision != DecisionRefuse ||
			len(res.Conflicts) == 0 || res.Conflicts[0].Severity != SeverityBlock {
			t.Fatalf("bob's edit inside alice's reservation: %s %v, want a refusal for the reservation", res.Decision, res.Conflicts)
		}
	}
	if res, err := h.b.Declare(h.now, DeclareRequest{Member: "carol", Where: whereOf("carol"), Patterns: []string{"svc/pay/**"},
		Mode: ModeExclusive}); err != nil || len(res.Rejected) != 1 {
		t.Fatalf("carol's clashing reservation: %+v %v, want it rejected", res, err)
	}
	v := h.b.View(h.now, repo)
	listed := 0
	for _, c := range v.Claims {
		if c.Member != "alice" {
			continue
		}
		if !c.Active || len(c.Sessions) != 1 || c.Sessions[0].ID != "s1" {
			t.Errorf("alice's claim in %s: active %t, sessions %v", c.Worktree, c.Active, c.Sessions)
		}
		listed++
	}
	if listed != 2 || v.Sessions != 2 {
		t.Errorf("view lists %d claims of alice's and counts %d live sessions, want 2 and 2 (alice's and bob's)", listed, v.Sessions)
	}
	if r := h.b.Repos(h.now)[0]; r.ActiveClaims != 3 || r.LiveSessions != 2 {
		t.Errorf("repos: %+v, want 3 active claims and 2 live sessions", r)
	}
	if got := h.b.agentsOf(h.now, h.b.findClaim("alice", whereOf("alice"))); got != "claude-code working" {
		t.Errorf("the greeting says alice's first worktree has %q", got)
	}
	// Back in her first worktree, alice's agent edits inside her own reservation.
	if res := h.aliceFrom(KindPreEdit, whereOf("alice"), "svc/pay/retry.go"); res.Decision != DecisionAllow {
		t.Fatalf("alice's own edit: %s %q", res.Decision, res.Reason)
	}
	// Once the session ends, neither claim is live.
	h.aliceFrom(KindSessionEnd, whereOf("alice"))
	if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); res.Decision == DecisionRefuse && res.Conflicts[0].Severity == SeverityBlock {
		t.Fatalf("bob is refused for a reservation whose session ended")
	}
}

// The claims a session keeps live after it moved are its own work, not a
// teammate's: from the worktree it moved to, its edits inside the
// reservation it made in the first are let through without a word, git
// finding its changes there is no breach, intagent guard does not refuse
// its commit, and it can declare the same reservation where it now works.
// Teammates are still refused every time. Before, the session was refused
// every time by its own reservation, its guard refused its commits, its
// declaration was rejected and its changes were recorded as breaches.
func TestAMovedSessionIsNotHeldBackByItsOwnReservation(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
	h.advance(time.Minute)
	h.aliceFrom(KindPrompt, worktree("b"))
	for range 2 {
		if res := h.aliceFrom(KindPreEdit, worktree("b"), "svc/pay/retry.go"); res.Decision != DecisionAllow || len(res.Conflicts) != 0 {
			t.Fatalf("alice's edit in b inside her own reservation: %s %v", res.Decision, res.Conflicts)
		}
	}
	res, err := h.b.Check(h.now, CheckRequest{Member: "alice", Where: worktree("b"), Paths: refs("svc/pay/retry.go")})
	if err != nil || len(res.Conflicts) != 0 {
		t.Fatalf("intagent guard's check in b: %v %v, want no conflict", res.Conflicts, err)
	}
	scan := HookEvent{Kind: KindToolEnd, Member: "alice", Agent: AgentClaudeCode, SessionID: "s1", Where: worktree("b"),
		Footprint: &Footprint{Files: refs("svc/pay/retry.go", "svc/pay/fees.go")}}
	if got, err := h.b.Hook(h.now, scan); err != nil || got.Context != "" {
		t.Fatalf("git finds alice's changes in b: %q %v, want nothing said", got.Context, err)
	}
	if n := len(h.activities(ActivityConflict)); n != 0 {
		t.Fatalf("%d conflicts recorded, want none: %+v", n, h.activities(ActivityConflict))
	}
	moved, err := h.b.Declare(h.now, DeclareRequest{Member: "alice", Where: worktree("b"), Patterns: []string{"svc/pay/**"},
		Mode: ModeExclusive, Summary: "rework payments"})
	if err != nil || len(moved.Accepted) != 1 || len(moved.Overlaps) != 0 {
		t.Fatalf("alice declares her reservation in b: %+v %v, want it accepted, overlapping nothing", moved, err)
	}

	h.hook(KindSessionStart, "bob", "b1")
	for range 2 {
		if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
			t.Fatalf("bob's edit inside alice's reservations: %s, want a refusal", res.Decision)
		}
	}
	if res, err := h.b.Check(h.now, CheckRequest{Member: "bob", Where: whereOf("bob"), Paths: refs("svc/pay/retry.go")}); err != nil ||
		len(res.Conflicts) != 2 || res.Conflicts[0].Severity != SeverityBlock || res.Conflicts[1].Severity != SeverityBlock {
		t.Fatalf("bob's check: %v %v, want both of alice's reservations blocking", res.Conflicts, err)
	}
}

// A worker in a worktree of its own (a Claude Code subagent with worktree
// isolation) is not held back by the reservation its session made before it
// ran, and neither is the session's main agent back where it was, by what
// the worker changed.
func TestAWorkerIsNotHeldBackByItsSessionsReservation(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
	h.advance(time.Minute)
	for range 2 {
		if res := h.aliceWorker(KindPreEdit, worktree("w1"), "agent-1", "svc/pay/retry.go"); res.Decision != DecisionAllow ||
			len(res.Conflicts) != 0 {
			t.Fatalf("the worker's edit inside its session's reservation: %s %v", res.Decision, res.Conflicts)
		}
	}
	h.aliceWorker(KindPostEdit, worktree("w1"), "agent-1", "svc/pay/retry.go")
	h.advance(time.Second)
	if res := h.aliceFrom(KindPrompt, whereOf("alice")); res.Context != "" {
		t.Fatalf("the main agent is told of its worker's change: %q", res.Context)
	}
	if res := h.aliceFrom(KindPreEdit, whereOf("alice"), "svc/pay/retry.go"); res.Decision != DecisionAllow || len(res.Conflicts) != 0 {
		t.Fatalf("the main agent's edit of the file its worker changed: %s %v", res.Decision, res.Conflicts)
	}
}

// A claim kept live by another agent, at work in it or moved from it to
// somewhere else, before or after this session moved, still holds back a
// session that moved from it: the reservation may be that agent's. (Each
// case is played several times, as which session the board finds first
// follows the order of a map.)
func TestAReservationAnotherAgentKeepsStillHoldsBack(t *testing.T) {
	cases := []struct {
		name   string
		to     Where
		before bool
	}{{"at work in it", whereOf("alice"), true}, {"moved before", worktree("c"), true},
		{"moved after", worktree("c"), false}}
	for i := range 4 * len(cases) {
		tc := cases[i%len(cases)]
		h := newHarness(t)
		h.aliceFrom(KindSessionStart, whereOf("alice"))
		h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
		other := func() {
			ev := HookEvent{Kind: KindSessionStart, Member: "alice", Agent: AgentCodex, SessionID: "x1", Where: whereOf("alice")}
			if _, err := h.b.Hook(h.now, ev); err != nil {
				t.Fatal(err)
			}
			ev.Kind, ev.Where = KindPrompt, tc.to
			if _, err := h.b.Hook(h.now, ev); err != nil {
				t.Fatal(err)
			}
		}
		if tc.before {
			other()
		}
		h.aliceFrom(KindPrompt, worktree("b"))
		if !tc.before {
			other()
		}
		if res := h.aliceFrom(KindPreEdit, worktree("b"), "svc/pay/retry.go"); res.Decision != DecisionRefuse ||
			res.Conflicts[0].Severity != SeverityBlock {
			t.Fatalf("with alice's other agent %s, her moved session's edit: %s %v, want a refusal for the reservation",
				tc.name, res.Decision, res.Conflicts)
		}
	}
}

// A note to a member goes where their agents are at work, not to every claim
// one of them keeps live after moving on: the worktree an agent left hears
// it only if the agent comes back, and then again after it heard it where it
// went. Before, the note was queued in both and the agent heard it twice.
func TestANoteToAMemberGoesWhereTheirAgentIs(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.advance(time.Minute)
	h.aliceFrom(KindPrompt, worktree("b"))
	res, err := h.b.Note(h.now, NoteRequest{Member: "bob", Where: whereOf("bob"), To: "alice", Text: "ping", ToMember: true})
	if b := h.b.findClaim("alice", worktree("b")); err != nil || !slices.Equal(res.Delivered, []string{b.ID}) {
		t.Fatalf("note to alice: %+v %v, want it queued in %s, where her agent is", res, err, b.ID)
	}
	heard := 0
	for _, w := range []Where{worktree("b"), whereOf("alice"), worktree("b")} {
		h.advance(time.Minute)
		if strings.Contains(h.aliceFrom(KindPrompt, w).Context, "ping") {
			heard++
		}
	}
	if n := h.b.statsFor(repo).Notes; heard != 1 || n != 1 {
		t.Errorf("alice's agent heard the note %d times, and it counts as %d notes; want once and 1", heard, n)
	}
}

// A note to a member whose only agent moved on to another repository waits
// for their next session in this one, which hears it in whichever worktree.
// Before, it was queued in the claim the agent left, which kept it from the
// member's mailbox, and their next session, in a fresh worktree, never heard
// it.
func TestANoteWaitsForAMemberWhoseAgentMovedToAnotherRepository(t *testing.T) {
	h := newHarness(t)
	h.edit("bob", "b1", "go.mod")
	h.advance(time.Minute)
	other := whereOf("bob")
	other.Repo = "github.com/acme/lib"
	ev := HookEvent{Kind: KindPrompt, Member: "bob", Agent: AgentClaudeCode, SessionID: "b1", Where: other}
	if _, err := h.b.Hook(h.now, ev); err != nil {
		t.Fatal(err)
	}
	res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "bob", Text: "go.mod is mine today", ToMember: true})
	if err != nil || res.HeldFor != "bob" || len(res.Delivered) != 0 {
		t.Fatalf("note to bob, whose agent is in another repository: %+v %v, want it held for him", res, err)
	}
	h.advance(time.Minute)
	if ctx := h.at(KindSessionStart, "bob", "fresh", "b2").Context; !strings.Contains(ctx, "go.mod is mine today") {
		t.Fatalf("bob's next session here, in a fresh worktree, was told:\n%s", ctx)
	}
}

// A session whose workers each report from a worktree of their own, under the
// session's id, keeps every one of those claims live: the parent's
// reservation refuses every one of a teammate's edits, and no claim is
// released and opened again as the session's events move between them. The
// workers' own edits inside it, and of the files the others change, go
// through. Before, with 10 workers, 13 of 600 edits were refused and the
// claims were released and opened again 360 times in 10 minutes.
func TestFanOutKeepsEveryWorktreeLive(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
	h.hook(KindSessionStart, "bob", "b1")
	refused, workers := 0, 0
	for sec := range 600 {
		h.advance(time.Second)
		if sec%30 == 0 {
			h.aliceFrom(KindToolEnd, whereOf("alice"))
		}
		for w := range 10 {
			if sec%(2+w%5) != 0 {
				continue
			}
			where, worker := worktree(string(rune('a'+w))), fmt.Sprintf("agent-%d", w)
			if sec%60 == 0 {
				file := fmt.Sprintf("svc/pay/f%d.go", sec/60)
				if res := h.aliceWorker(KindPreEdit, where, worker, file); res.Decision != DecisionAllow || res.Context != "" {
					t.Fatalf("worker %s's edit of %s in its session's reservation: %s %q", worker, file, res.Decision, res.Context)
				}
				h.aliceWorker(KindPostEdit, where, worker, file)
				workers++
			}
			if res := h.aliceWorker(KindToolEnd, where, worker); res.Context != "" {
				t.Fatalf("worker %s is told %q", worker, res.Context)
			}
		}
		if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); res.Decision == DecisionRefuse {
			refused++
		}
		if sec%15 == 0 {
			h.b.Sweep(h.now)
		}
	}
	if refused != 600 || workers != 100 {
		t.Errorf("%d of 600 of bob's edits refused, want all, and %d workers' edits, want 100", refused, workers)
	}
	if opened, released := len(h.activities(ActivityClaimOpened)), len(h.activities(ActivityClaimReleased)); opened != 12 || released != 0 {
		t.Errorf("%d claims opened and %d released, want 12 (alice's 11 and bob's) and none", opened, released)
	}
	mustIndex(t, h.b, "the fan-out")
}

// A session keeps the maxAlsoClaims claims it reported from last, letting
// go of those it reported from longest ago, and of those it reported from at
// the same moment by ID: what it keeps does not depend on the order of a map.
func TestASessionKeepsTheClaimsItReportedFromLast(t *testing.T) {
	h := newHarness(t)
	var ids []string
	for i := range maxAlsoClaims + 9 {
		if i >= 10 {
			h.advance(time.Second) // the first ten at the same moment
		}
		w := worktree(fmt.Sprintf("w%02d", i))
		h.aliceFrom(KindToolEnd, w)
		ids = append(ids, h.b.findClaim("alice", w).ID)
	}
	// The last claim is the session's own. Of the 40 before it, it lets go
	// of eight, all among the first ten, those with the least IDs.
	tied := slices.Sorted(slices.Values(ids[:10]))
	gone := map[string]bool{}
	for _, id := range tied[:8] {
		gone[id] = true
	}
	s := h.b.sessions[sessionKey("alice", AgentClaudeCode, "s1")]
	if len(s.Also) != maxAlsoClaims {
		t.Fatalf("the session keeps %d claims, want %d", len(s.Also), maxAlsoClaims)
	}
	for _, id := range ids[:len(ids)-1] {
		if _, kept := s.Also[id]; kept == gone[id] {
			t.Errorf("claim %s: kept %t", id, kept)
		}
	}
	mustIndex(t, h.b, "the moves")
}

// A claim taken off the board is no longer kept live by the sessions that
// moved from it.
func TestAClaimTakenOffTheBoardLeavesAlso(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindPostEdit, whereOf("alice"), "svc/pay/retry.go")
	h.advance(time.Second)
	h.aliceFrom(KindToolEnd, worktree("b"))
	s := h.b.sessions[sessionKey("alice", AgentClaudeCode, "s1")]
	c := h.b.findClaim("alice", whereOf("alice"))
	if _, ok := s.Also[c.ID]; !ok {
		t.Fatalf("the session does not keep the claim it moved from: %v", s.Also)
	}
	h.b.mu.Lock()
	h.b.deleteClaim(h.now, c, ActivityClaimForgotten)
	h.b.mu.Unlock()
	if s.Also != nil {
		t.Errorf("the session still keeps %v", s.Also)
	}
	mustIndex(t, h.b, "a claim taken off the board")
}

// A snapshot keeps what each session keeps live, and an older server's,
// which keeps nothing, restores as it was. What a snapshot's session keeps
// that is not on the board, or is its own claim, is let go of.
func TestSnapshotsKeepWhatSessionsKeepLive(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
	h.advance(time.Second)
	h.aliceFrom(KindToolEnd, worktree("b"))
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"also":{"c_1":`) {
		t.Fatalf("the snapshot does not say what the session keeps live:\n%s", data)
	}
	b := New(DefaultConfig())
	if err := b.Restore(strings.NewReader(string(data)), h.now); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, b, "a restore")
	if again, _, _ := b.Snapshot(h.now); string(again) != string(data) {
		t.Fatalf("restored and saved again, the snapshot differs:\n%s\n%s", data, again)
	}
	if !b.liveAt(h.now).claim("c_1") {
		t.Error("after a restore the claim moved from is not live")
	}

	// As an older server wrote it, or one whose claims went since.
	for _, also := range []string{``, `,"also":{"c_2":"2026-10-02T09:00:00Z","c_9":"2026-10-02T09:00:00Z"}`} {
		old := strings.Replace(string(data), `,"also":{"c_1":"2026-10-02T09:00:00Z"}`, also, 1)
		if old == string(data) {
			t.Fatal("the snapshot's session is not as the test expects")
		}
		b := New(DefaultConfig())
		if err := b.Restore(strings.NewReader(old), h.now); err != nil {
			t.Fatal(err)
		}
		mustIndex(t, b, "an older snapshot's restore")
		if again, _, _ := b.Snapshot(h.now); string(again) != strings.Replace(old, also, "", 1) {
			t.Errorf("restored and saved again, the snapshot differs:\n%s\n%s", old, again)
		}
		if b.liveAt(h.now).claim("c_1") {
			t.Error("a claim no session keeps live is live")
		}
	}
}

// A sweep treats the claims a session moved from as it treats the session's
// own: kept, and not forgotten, while the session is live, however long ago
// it last reported from them; and not released while a session that may
// come back, stalled, holds them.
func TestASweepKeepsWhatAMovedSessionKeepsLive(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeExclusive, "rework payments", "svc/pay/**")
	h.aliceFrom(KindPrompt, worktree("b"))
	for range 8 * 24 { // past ForgetAfter, at work in the other worktree
		h.advance(time.Hour)
		h.aliceFrom(KindToolEnd, worktree("b"))
		h.b.Sweep(h.now)
	}
	if h.b.findClaim("alice", whereOf("alice")) == nil {
		t.Fatal("the sweep forgot the claim a live session moved from")
	}
	h.hook(KindSessionStart, "bob", "b1")
	if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Fatalf("bob's edit inside alice's reservation, a week on: %s", res.Decision)
	}

	// A claim with nothing in it, moved from, then the session stalls.
	h.aliceFrom(KindToolEnd, worktree("c"))
	h.aliceFrom(KindToolEnd, worktree("d"))
	h.advance(h.b.cfg.StallAfter + time.Minute)
	h.b.Sweep(h.now)
	if h.b.findClaim("alice", worktree("c")) == nil {
		t.Fatal("the sweep released the claim a stalled session moved from")
	}
	mustIndex(t, h.b, "the sweeps")
}

// When a session ends, every claim it kept live that has nothing left to
// tell anyone is released at once, as its own claim is, rather than an hour
// later when a sweep lets go of the session.
func TestASessionsEndReleasesTheClaimsItMovedFrom(t *testing.T) {
	h := newHarness(t)
	h.aliceFrom(KindSessionStart, whereOf("alice"))
	h.declare("alice", ModeShared, "", "docs/**") // the first keeps an intent
	h.aliceFrom(KindToolEnd, worktree("b"))
	h.aliceFrom(KindToolEnd, worktree("c"))
	h.aliceFrom(KindSessionEnd, worktree("d"))
	released := map[string]bool{}
	for _, a := range h.activities(ActivityClaimReleased) {
		released[a.ClaimID] = true
	}
	for _, w := range []Where{whereOf("alice"), worktree("b"), worktree("c"), worktree("d")} {
		c := h.b.findClaim("alice", w)
		if keep := w == whereOf("alice"); (c != nil) != keep {
			t.Errorf("claim in %s: kept %t, want %t (released: %v)", w.Worktree, c != nil, keep, released)
		}
	}
	mustIndex(t, h.b, "the session's end")
}
