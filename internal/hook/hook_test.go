package hook

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/patakil/intagent/internal/board"
)

// Payloads below are trimmed copies of what the agents sent in live runs.

const ccPreEdit = `{"session_id":"918b46a0-37d8-4501-b2b8-1f36670f1113","transcript_path":"/root/.claude/projects/x/918b.jsonl",
"cwd":"/lab","prompt_id":"441f","permission_mode":"acceptEdits","hook_event_name":"PreToolUse","tool_name":"Edit",
"tool_input":{"file_path":"/lab/notes.txt","old_string":"beta","new_string":"BETA","replace_all":false},"tool_use_id":"toolu_01"}`

func TestClaudeCodeParse(t *testing.T) {
	cc := ClaudeCode{}
	tests := []struct {
		name, payload string
		want          Event
	}{
		{"pre edit", ccPreEdit, Event{Kind: board.KindPreEdit, Name: "PreToolUse", SessionID: "918b46a0-37d8-4501-b2b8-1f36670f1113", Cwd: "/lab", Tool: "Edit", Paths: []string{"/lab/notes.txt"}}},
		{"notebook", `{"session_id":"s","cwd":"/lab","hook_event_name":"PostToolUse","tool_name":"NotebookEdit","tool_input":{"notebook_path":"/lab/a.ipynb","new_source":"x"}}`,
			Event{Kind: board.KindPostEdit, Name: "PostToolUse", SessionID: "s", Cwd: "/lab", Tool: "NotebookEdit", Paths: []string{"/lab/a.ipynb"}}},
		{"write", `{"session_id":"s","cwd":"/lab","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/lab/new.go","content":"x"}}`,
			Event{Kind: board.KindPreEdit, Name: "PreToolUse", SessionID: "s", Cwd: "/lab", Tool: "Write", Paths: []string{"/lab/new.go"}}},
		{"bash start", `{"session_id":"s","cwd":"/lab","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`,
			Event{Kind: board.KindToolStart, Name: "PreToolUse", SessionID: "s", Cwd: "/lab", Tool: "Bash"}},
		{"bash end", `{"session_id":"s","cwd":"/lab","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"sed -i s/a/b/ x"}}`,
			Event{Kind: board.KindToolEnd, Name: "PostToolUse", SessionID: "s", Cwd: "/lab", Tool: "Bash", Footprint: true}},
		{"prompt", `{"session_id":"s","cwd":"/lab","hook_event_name":"UserPromptSubmit","prompt":"Fix the retry bug"}`,
			Event{Kind: board.KindPrompt, Name: "UserPromptSubmit", SessionID: "s", Cwd: "/lab", Prompt: "Fix the retry bug"}},
		{"task notification", `{"session_id":"s","cwd":"/lab","hook_event_name":"UserPromptSubmit","prompt":"<task-notification>\n<task-id>x</task-id>"}`,
			Event{Kind: board.KindPrompt, Name: "UserPromptSubmit", SessionID: "s", Cwd: "/lab"}},
		{"start", `{"session_id":"s","cwd":"/lab","hook_event_name":"SessionStart","source":"startup"}`,
			Event{Kind: board.KindSessionStart, Name: "SessionStart", SessionID: "s", Cwd: "/lab", Footprint: true}},
		{"stop", `{"session_id":"s","cwd":"/lab","hook_event_name":"Stop","stop_hook_active":false}`,
			Event{Kind: board.KindStop, Name: "Stop", SessionID: "s", Cwd: "/lab", Footprint: true}},
		{"end", `{"session_id":"s","cwd":"/lab","hook_event_name":"SessionEnd","reason":"other"}`,
			Event{Kind: board.KindSessionEnd, Name: "SessionEnd", SessionID: "s", Cwd: "/lab", Footprint: true}},
		{"notification", `{"session_id":"s","cwd":"/lab","hook_event_name":"Notification"}`,
			Event{Skip: true, Name: "Notification", SessionID: "s", Cwd: "/lab"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cc.Parse([]byte(tt.payload))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
	if _, err := cc.Parse([]byte(`{"hook_event_name":"PreToolUse","tool_name":"Edit"}`)); err == nil {
		t.Error("accepted a payload without session_id")
	}
	if _, err := cc.Parse([]byte(`not json`)); err == nil {
		t.Error("accepted garbage")
	}
}

type spec struct {
	HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
}

func decodeOut(t *testing.T, o Output) map[string]string {
	t.Helper()
	if len(o.Stdout) == 0 {
		return nil
	}
	var s spec
	dec := json.NewDecoder(strings.NewReader(string(o.Stdout)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("output %s: %v", o.Stdout, err)
	}
	return s.HookSpecificOutput
}

func TestClaudeCodeRender(t *testing.T) {
	cc := ClaudeCode{}
	pre := Event{Name: "PreToolUse"}
	deny := decodeOut(t, cc.Render(pre, board.HookResult{Decision: board.Refuse, Reason: "held by alice"}))
	if deny["hookEventName"] != "PreToolUse" || deny["permissionDecision"] != "deny" || deny["permissionDecisionReason"] != "held by alice" {
		t.Fatalf("deny = %v", deny)
	}
	if ask := decodeOut(t, cc.Render(pre, board.HookResult{Decision: board.DecideAsk, Reason: "r"})); ask["permissionDecision"] != "ask" {
		t.Fatalf("ask = %v", ask)
	}
	warn := decodeOut(t, cc.Render(pre, board.HookResult{Decision: board.Allow, Context: "heads-up"}))
	if warn["additionalContext"] != "heads-up" || warn["permissionDecision"] != "" {
		t.Fatalf("warn = %v", warn)
	}
	if out := cc.Render(pre, board.HookResult{Decision: board.Allow}); len(out.Stdout) != 0 || out.Exit != 0 {
		t.Fatalf("plain allow printed %q", out.Stdout)
	}
	for _, name := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		got := decodeOut(t, cc.Render(Event{Name: name}, board.HookResult{Context: "news"}))
		if got["hookEventName"] != name || got["additionalContext"] != "news" {
			t.Errorf("%s = %v", name, got)
		}
	}
	for _, name := range []string{"Stop", "SessionEnd"} {
		if out := cc.Render(Event{Name: name}, board.HookResult{Context: "x"}); len(out.Stdout) != 0 {
			t.Errorf("%s printed %q", name, out.Stdout)
		}
	}
}

func TestCodexParseAndPatchPaths(t *testing.T) {
	cx := Codex{}
	patch := "*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** Update File: src/a.go\n*** Move to: src/b.go\n@@\n-x\n+y\n*** Delete File:  old/c.go \n*** End Patch\n"
	payload, _ := json.Marshal(map[string]any{
		"session_id": "01a0", "turn_id": "t", "cwd": "/proj", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_input": map[string]string{"command": patch}, "tool_use_id": "exec-1",
	})
	ev, err := cx.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != board.KindPreEdit || strings.Join(ev.Paths, ",") != "hello.txt,src/a.go,src/b.go,old/c.go" {
		t.Fatalf("apply_patch event = %+v", ev)
	}

	heredoc := "cd services && apply_patch <<'EOF'\n" + patch + "EOF"
	payload, _ = json.Marshal(map[string]any{"session_id": "01a0", "cwd": "/proj", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": heredoc}})
	ev, _ = cx.Parse(payload)
	if ev.Kind != board.KindPreEdit || ev.Paths[0] != "services/hello.txt" || ev.Paths[3] != "services/old/c.go" {
		t.Fatalf("heredoc event = %+v", ev)
	}

	payload, _ = json.Marshal(map[string]any{"session_id": "01a0", "cwd": "/proj", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": "make gen"}, "tool_response": "done\n"})
	ev, _ = cx.Parse(payload)
	if ev.Kind != board.KindToolEnd || !ev.Footprint {
		t.Fatalf("bash post = %+v", ev)
	}
	for name, kind := range map[string]board.Kind{"SessionStart": board.KindSessionStart, "UserPromptSubmit": board.KindPrompt, "Stop": board.KindStop, "SessionEnd": board.KindSessionEnd} {
		payload, _ = json.Marshal(map[string]any{"session_id": "s", "cwd": "/p", "hook_event_name": name, "prompt": "do it"})
		if ev, _ := cx.Parse(payload); ev.Kind != kind {
			t.Errorf("%s → %s", name, ev.Kind)
		}
	}
	if ev, _ := cx.Parse([]byte(`{"session_id":"s","hook_event_name":"PreCompact"}`)); !ev.Skip {
		t.Error("PreCompact not skipped")
	}
}

func TestCodexRenderIsStrict(t *testing.T) {
	cx := Codex{}
	pre := Event{Name: "PreToolUse"}
	deny := decodeOut(t, cx.Render(pre, board.HookResult{Decision: board.Refuse, Reason: "r"}))
	if deny["permissionDecision"] != "deny" {
		t.Fatalf("deny = %v", deny)
	}
	// Codex has no ask; it must become a deny that tells the agent to ask.
	ask := decodeOut(t, cx.Render(pre, board.HookResult{Decision: board.DecideAsk, Reason: "r"}))
	if ask["permissionDecision"] != "deny" || !strings.Contains(ask["permissionDecisionReason"], "Ask your user") {
		t.Fatalf("ask = %v", ask)
	}
	// An allow is never spelled out: Codex treats an explicit allow as invalid.
	if out := cx.Render(pre, board.HookResult{Decision: board.Allow}); len(out.Stdout) != 0 {
		t.Fatalf("allow printed %q", out.Stdout)
	}
	ctx := decodeOut(t, cx.Render(pre, board.HookResult{Decision: board.Allow, Context: "c"}))
	if len(ctx) != 2 || ctx["additionalContext"] != "c" {
		t.Fatalf("context = %v", ctx)
	}
	if out := cx.Render(Event{Name: "SessionEnd"}, board.HookResult{Context: "x"}); len(out.Stdout) != 0 {
		t.Fatal("SessionEnd printed output")
	}
}

func TestCursorParse(t *testing.T) {
	cu := Cursor{}
	ev, err := cu.Parse([]byte(`{"conversation_id":"c1","generation_id":"g","hook_event_name":"afterFileEdit","workspace_roots":["/ws"],"file_path":"/ws/a.ts","edits":[]}`))
	if err != nil || ev.Kind != board.KindPostEdit || ev.Cwd != "/ws" || ev.Paths[0] != "/ws/a.ts" {
		t.Fatalf("afterFileEdit = %+v, %v", ev, err)
	}
	if ev, _ := cu.Parse([]byte(`{"conversation_id":"c1","hook_event_name":"beforeReadFile"}`)); !ev.Skip {
		t.Fatal("beforeReadFile not skipped")
	}
	out := cu.Render(Event{Name: "beforeShellExecution"}, board.HookResult{})
	if !strings.Contains(string(out.Stdout), `"permission":"allow"`) {
		t.Fatalf("shell permission = %s", out.Stdout)
	}
}

func TestFor(t *testing.T) {
	for _, n := range []string{"claude-code", "claude", "codex", "cursor"} {
		if _, err := For(n); err != nil {
			t.Errorf("For(%q): %v", n, err)
		}
	}
	if _, err := For("gemini"); err == nil {
		t.Error("For accepted an unknown agent")
	}
}
