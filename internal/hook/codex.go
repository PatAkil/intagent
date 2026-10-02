package hook

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/patakil/intagent/internal/board"
)

// Codex speaks the OpenAI Codex CLI's hook protocol (verified against 0.160.0).
// Codex rejects unknown keys and the decisions "allow" and "ask", so the
// adapter only ever emits a deny, additional context, or nothing.
type Codex struct{}

// Agent names the agent.
func (Codex) Agent() board.Agent { return board.AgentCodex }

type codexInput struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	Prompt        string          `json:"prompt"`
}

// Parse reads a Codex hook payload.
func (Codex) Parse(stdin []byte) (Event, error) {
	var in codexInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Event{}, fmt.Errorf("codex hook payload: %w", err)
	}
	ev := Event{Name: in.HookEventName, SessionID: in.SessionID, Cwd: in.Cwd, Tool: in.ToolName}
	command := stringField(in.ToolInput, "command")
	switch in.HookEventName {
	case "SessionStart":
		ev.Kind, ev.Footprint = board.KindSessionStart, true
	case "UserPromptSubmit":
		ev.Kind, ev.Prompt = board.KindPrompt, in.Prompt
	case "PreToolUse":
		ev.Kind = board.KindToolStart
		if paths := patchPaths(in.ToolName, command); len(paths) > 0 {
			ev.Kind, ev.Paths = board.KindPreEdit, paths
		}
	case "PostToolUse":
		ev.Kind = board.KindToolEnd
		if paths := patchPaths(in.ToolName, command); len(paths) > 0 {
			ev.Kind, ev.Paths = board.KindPostEdit, paths
		} else if in.ToolName == "Bash" {
			ev.Footprint = true
		}
	case "SubagentStop":
		ev.Kind = board.KindToolEnd
	case "Stop":
		ev.Kind, ev.Footprint = board.KindStop, true
	case "SessionEnd":
		ev.Kind, ev.Footprint = board.KindSessionEnd, true
	default:
		ev.Skip = true
	}
	if !ev.Skip && ev.SessionID == "" {
		return Event{}, fmt.Errorf("codex hook payload has no session_id")
	}
	return ev, nil
}

// Render answers in the strict JSON Codex accepts.
func (Codex) Render(ev Event, res board.HookResult) Output {
	switch ev.Name {
	case "PreToolUse":
		if res.Decision == board.Refuse || res.Decision == board.DecideAsk {
			// Codex cannot ask the user from PreToolUse; asking becomes a refusal
			// that tells the agent to ask.
			reason := res.Reason
			if res.Decision == board.DecideAsk {
				reason += "\nAsk your user whether to go ahead before retrying."
			}
			return specific(hookSpecific{HookEventName: ev.Name, PermissionDecision: "deny", PermissionDecisionReason: reason})
		}
		if res.Context != "" {
			return specific(hookSpecific{HookEventName: ev.Name, AdditionalContext: res.Context})
		}
	case "SessionStart", "UserPromptSubmit", "PostToolUse":
		if res.Context != "" {
			return specific(hookSpecific{HookEventName: ev.Name, AdditionalContext: res.Context})
		}
	}
	return Output{}
}

var cdPrefix = regexp.MustCompile(`^\s*cd\s+("[^"]+"|'[^']+'|\S+)\s*&&`)

// patchPaths returns the files an apply_patch call, or a shell command that
// pipes a patch into apply_patch, will touch. Paths are relative to the cwd,
// or to the directory a leading "cd <dir> &&" moves into.
func patchPaths(tool, command string) []string {
	base := ""
	switch tool {
	case "apply_patch":
	case "Bash":
		if !strings.Contains(command, "*** Begin Patch") || (!strings.Contains(command, "apply_patch") && !strings.Contains(command, "applypatch")) {
			return nil
		}
		if m := cdPrefix.FindStringSubmatch(command); m != nil {
			base = strings.Trim(m[1], `"'`)
		}
	default:
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(command, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{"*** Add File:", "*** Delete File:", "*** Update File:", "*** Move to:"} {
			p, ok := strings.CutPrefix(line, marker)
			if !ok {
				continue
			}
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if base != "" && !strings.HasPrefix(p, "/") {
				p = path.Join(base, p)
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
