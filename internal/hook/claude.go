package hook

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patakil/intagent/internal/board"
)

// ClaudeCode speaks Claude Code's hook protocol (verified against v2.1.288).
type ClaudeCode struct{}

// Agent names the agent.
func (ClaudeCode) Agent() board.Agent { return board.AgentClaudeCode }

type claudeInput struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Prompt        string          `json:"prompt"`
}

// claudeEditPath names the tool_input field that holds the path each
// file-writing tool writes.
var claudeEditPath = map[string]string{
	"Edit":         "file_path",
	"Write":        "file_path",
	"MultiEdit":    "file_path",
	"NotebookEdit": "notebook_path",
}

// Parse reads a Claude Code hook payload.
func (ClaudeCode) Parse(stdin []byte) (Event, error) {
	var in claudeInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Event{}, fmt.Errorf("claude-code hook payload: %w", err)
	}
	ev := Event{Name: in.HookEventName, SessionID: in.SessionID, Cwd: in.Cwd, Tool: in.ToolName}
	field, isEdit := claudeEditPath[in.ToolName]
	switch in.HookEventName {
	case "SessionStart":
		ev.Kind, ev.Footprint = board.KindSessionStart, true
	case "UserPromptSubmit":
		ev.Kind = board.KindPrompt
		// Background task notifications arrive as prompts; they describe no task.
		if !strings.HasPrefix(strings.TrimSpace(in.Prompt), "<task-notification>") {
			ev.Prompt = in.Prompt
		}
	case "PreToolUse":
		ev.Kind = board.KindToolStart
		if isEdit {
			ev.Kind = board.KindPreEdit
			if p := stringField(in.ToolInput, field); p != "" {
				ev.Paths = []string{p}
			}
		}
	case "PostToolUse":
		ev.Kind = board.KindToolEnd
		switch {
		case isEdit:
			ev.Kind = board.KindPostEdit
			if p := stringField(in.ToolInput, field); p != "" {
				ev.Paths = []string{p}
			}
		case in.ToolName == "Bash" || in.ToolName == "PowerShell":
			// Shell commands can write files without saying which.
			ev.Footprint = true
		}
	case "PostToolUseFailure", "SubagentStop":
		ev.Kind = board.KindToolEnd
	case "Stop":
		ev.Kind, ev.Footprint = board.KindStop, true
	case "SessionEnd":
		ev.Kind, ev.Footprint = board.KindSessionEnd, true
	default:
		ev.Skip = true
	}
	if !ev.Skip && ev.SessionID == "" {
		return Event{}, fmt.Errorf("claude-code hook payload has no session_id")
	}
	return ev, nil
}

// Render answers in the JSON Claude Code expects; anything else would be ignored.
func (ClaudeCode) Render(ev Event, res board.HookResult) Output {
	switch ev.Name {
	case "PreToolUse":
		switch res.Decision {
		case board.Refuse:
			return specific(hookSpecific{HookEventName: ev.Name, PermissionDecision: "deny", PermissionDecisionReason: res.Reason})
		case board.DecideAsk:
			return specific(hookSpecific{HookEventName: ev.Name, PermissionDecision: "ask", PermissionDecisionReason: res.Reason})
		}
		if res.Context != "" {
			return specific(hookSpecific{HookEventName: ev.Name, AdditionalContext: res.Context})
		}
	case "SessionStart", "UserPromptSubmit", "PostToolUse", "PostToolUseFailure":
		if res.Context != "" {
			return specific(hookSpecific{HookEventName: ev.Name, AdditionalContext: res.Context})
		}
	}
	return Output{}
}
