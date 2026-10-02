package hook

import (
	"encoding/json"
	"fmt"

	"github.com/patakil/intagent/internal/board"
)

// Cursor speaks Cursor's hooks.json protocol. Cursor reports edits after they
// happen (afterFileEdit), so it cannot be stopped before a write; its agent
// still hears about others' work through the intagent MCP tools, and its edits
// reach everyone else's agents.
type Cursor struct{}

// Agent names the agent.
func (Cursor) Agent() board.Agent { return board.AgentCursor }

type cursorInput struct {
	ConversationID string   `json:"conversation_id"`
	HookEventName  string   `json:"hook_event_name"`
	WorkspaceRoots []string `json:"workspace_roots"`
	FilePath       string   `json:"file_path"`
	Prompt         string   `json:"prompt"`
	Command        string   `json:"command"`
	Cwd            string   `json:"cwd"`
}

// Parse reads a Cursor hook payload.
func (Cursor) Parse(stdin []byte) (Event, error) {
	var in cursorInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Event{}, fmt.Errorf("cursor hook payload: %w", err)
	}
	ev := Event{Name: in.HookEventName, SessionID: in.ConversationID, Cwd: in.Cwd}
	if ev.Cwd == "" && len(in.WorkspaceRoots) > 0 {
		ev.Cwd = in.WorkspaceRoots[0]
	}
	switch in.HookEventName {
	case "beforeSubmitPrompt":
		ev.Kind, ev.Prompt, ev.Footprint = board.KindPrompt, in.Prompt, true
	case "beforeShellExecution":
		ev.Kind, ev.Tool = board.KindToolStart, "shell"
	case "afterShellExecution":
		ev.Kind, ev.Tool, ev.Footprint = board.KindToolEnd, "shell", true
	case "afterFileEdit":
		ev.Kind, ev.Tool = board.KindPostEdit, "edit"
		if in.FilePath != "" {
			ev.Paths = []string{in.FilePath}
		}
	case "stop":
		ev.Kind, ev.Footprint = board.KindStop, true
	default:
		ev.Skip = true
	}
	if !ev.Skip && ev.SessionID == "" {
		return Event{}, fmt.Errorf("cursor hook payload has no conversation_id")
	}
	return ev, nil
}

// Render answers Cursor. Only the permission hooks take an answer.
func (Cursor) Render(ev Event, _ board.HookResult) Output {
	switch ev.Name {
	case "beforeSubmitPrompt":
		return jsonOut(map[string]any{"continue": true})
	case "beforeShellExecution":
		return jsonOut(map[string]any{"permission": "allow"})
	}
	return Output{}
}

func jsonOut(v any) Output {
	b, err := json.Marshal(v)
	if err != nil {
		return Output{}
	}
	return Output{Stdout: append(b, '\n')}
}
