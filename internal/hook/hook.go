// Package hook translates between agents' hook protocols and intagent's
// normalised lifecycle events. Each adapter parses its agent's stdin payload
// and renders the server's answer in exactly the shape that agent accepts,
// because every agent silently ignores output it does not understand.
package hook

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patakil/intagent/internal/board"
)

// Event is what an adapter extracts from one hook invocation.
type Event struct {
	// Kind is the normalised event; Skip means intagent has nothing to do.
	Kind board.Kind
	Skip bool
	// Name is the agent's own event name, needed to render the answer.
	Name      string
	SessionID string
	// Cwd is the agent's working directory; relative Paths resolve against it.
	Cwd   string
	Tool  string
	Paths []string
	// Prompt is the user's prompt on prompt events.
	Prompt string
	// Footprint asks the caller to reconcile the worktree's git changes.
	Footprint bool
}

// Output is what the hook process writes and how it exits.
type Output struct {
	Stdout []byte
	Exit   int
}

// Adapter speaks one agent's hook protocol.
type Adapter interface {
	Agent() board.Agent
	Parse(stdin []byte) (Event, error)
	Render(ev Event, res board.HookResult) Output
}

// For returns the adapter for an agent name as used on the command line.
func For(name string) (Adapter, error) {
	switch strings.ToLower(name) {
	case "claude-code", "claude", "claudecode":
		return ClaudeCode{}, nil
	case "codex":
		return Codex{}, nil
	case "cursor":
		return Cursor{}, nil
	}
	return nil, fmt.Errorf("unknown agent %q: want claude-code, codex or cursor", name)
}

// hookSpecific is the hookSpecificOutput object shared by Claude Code and Codex.
type hookSpecific struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

func specific(h hookSpecific) Output {
	b, err := json.Marshal(struct {
		HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
	}{h})
	if err != nil {
		return Output{}
	}
	return Output{Stdout: append(b, '\n')}
}

// stringField reads a string field from a raw JSON object.
func stringField(raw json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
