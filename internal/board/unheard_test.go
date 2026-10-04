package board

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// alicesSession is alice's only session as the board shows it.
func alicesSession(t *testing.T, h *harness) SessionView {
	t.Helper()
	for _, c := range h.b.View(h.now, repo).Claims {
		if c.Member == "alice" && len(c.Sessions) == 1 {
			return c.Sessions[0]
		}
	}
	t.Fatal("alice has no single session on the board")
	return SessionView{}
}

func changed(h *harness, member, path string) bool {
	for _, c := range h.b.View(h.now, repo).Claims {
		if c.Member != member {
			continue
		}
		for _, f := range c.Files {
			if f.Path == path {
				return true
			}
		}
	}
	return false
}

func conflictsOf(h *harness, member string) int {
	n := 0
	for _, a := range h.activities(ActivityConflict) {
		if a.Member == member {
			n++
		}
	}
	return n
}

// An event whose agent stopped waiting: a question changes nothing, a fact is
// recorded, and neither delivers what the session has not heard yet, nor
// spends a one-time answer.
func TestLateEventsAreRecordedButNotAnswered(t *testing.T) {
	fp := &Footprint{Files: refs("docs/own.md", "svc/new.go")}
	rows := []struct {
		name     string
		ev       HookEvent
		question bool // a late one is dropped
		delivers bool // one answered in time carries the note
		applied  func(h *harness) bool
	}{
		{name: "pre_edit", ev: HookEvent{Kind: KindPreEdit, Tool: "Edit", ToolUseID: "t1", Paths: refs("svc/pay/retry.go")}, question: true,
			applied: func(h *harness) bool { return h.b.View(h.now, repo).Stats.Checks == 3 }}, // after bob's edit and hers
		{name: "tool_start", ev: HookEvent{Kind: KindToolStart, Tool: "Grep", ToolUseID: "t1"},
			applied: func(h *harness) bool { return h.toolOf("alice") == "Grep" }},
		{name: "heartbeat", ev: HookEvent{Kind: KindHeartbeat}, delivers: true,
			applied: func(h *harness) bool { return alicesSession(h.t, h).LastSeen.Equal(h.now) }},
		{name: "heartbeat with a footprint", ev: HookEvent{Kind: KindHeartbeat, Footprint: fp}, delivers: true,
			applied: func(h *harness) bool { return changed(h, "alice", "svc/new.go") }},
		{name: "session_start", ev: HookEvent{Kind: KindSessionStart, Footprint: fp}, delivers: true,
			applied: func(h *harness) bool { return changed(h, "alice", "svc/new.go") }},
		{name: "prompt", ev: HookEvent{Kind: KindPrompt, Prompt: "Tidy up"}, delivers: true,
			applied: func(h *harness) bool { return h.b.View(h.now, repo).Claims[0].Task == "Tidy up" }},
		{name: "post_edit", ev: HookEvent{Kind: KindPostEdit, Tool: "Edit", Paths: refs("docs/a.md")}, delivers: true,
			applied: func(h *harness) bool { return changed(h, "alice", "docs/a.md") }},
		{name: "tool_end", ev: HookEvent{Kind: KindToolEnd, Tool: "Bash", ToolUseID: "t0"}, delivers: true,
			applied: func(h *harness) bool { return h.toolOf("alice") == "" }},
		{name: "stop", ev: HookEvent{Kind: KindStop, Footprint: fp},
			applied: func(h *harness) bool { return alicesSession(h.t, h).State == StateWaiting }},
		{name: "session_end", ev: HookEvent{Kind: KindSessionEnd, Footprint: fp},
			applied: func(h *harness) bool { return alicesSession(h.t, h).State == StateEnded }},
	}
	for _, row := range rows {
		for _, late := range []bool{false, true} {
			name := row.name + "/in time"
			if late {
				name = row.name + "/late"
			}
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.hook(KindPrompt, "bob", "b1")
				h.edit("bob", "b1", "svc/pay/retry.go")
				h.hook(KindPrompt, "alice", "a1")
				h.edit("alice", "a1", "docs/own.md") // keeps her claim when her session ends
				if _, err := h.b.Hook(h.now, HookEvent{Kind: KindToolStart, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1",
					Where: whereOf("alice"), Tool: "Bash", ToolUseID: "t0"}); err != nil {
					t.Fatal(err)
				}
				if _, err := h.b.Note(h.now, NoteRequest{Member: "bob", Where: whereOf("bob"), To: "alice", Text: "I renamed UserID"}); err != nil {
					t.Fatal(err)
				}
				h.advance(time.Minute)
				asked := 0
				ev := row.ev
				ev.Member, ev.Agent, ev.SessionID, ev.Where = "alice", AgentClaudeCode, "a1", whereOf("alice")
				ev.Late = func() bool { asked++; return late }

				res, err := h.b.Hook(h.now, ev)

				if asked != 1 {
					t.Errorf("Late asked %d times, want once", asked)
				}
				dropped := late && row.question
				if got := errors.Is(err, ErrAbandoned); got != dropped || (err != nil && !dropped) {
					t.Fatalf("err = %v, want abandoned %v", err, dropped)
				}
				if res.Decision != DecisionAllow && late {
					t.Errorf("late answer decided %s", res.Decision)
				}
				if res.Unchecked != dropped {
					t.Errorf("Unchecked = %v, want %v", res.Unchecked, dropped)
				}
				if got := row.applied(h); got == dropped {
					t.Errorf("event applied = %v, want %v", got, !dropped)
				}
				if late && (res.Context != "" || res.Reason != "") {
					t.Errorf("a late answer says something:\n%s%s", res.Context, res.Reason)
				}
				told := strings.Contains(res.Context, "I renamed UserID")
				if want := !late && row.delivers; told != want {
					t.Errorf("note delivered = %v, want %v:\n%s", told, want, res.Context)
				}
				if st := h.b.View(h.now, repo).Stats; (st.Unheard == 1) != (dropped && ev.Kind == KindPreEdit) {
					t.Errorf("unheard = %d", st.Unheard)
				}
				spent := ev.Kind == KindPreEdit && !late
				if n := conflictsOf(h, "alice"); n != map[bool]int{true: 1}[spent] {
					t.Errorf("%d collisions announced for alice", n)
				}

				// What she did not hear waits for her next answer, and the bump
				// she did not hear still stops her next edit of bob's file.
				if again := h.hook(KindPrompt, "alice", "a1").Context; strings.Contains(again, "I renamed UserID") == told {
					t.Errorf("note told %v by the event and %v after it", told, !told)
				}
				if next := h.hook(KindPreEdit, "alice", "a1", "svc/pay/retry.go"); (next.Decision == DecisionRefuse) == spent {
					t.Errorf("next edit of bob's file: %s (bump spent by the event: %v)", next.Decision, spent)
				}
			})
		}
	}
}

// A late tool start is a fact: alice's agent is inside a long shell command,
// whose start it never waits to hear answered. Her session keeps the longer
// stall threshold of a tool call, and her reservation keeps blocking edits.
func TestLateToolStartKeepsTheSessionInItsTool(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "Rewrite payments", "svc/pay/**")
	h.hook(KindPrompt, "bob", "b1")
	h.advance(time.Second)
	if _, err := h.b.Hook(h.now, HookEvent{Kind: KindToolStart, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1",
		Where: whereOf("alice"), Tool: "Bash", ToolUseID: "sh1", Late: func() bool { return true }}); err != nil && !errors.Is(err, ErrAbandoned) {
		t.Fatal(err)
	}
	for range 20 { // twenty minutes into her test run, with bob's agent at work
		h.advance(time.Minute)
		h.hook(KindHeartbeat, "bob", "b1")
		h.b.Sweep(h.now)
	}
	if n := len(h.activities(ActivitySessionStalled)); n != 0 {
		t.Fatalf("%d stalls announced for a session inside a tool call", n)
	}
	if res := h.hook(KindPreEdit, "bob", "b1", "svc/pay/retry.go"); len(res.Conflicts) != 1 || res.Conflicts[0].Severity != SeverityBlock {
		t.Fatalf("bob's edit inside alice's reservation: %s %+v, want blocked", res.Decision, res.Conflicts)
	}
}

// A late question for a member the board has never seen creates nothing; it
// is counted as an edit that went ahead unchecked.
func TestLateQuestionCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	_, err := h.b.Hook(h.now, HookEvent{Kind: KindPreEdit, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
		Tool: "Edit", Paths: refs("a.go"), Late: func() bool { return true }})
	if !errors.Is(err, ErrAbandoned) {
		t.Fatalf("err = %v", err)
	}
	if v := h.b.View(h.now, repo); len(v.Claims) != 1 || len(h.activities(ActivitySessionStarted)) != 1 || v.Stats.Unheard != 1 {
		t.Fatalf("claims %d, unheard %d", len(v.Claims), v.Stats.Unheard)
	}
	mustContain(t, h.b.View(h.now, repo).Text(), "1 edit went ahead unchecked")
}

func (h *harness) call(kind Kind, member, session, id string, paths ...string) HookResult {
	h.t.Helper()
	res, err := h.b.Hook(h.now, HookEvent{Kind: kind, Member: member, Agent: AgentClaudeCode, SessionID: session, Where: whereOf(member),
		Tool: "Edit", ToolUseID: id, Paths: refs(paths...)})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// An edit whose check the agent never heard is checked when the edit is
// reported, and what the check finds is told then, as information, without
// spending the bump the agent's next edit of the file gets.
func TestEditMadeWithoutACheckIsToldAfterwards(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	h.edit("bob", "b1", "svc/pay/retry.go")
	h.hook(KindPrompt, "alice", "a1")

	// A call checked as usual says nothing more when it ends, nor does one
	// the board cannot tell from another.
	h.call(KindPreEdit, "alice", "a1", "t1", "docs/a.md")
	if res := h.call(KindPostEdit, "alice", "a1", "t1", "docs/a.md"); res.Context != "" || res.CheckedAfter {
		t.Fatalf("checked edit: %+v", res)
	}
	if res := h.call(KindPostEdit, "alice", "a1", "", "svc/pay/retry.go"); res.Context != "" || res.CheckedAfter {
		t.Fatalf("edit without an id: %+v", res)
	}
	// One whose pre_edit never arrived is told what its check would have said.
	res := h.call(KindPostEdit, "alice", "a1", "t2", "svc/pay/retry.go")
	mustContain(t, res.Context, "Your edit of svc/pay/retry.go was not checked before it ran", dataNotice,
		"bob's agent", "has unmerged changes to this file", "Keep your change compatible")
	if !res.CheckedAfter {
		t.Fatal("the answer does not say the edit was checked after it ran")
	}
	if res := h.call(KindPreEdit, "alice", "a1", "t3", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Fatalf("next edit of the file: %s, want the bump it has not had", res.Decision)
	}
	if n := conflictsOf(h, "alice"); n != 1 {
		t.Fatalf("%d collisions announced, want the bump's alone", n)
	}
}

// A refusal whose post_edit arrives never reached the agent, which made the
// edit: it hears the check then, and its next edit of the file is refused
// again, not let through as the retry of a refusal it heard.
func TestRefusalTheAgentDidNotHearIsUnspent(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	h.edit("bob", "b1", "svc/pay/retry.go")
	h.hook(KindPrompt, "alice", "a1")

	if res := h.call(KindPreEdit, "alice", "a1", "t1", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Fatalf("first edit: %s", res.Decision)
	}
	res := h.call(KindPostEdit, "alice", "a1", "t1", "svc/pay/retry.go")
	mustContain(t, res.Context, "was not checked before it ran", "bob's agent")
	if res := h.call(KindPreEdit, "alice", "a1", "t2", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Fatalf("next edit after the unheard refusal: %s, want refused", res.Decision)
	}
	if res := h.call(KindPreEdit, "alice", "a1", "t3", "svc/pay/retry.go"); res.Decision != DecisionAllow {
		t.Fatalf("retry of a refusal she heard: %s, want allowed", res.Decision)
	}
}

// An unheard refusal gives back what it spent and nothing more. Alice heard
// bob's bump on retry.go and edited it; a later patch of retry.go and a file
// bob reserved is refused for the reservation alone, and she never hears it.
// Her next edit of retry.go is still the retry of a bump she heard.
func TestUnheardRefusalGivesBackOnlyWhatItSpent(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	h.edit("bob", "b1", "svc/pay/retry.go")
	h.hook(KindPrompt, "alice", "a1")
	if res := h.call(KindPreEdit, "alice", "a1", "t1", "svc/pay/retry.go"); res.Decision != DecisionRefuse {
		t.Fatalf("first edit: %s, want the bump", res.Decision)
	}
	h.call(KindPreEdit, "alice", "a1", "t2", "svc/pay/retry.go")
	h.call(KindPostEdit, "alice", "a1", "t2", "svc/pay/retry.go")
	h.declare("bob", ModeExclusive, "Rewrite the guide", "docs/**")

	if res := h.call(KindPreEdit, "alice", "a1", "t3", "svc/pay/retry.go", "docs/guide.md"); res.Decision != DecisionRefuse {
		t.Fatalf("patch: %s", res.Decision)
	}
	h.call(KindPostEdit, "alice", "a1", "t3", "svc/pay/retry.go", "docs/guide.md")

	if res := h.call(KindPreEdit, "alice", "a1", "t4", "svc/pay/retry.go"); res.Decision != DecisionAllow {
		t.Fatalf("next edit of retry.go, whose bump she heard at t1: %s, want allow", res.Decision)
	}
}

// An agent that cannot ask its person is told to; when it never hears that,
// the question is given back, and its next attempt is told again.
func TestUnheardQuestionIsAskedAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy.Overlap = ActionAsk })
	h.hook(KindPrompt, "bob", "b1")
	h.edit("bob", "b1", "svc/pay/retry.go")
	codex := func(kind Kind, id string) HookResult {
		t.Helper()
		res, err := h.b.Hook(h.now, HookEvent{Kind: kind, Member: "alice", Agent: AgentCodex, SessionID: "a1", Where: whereOf("alice"),
			Tool: "apply_patch", ToolUseID: id, Paths: refs("svc/pay/retry.go"), NoAsk: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := codex(KindPreEdit, "t1"); res.Decision != DecisionAsk {
		t.Fatalf("first edit: %s, want the question", res.Decision)
	}
	codex(KindPostEdit, "t1")
	if res := codex(KindPreEdit, "t2"); res.Decision != DecisionAsk {
		t.Fatalf("next edit after the unheard question: %s, want it asked again", res.Decision)
	}
	if res := codex(KindPreEdit, "t3"); res.Decision != DecisionAllow {
		t.Fatalf("retry of a question it heard: %s, want allowed", res.Decision)
	}
}

// Refused calls are remembered while others are refused and end, a few at a
// time.
func TestRefusedCallsAreRememberedAFewAtATime(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	h.declare("bob", ModeExclusive, "Mine", "svc/**")
	h.hook(KindPrompt, "alice", "a1")
	for i := range maxRefused + 2 {
		h.call(KindToolStart, "alice", "a1", fmt.Sprintf("shell%d", i))
		if res := h.call(KindPreEdit, "alice", "a1", fmt.Sprintf("t%d", i), "svc/x.go"); res.Decision != DecisionRefuse {
			t.Fatalf("edit %d: %s", i, res.Decision)
		}
		h.call(KindToolEnd, "alice", "a1", fmt.Sprintf("shell%d", i))
	}
	s := h.b.sessions[sessionKey("alice", AgentClaudeCode, "a1")]
	if len(s.refused) != maxRefused || s.refused[0].id != "t2" || s.refused[maxRefused-1].id != fmt.Sprintf("t%d", maxRefused+1) {
		t.Fatalf("refused calls remembered: %q", s.refused)
	}
}

// An unchecked edit inside an active reservation is a breach, recorded once
// per file, and the agent is told to undo it or agree it.
func TestUncheckedEditInsideAReservationIsABreach(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	h.declare("bob", ModeExclusive, "Rewrite the guide", "docs/**")
	h.hook(KindPrompt, "alice", "a1")

	res := h.call(KindPostEdit, "alice", "a1", "t1", "docs/guide.md")
	mustContain(t, res.Context, "docs/guide.md was not checked", `"Rewrite the guide"`, "Undo your change", "tell your user")
	got := breaches(h)
	if len(got) != 1 || got[0].Member != "alice" || got[0].Severity != SeverityBlock || got[0].Decision != DecisionAllow ||
		got[0].Paths[0] != "docs/guide.md" {
		t.Fatalf("breaches = %+v", got)
	}
	h.call(KindPostEdit, "alice", "a1", "t2", "docs/guide.md")
	if n := len(breaches(h)); n != 1 {
		t.Fatalf("%d breaches for one file", n)
	}
	// A change git found first is not reported again by the edit's report.
	scan(t, h, "alice", "a1", "docs/guide.md", "docs/index.md")
	if n := len(breaches(h)); n != 2 {
		t.Fatalf("%d breaches after the scan found a second file", n)
	}
	h.call(KindPostEdit, "alice", "a1", "t3", "docs/index.md")
	if n := len(breaches(h)); n != 2 {
		t.Fatalf("%d breaches: the scan's file reported twice", n)
	}
}

// An unchecked edit reported late is held, like everything else nobody
// would read, for the session's next answer.
func TestUncheckedEditReportedLateIsToldNextTime(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "bob", "b1")
	h.edit("bob", "b1", "svc/pay/retry.go")
	h.hook(KindPrompt, "alice", "a1")
	res, err := h.b.Hook(h.now, HookEvent{Kind: KindPostEdit, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice"),
		Tool: "Edit", ToolUseID: "t1", Paths: refs("svc/pay/retry.go"), Late: func() bool { return true }})
	if err != nil || res.Context != "" {
		t.Fatalf("late post_edit: %v %q", err, res.Context)
	}
	mustContain(t, h.hook(KindPrompt, "alice", "a1").Context, "Your edit of svc/pay/retry.go was not checked", "bob's agent")
}
