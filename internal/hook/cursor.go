package hook

import (
	"encoding/json"
	"fmt"
	"unicode"

	"github.com/patakil/intagent/internal/board"
)

// Cursor speaks the hooks protocol of Cursor's agent runtime (verified against
// @cursor/sdk 1.0.35). Edits reach preToolUse as tool "Write" (or "Delete")
// with tool_input.file_path, so Cursor can be refused before a write, like
// Claude Code and Codex.
//
// Cursor treats invalid JSON from a permission step as a refusal, so every
// answer here is valid JSON, and an allow is spelled out.
type Cursor struct{}

// Agent names the agent.
func (Cursor) Agent() board.Agent { return board.AgentCursor }

type cursorInput struct {
	ConversationID string          `json:"conversation_id"`
	SessionID      string          `json:"session_id"`
	HookEventName  string          `json:"hook_event_name"`
	WorkspaceRoots []string        `json:"workspace_roots"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
	FilePath       string          `json:"file_path"`
	Prompt         string          `json:"prompt"`
	Cwd            string          `json:"cwd"`
	CursorVersion  string          `json:"cursor_version"`
}

// isCursorPayload recognises Cursor's payloads, which Cursor also sends to
// hooks it imports from Claude Code's settings files.
func isCursorPayload(stdin []byte) bool {
	var in cursorInput
	if json.Unmarshal(stdin, &in) != nil {
		return false
	}
	if in.CursorVersion != "" {
		return true
	}
	r := []rune(in.HookEventName)
	return len(r) > 0 && unicode.IsLower(r[0]) && in.ConversationID != ""
}

// Parse reads a Cursor hook payload.
func (Cursor) Parse(stdin []byte) (Event, error) {
	var in cursorInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Event{}, fmt.Errorf("cursor hook payload: %w", err)
	}
	session := in.ConversationID
	if session == "" {
		session = in.SessionID
	}
	ev := Event{Name: in.HookEventName, SessionID: session, Tool: in.ToolName, ToolUseID: in.ToolUseID}
	// Paths resolve against the workspace; a shell command reports its own cwd.
	if len(in.WorkspaceRoots) > 0 {
		ev.Cwd = in.WorkspaceRoots[0]
	}
	if ev.Cwd == "" {
		ev.Cwd = in.Cwd
	}
	path := stringField(in.ToolInput, "file_path")
	isWrite := in.ToolName == "Write" || in.ToolName == "Delete"
	switch in.HookEventName {
	case "sessionStart":
		ev.Kind, ev.Footprint = board.KindSessionStart, true
	case "beforeSubmitPrompt":
		ev.Kind, ev.Prompt = board.KindPrompt, in.Prompt
	case "preToolUse":
		ev.Kind = board.KindToolStart
		if isWrite && path != "" {
			ev.Kind, ev.Paths, ev.NoAsk = board.KindPreEdit, []string{path}, true
		}
	case "postToolUse":
		ev.Kind = board.KindToolEnd
		switch {
		case isWrite && path != "":
			ev.Kind, ev.Paths = board.KindPostEdit, []string{path}
		case in.ToolName == "Shell":
			ev.Footprint = true
		}
	case "postToolUseFailure":
		ev.Kind = board.KindToolEnd
	case "afterFileEdit":
		// Older Cursor versions report edits only after the fact.
		ev.Kind, ev.Tool = board.KindPostEdit, "Write"
		if in.FilePath != "" {
			ev.Paths = []string{in.FilePath}
		}
	case "beforeShellExecution":
		ev.Kind, ev.Tool = board.KindToolStart, "Shell"
	case "afterShellExecution":
		ev.Kind, ev.Tool, ev.Footprint = board.KindToolEnd, "Shell", true
	case "stop":
		ev.Kind, ev.Footprint = board.KindStop, true
	case "sessionEnd":
		ev.Kind, ev.Footprint = board.KindSessionEnd, true
	default:
		ev.Skip = true
	}
	if !ev.Skip && ev.SessionID == "" {
		return Event{}, fmt.Errorf("cursor hook payload has no conversation_id")
	}
	return ev, nil
}

// Render answers in Cursor's format.
func (Cursor) Render(ev Event, res board.HookResult) Output {
	out := map[string]any{}
	switch ev.Name {
	case "preToolUse":
		switch res.Decision {
		case board.Refuse, board.DecideAsk:
			// Cursor ignores "ask" on preToolUse, so asking becomes a refusal that
			// tells the agent to ask.
			reason := res.Reason
			if res.Decision == board.DecideAsk {
				reason += askInstead
			}
			out["permission"] = "deny"
			out["user_message"] = reason
			out["agent_message"] = reason
		default:
			out["permission"] = "allow"
		}
		if res.Context != "" {
			out["additional_context"] = res.Context
		}
	case "beforeShellExecution":
		out["permission"] = "allow"
	case "beforeSubmitPrompt":
		out["continue"] = true
		if res.Context != "" {
			out["additional_context"] = res.Context
		}
	case "sessionStart", "postToolUse", "postToolUseFailure":
		if res.Context != "" {
			out["additional_context"] = res.Context
		}
	}
	return jsonOut(out)
}

func jsonOut(v any) Output {
	b, err := json.Marshal(v)
	if err != nil {
		return Output{Stdout: []byte("{}\n")}
	}
	return Output{Stdout: append(b, '\n')}
}
