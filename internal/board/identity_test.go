package board

import (
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
