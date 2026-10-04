package board

import (
	"errors"
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
		advisory bool // a late one is dropped
		delivers bool // one answered in time carries the note
		applied  func(h *harness) bool
	}{
		{name: "pre_edit", ev: HookEvent{Kind: KindPreEdit, Tool: "Edit", ToolUseID: "t1", Paths: refs("svc/pay/retry.go")}, advisory: true,
			applied: func(h *harness) bool { return h.b.View(h.now, repo).Stats.Checks == 3 }}, // after bob's edit and hers
		{name: "tool_start", ev: HookEvent{Kind: KindToolStart, Tool: "Grep", ToolUseID: "t1"}, advisory: true,
			applied: func(h *harness) bool { return h.toolOf("alice") == "Grep" }},
		{name: "heartbeat", ev: HookEvent{Kind: KindHeartbeat}, advisory: true, delivers: true,
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
				dropped := late && row.advisory
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
