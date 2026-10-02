package hook

import (
	"encoding/json"
	"fmt"

	"github.com/patakil/intagent/internal/board"
)

// Gemini speaks Gemini CLI's hook protocol (verified against v0.62.0). Its
// payloads look like Claude Code's, but its answers differ: a refusal is
// {"decision":"deny","reason":R} (the nested Claude form is ignored), there is
// no ask, and context returned before a tool runs is dropped, so a pre-edit's
// warnings are held by the board and arrive with the edit's AfterTool.
type Gemini struct{}

// Agent names the agent.
func (Gemini) Agent() board.Agent { return board.AgentGemini }

// geminiEditTools are Gemini CLI's file-writing tools; both carry file_path.
var geminiEditTools = map[string]bool{"write_file": true, "replace": true}

// Parse reads a Gemini CLI hook payload.
func (Gemini) Parse(stdin []byte) (Event, error) {
	var in claudeInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Event{}, fmt.Errorf("gemini hook payload: %w", err)
	}
	ev := Event{Name: in.HookEventName, SessionID: in.SessionID, Cwd: in.Cwd, Tool: in.ToolName}
	path := ""
	if geminiEditTools[in.ToolName] {
		path = stringField(in.ToolInput, "file_path")
	}
	switch in.HookEventName {
	case "SessionStart":
		ev.Kind, ev.Footprint = board.KindSessionStart, true
	case "BeforeAgent":
		ev.Kind, ev.Prompt = board.KindPrompt, in.Prompt
	case "BeforeTool":
		ev.Kind = board.KindToolStart
		if path != "" {
			ev.Kind, ev.Paths, ev.LateContext = board.KindPreEdit, []string{path}, true
		}
	case "AfterTool":
		ev.Kind = board.KindToolEnd
		switch {
		case path != "":
			ev.Kind, ev.Paths = board.KindPostEdit, []string{path}
		case in.ToolName == "run_shell_command":
			ev.Footprint = true
		}
	case "AfterAgent":
		ev.Kind, ev.Footprint = board.KindStop, true
	case "SessionEnd":
		ev.Kind, ev.Footprint = board.KindSessionEnd, true
	default:
		ev.Skip = true
	}
	if !ev.Skip && ev.SessionID == "" {
		return Event{}, fmt.Errorf("gemini hook payload has no session_id")
	}
	return ev, nil
}

// Render answers in Gemini CLI's format. Allowing is printing nothing.
func (Gemini) Render(ev Event, res board.HookResult) Output {
	switch ev.Name {
	case "BeforeTool":
		if res.Decision == board.Refuse || res.Decision == board.DecideAsk {
			reason := res.Reason
			if res.Decision == board.DecideAsk {
				reason += "\nAsk your user whether to go ahead before retrying."
			}
			return jsonOut(map[string]string{"decision": "deny", "reason": reason})
		}
	case "SessionStart", "BeforeAgent", "AfterTool":
		if res.Context != "" {
			return specific(hookSpecific{HookEventName: ev.Name, AdditionalContext: res.Context})
		}
	}
	return Output{}
}
