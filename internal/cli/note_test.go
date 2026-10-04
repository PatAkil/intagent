package cli

import (
	"strings"
	"testing"
)

// A note to a teammate with no agent in the repository yet says it waits
// for their next session there, which hears it.
func TestNoteToATeammateNotHereYetIsHeld(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	out, errOut, code := tm.as("alice", a, "", "note", "bob", "start", "with", "the", "docs")
	if code != 0 || !strings.Contains(out, "Note held for bob's next session in this repository.") {
		t.Fatalf("note: %d %s %s", code, out, errOut)
	}
	out, _, _ = tm.as("bob", b, claudeEvent("b1", b, "SessionStart", map[string]any{"source": "startup"}), "hook")
	if !strings.Contains(out, "start with the docs") {
		t.Fatalf("bob's session start: %s", out)
	}
}
