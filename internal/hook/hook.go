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
	// LateContext says the agent drops context given at this point, so the
	// board should hold it for the next event.
	LateContext bool
	// NoAsk says the agent cannot put a question to its person here; the
	// board then lets the retry of an asked edit through.
	NoAsk bool
}

// askInstead turns an "ask" into a refusal for agents that cannot ask: the
// board lets the same edit through on its retry.
const askInstead = "\nAsk your user whether to go ahead. If they agree, retry the same edit and intagent will let it through."

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
	case "", "claude-code", "claude", "claudecode":
		return ClaudeCode{}, nil
	case "copilot":
		return ClaudeCode{Copilot: true}, nil
	case "codex":
		return Codex{}, nil
	case "cursor":
		return Cursor{}, nil
	case "gemini":
		return Gemini{}, nil
	}
	return nil, fmt.Errorf("unknown agent %q: want claude-code, codex, cursor, copilot or gemini", name)
}

// Detect picks the adapter for one invocation. name is the agent named on the
// command line, if any. The payload wins when it is unmistakably Cursor's,
// because Cursor also runs the hooks in Claude Code's settings files and sends
// them its own payloads. GitHub Copilot CLI runs those hooks too, with
// Claude-style payloads, and is recognised by its environment.
func Detect(name string, stdin []byte, getenv func(string) string) (Adapter, error) {
	if isCursorPayload(stdin) {
		return Cursor{}, nil
	}
	ad, err := For(name)
	if err != nil {
		return nil, err
	}
	if cc, ok := ad.(ClaudeCode); ok && getenv("COPILOT_CLI") == "1" {
		cc.Copilot = true
		return cc, nil
	}
	return ad, nil
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
