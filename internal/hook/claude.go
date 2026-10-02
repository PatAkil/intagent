package hook

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patakil/intagent/internal/board"
)

// ClaudeCode speaks Claude Code's hook protocol (verified against v2.1.288).
// GitHub Copilot CLI runs the same hooks with the same payloads, except that
// tool arguments keep Copilot's names (path instead of file_path); Copilot is
// set for it.
type ClaudeCode struct {
	Copilot bool
}

// Agent names the agent.
func (c ClaudeCode) Agent() board.Agent {
	if c.Copilot {
		return board.AgentCopilot
	}
	return board.AgentClaudeCode
}

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
func (c ClaudeCode) Parse(stdin []byte) (Event, error) {
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
			if p := editPath(in.ToolInput, field); p != "" {
				ev.Paths = []string{p}
			}
		}
	case "PostToolUse":
		ev.Kind = board.KindToolEnd
		switch {
		case isEdit:
			ev.Kind = board.KindPostEdit
			if p := editPath(in.ToolInput, field); p != "" {
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
		return Event{}, fmt.Errorf("%s hook payload has no session_id", c.Agent())
	}
	return ev, nil
}

// editPath reads the written path, under Claude Code's or Copilot's field name.
func editPath(input json.RawMessage, field string) string {
	if p := stringField(input, field); p != "" {
		return p
	}
	return stringField(input, "path")
}

// Render answers in the JSON Claude Code expects; anything else would be ignored.
func (c ClaudeCode) Render(ev Event, res board.HookResult) Output {
	if c.Copilot {
		return renderCopilot(ev, res)
	}
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

// renderCopilot answers GitHub Copilot CLI, which honours both the nested
// Claude form and its own top-level fields; it gets both.
func renderCopilot(ev Event, res board.HookResult) Output {
	h := hookSpecific{HookEventName: ev.Name, AdditionalContext: res.Context}
	top := map[string]any{}
	if res.Context != "" {
		top["additionalContext"] = res.Context
	}
	if ev.Name == "PreToolUse" && res.Decision != board.Allow {
		h.PermissionDecision, h.PermissionDecisionReason = string(res.Decision), res.Reason
		top["permissionDecision"], top["permissionDecisionReason"] = string(res.Decision), res.Reason
	}
	if h.PermissionDecision == "" && h.AdditionalContext == "" {
		return Output{}
	}
	top["hookSpecificOutput"] = h
	return jsonOut(top)
}
