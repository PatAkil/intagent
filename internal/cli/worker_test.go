package cli

import (
	"path/filepath"
	"testing"
)

// Two subagents of one Claude Code session, editing a file a teammate
// changed, are each refused once and told why: the hook sends the board the
// agent_id each reports with. A subagent's SubagentStop makes the board
// forget what it told that subagent, so one run again is told again.
func TestSubagentsEachHearTheirBump(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	edit := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "svc/pay/retry.go")}}
	tm.as("alice", a, claudeEvent("a1", a, "PreToolUse", edit), "hook", "claude-code")
	tm.as("alice", a, claudeEvent("a1", a, "PostToolUse", edit), "hook", "claude-code")

	bob := func(event, worker string) (string, string) {
		t.Helper()
		ev := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(b, "svc/pay/retry.go")},
			"agent_id": worker, "agent_type": "general-purpose"}
		out, errOut, code := tm.as("bob", b, claudeEvent("b1", b, event, ev), "hook", "claude-code")
		if code != 0 {
			t.Fatalf("bob's %s from %s: exit %d %s", event, worker, code, errOut)
		}
		dec, reason, _ := decision(t, out)
		return dec, reason
	}
	for _, w := range []string{"agent-1", "agent-2"} {
		if dec, reason := bob("PreToolUse", w); dec != "deny" {
			t.Fatalf("%s's first edit: %q %q, want a refusal", w, dec, reason)
		}
	}
	for _, w := range []string{"agent-1", "agent-2"} {
		if dec, _ := bob("PreToolUse", w); dec != "" {
			t.Fatalf("%s's retry: %q, want it let through", w, dec)
		}
	}
	if dec, _ := bob("SubagentStop", "agent-1"); dec != "" {
		t.Fatalf("SubagentStop answered %q", dec)
	}
	if dec, _ := bob("PreToolUse", "agent-1"); dec != "deny" {
		t.Fatalf("agent-1 run again: %q, want a refusal", dec)
	}
	if dec, _ := bob("PreToolUse", "agent-2"); dec != "" {
		t.Fatalf("agent-2 after agent-1 stopped: %q, want it let through", dec)
	}
}
