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
		{"pre edit", ccPreEdit, Event{Kind: board.KindPreEdit, Name: "PreToolUse", SessionID: "918b46a0-37d8-4501-b2b8-1f36670f1113", Cwd: "/lab", Tool: "Edit", ToolUseID: "toolu_01", Paths: []string{"/lab/notes.txt"}}},
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
	deny := decodeOut(t, cc.Render(pre, board.HookResult{Decision: board.DecisionRefuse, Reason: "held by alice"}))
	if deny["hookEventName"] != "PreToolUse" || deny["permissionDecision"] != "deny" || deny["permissionDecisionReason"] != "held by alice" {
		t.Fatalf("deny = %v", deny)
	}
	if ask := decodeOut(t, cc.Render(pre, board.HookResult{Decision: board.DecisionAsk, Reason: "r"})); ask["permissionDecision"] != "ask" {
		t.Fatalf("ask = %v", ask)
	}
	warn := decodeOut(t, cc.Render(pre, board.HookResult{Decision: board.DecisionAllow, Context: "heads-up"}))
	if warn["additionalContext"] != "heads-up" || warn["permissionDecision"] != "" {
		t.Fatalf("warn = %v", warn)
	}
	if out := cc.Render(pre, board.HookResult{Decision: board.DecisionAllow}); len(out.Stdout) != 0 || out.Exit != 0 {
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
	if ev.Kind != board.KindPreEdit || !ev.NoAsk || strings.Join(ev.Paths, ",") != "hello.txt,src/a.go,src/b.go,old/c.go" {
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
	deny := decodeOut(t, cx.Render(pre, board.HookResult{Decision: board.DecisionRefuse, Reason: "r"}))
	if deny["permissionDecision"] != "deny" {
		t.Fatalf("deny = %v", deny)
	}
	// Codex has no ask; it must become a deny that tells the agent to ask.
	ask := decodeOut(t, cx.Render(pre, board.HookResult{Decision: board.DecisionAsk, Reason: "r"}))
	if ask["permissionDecision"] != "deny" || !strings.Contains(ask["permissionDecisionReason"], "Ask your user") {
		t.Fatalf("ask = %v", ask)
	}
	// An allow is never spelled out: Codex treats an explicit allow as invalid.
	if out := cx.Render(pre, board.HookResult{Decision: board.DecisionAllow}); len(out.Stdout) != 0 {
		t.Fatalf("allow printed %q", out.Stdout)
	}
	ctx := decodeOut(t, cx.Render(pre, board.HookResult{Decision: board.DecisionAllow, Context: "c"}))
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

// cuPre is a Cursor preToolUse payload as the SDK's runtime sent it for an edit.
const cuPre = `{"conversation_id":"c1","generation_id":"g1","model":"m","session_id":"c1","hook_event_name":"preToolUse",
"cursor_version":"1.0.35","workspace_roots":["/ws"],"user_email":null,"transcript_path":null,
"tool_name":"Write","tool_input":{"file_path":"/ws/src/a.ts","content":"x"},"tool_use_id":"t1","cwd":"/ws"}`

func TestCursorEditsAreCheckedBeforeTheWrite(t *testing.T) {
	cu := Cursor{}
	ev, err := cu.Parse([]byte(cuPre))
	want := Event{Kind: board.KindPreEdit, Name: "preToolUse", SessionID: "c1", Cwd: "/ws", Tool: "Write", ToolUseID: "t1", Paths: []string{"/ws/src/a.ts"}, NoAsk: true}
	if err != nil || !reflect.DeepEqual(ev, want) {
		t.Fatalf("preToolUse Write = %+v, %v", ev, err)
	}
	tests := []struct {
		name, payload string
		kind          board.Kind
		paths         []string
		footprint     bool
	}{
		{"delete", `{"conversation_id":"c1","hook_event_name":"postToolUse","tool_name":"Delete","tool_input":{"file_path":"/ws/old.ts"}}`, board.KindPostEdit, []string{"/ws/old.ts"}, false},
		{"shell before", `{"conversation_id":"c1","hook_event_name":"preToolUse","tool_name":"Shell","tool_input":{"command":"make"}}`, board.KindToolStart, nil, false},
		{"shell after", `{"conversation_id":"c1","hook_event_name":"postToolUse","tool_name":"Shell","tool_input":{"command":"make"}}`, board.KindToolEnd, nil, true},
		{"read", `{"conversation_id":"c1","hook_event_name":"preToolUse","tool_name":"Read","tool_input":{"file_path":"/ws/a.ts"}}`, board.KindToolStart, nil, false},
		{"session", `{"session_id":"s9","hook_event_name":"sessionStart","workspace_roots":["/ws"]}`, board.KindSessionStart, nil, true},
		{"end", `{"conversation_id":"c1","hook_event_name":"sessionEnd","reason":"user_close"}`, board.KindSessionEnd, nil, true},
	}
	for _, tt := range tests {
		ev, err := cu.Parse([]byte(tt.payload))
		if err != nil || ev.Kind != tt.kind || !reflect.DeepEqual(ev.Paths, tt.paths) || ev.Footprint != tt.footprint {
			t.Errorf("%s: %+v, %v", tt.name, ev, err)
		}
	}
	if _, err := cu.Parse([]byte(`{"hook_event_name":"stop"}`)); err == nil {
		t.Error("a payload without a conversation was accepted")
	}
}

func TestCursorRender(t *testing.T) {
	cu := Cursor{}
	pre := Event{Name: "preToolUse"}
	render := func(ev Event, res board.HookResult) map[string]any {
		t.Helper()
		var m map[string]any
		out := cu.Render(ev, res)
		if err := json.Unmarshal(out.Stdout, &m); err != nil || out.Exit != 0 {
			t.Fatalf("%s: not valid JSON or exit %d: %q", ev.Name, out.Exit, out.Stdout)
		}
		return m
	}
	m := render(pre, board.HookResult{Decision: board.DecisionRefuse, Reason: "alice's agent holds it", Context: "ctx"})
	if m["permission"] != "deny" || m["user_message"] != "alice's agent holds it" || m["agent_message"] != "alice's agent holds it" || m["additional_context"] != "ctx" {
		t.Errorf("refuse = %v", m)
	}
	m = render(pre, board.HookResult{Decision: board.DecisionAsk, Reason: "r"})
	if m["permission"] != "deny" || !strings.Contains(m["agent_message"].(string), "Ask your user") {
		t.Errorf("ask = %v", m)
	}
	if m = render(pre, board.HookResult{Decision: board.DecisionAllow}); m["permission"] != "allow" || len(m) != 1 {
		t.Errorf("allow = %v", m)
	}
	if m = render(Event{Name: "beforeSubmitPrompt"}, board.HookResult{Context: "c"}); m["continue"] != true || m["additional_context"] != "c" {
		t.Errorf("prompt = %v", m)
	}
	for _, name := range []string{"sessionStart", "postToolUse", "postToolUseFailure"} {
		if m = render(Event{Name: name}, board.HookResult{Context: "c"}); m["additional_context"] != "c" {
			t.Errorf("%s = %v", name, m)
		}
	}
	// Steps that take no answer still get valid JSON.
	if m = render(Event{Name: "stop"}, board.HookResult{Context: "c"}); len(m) != 0 {
		t.Errorf("stop = %v", m)
	}
}

func TestCopilotUsesClaudeHooksWithItsOwnArguments(t *testing.T) {
	cp := ClaudeCode{Copilot: true}
	if cp.Agent() != board.AgentCopilot {
		t.Fatal(cp.Agent())
	}
	ev, err := cp.Parse([]byte(`{"session_id":"p1","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"path":"/w/a.go","file_text":"x"}}`))
	if err != nil || ev.Kind != board.KindPreEdit || !reflect.DeepEqual(ev.Paths, []string{"/w/a.go"}) {
		t.Fatalf("parse = %+v, %v", ev, err)
	}
	var m struct {
		PermissionDecision string       `json:"permissionDecision"`
		Reason             string       `json:"permissionDecisionReason"`
		H                  hookSpecific `json:"hookSpecificOutput"`
	}
	out := cp.Render(ev, board.HookResult{Decision: board.DecisionRefuse, Reason: "held"})
	if err := json.Unmarshal(out.Stdout, &m); err != nil || m.PermissionDecision != "deny" || m.Reason != "held" ||
		m.H.PermissionDecision != "deny" || m.H.HookEventName != "PreToolUse" || out.Exit != 0 {
		t.Fatalf("deny = %q", out.Stdout)
	}
	if out := cp.Render(ev, board.HookResult{Decision: board.DecisionAllow}); len(out.Stdout) != 0 {
		t.Fatalf("silent allow printed %q", out.Stdout)
	}
	out = cp.Render(Event{Name: "SessionStart"}, board.HookResult{Context: "board"})
	if !strings.Contains(string(out.Stdout), `"additionalContext":"board"`) || strings.Contains(string(out.Stdout), "permissionDecision") {
		t.Fatalf("context = %q", out.Stdout)
	}
}

// gmPre is a Gemini CLI BeforeTool payload as Gemini CLI 0.62.0 sent it.
const gmPre = `{"session_id":"52096186-aa","transcript_path":"/h/.gemini/tmp/proj/chats/session.jsonl","cwd":"/abs/proj",
"hook_event_name":"BeforeTool","timestamp":"2026-10-02T22:46:40.788Z","tool_name":"write_file",
"tool_input":{"file_path":"/abs/proj/claimed.go","content":"package claimed\n"}}`

func TestGemini(t *testing.T) {
	gm := Gemini{}
	ev, err := gm.Parse([]byte(gmPre))
	want := Event{Kind: board.KindPreEdit, Name: "BeforeTool", SessionID: "52096186-aa", Cwd: "/abs/proj", Tool: "write_file",
		Paths: []string{"/abs/proj/claimed.go"}, LateContext: true, NoAsk: true}
	if err != nil || !reflect.DeepEqual(ev, want) {
		t.Fatalf("BeforeTool = %+v, %v", ev, err)
	}
	tests := []struct {
		name, payload string
		kind          board.Kind
		paths         []string
		footprint     bool
	}{
		{"replace after", `{"session_id":"g","cwd":"/p","hook_event_name":"AfterTool","tool_name":"replace","tool_input":{"file_path":"/p/a.go","old_string":"a","new_string":"b"}}`, board.KindPostEdit, []string{"/p/a.go"}, false},
		{"shell before", `{"session_id":"g","cwd":"/p","hook_event_name":"BeforeTool","tool_name":"run_shell_command","tool_input":{"command":"make"}}`, board.KindToolStart, nil, false},
		{"shell after", `{"session_id":"g","cwd":"/p","hook_event_name":"AfterTool","tool_name":"run_shell_command","tool_input":{"command":"make"}}`, board.KindToolEnd, nil, true},
		{"read", `{"session_id":"g","cwd":"/p","hook_event_name":"BeforeTool","tool_name":"read_file","tool_input":{"file_path":"/p/a.go"}}`, board.KindToolStart, nil, false},
		{"prompt", `{"session_id":"g","cwd":"/p","hook_event_name":"BeforeAgent","prompt":"fix it"}`, board.KindPrompt, nil, false},
		{"turn ends", `{"session_id":"g","cwd":"/p","hook_event_name":"AfterAgent","prompt":"fix it","prompt_response":"done"}`, board.KindStop, nil, true},
		{"start", `{"session_id":"g","cwd":"/p","hook_event_name":"SessionStart","source":"startup"}`, board.KindSessionStart, nil, true},
		{"end", `{"session_id":"g","cwd":"/p","hook_event_name":"SessionEnd","reason":"exit"}`, board.KindSessionEnd, nil, true},
	}
	for _, tt := range tests {
		ev, err := gm.Parse([]byte(tt.payload))
		if err != nil || ev.Kind != tt.kind || !reflect.DeepEqual(ev.Paths, tt.paths) || ev.Footprint != tt.footprint || ev.LateContext {
			t.Errorf("%s: %+v, %v", tt.name, ev, err)
		}
	}
	if ev, _ := gm.Parse([]byte(`{"session_id":"g","hook_event_name":"PreCompress","trigger":"auto"}`)); !ev.Skip {
		t.Error("PreCompress not skipped")
	}

	pre := Event{Name: "BeforeTool"}
	var m map[string]string
	out := gm.Render(pre, board.HookResult{Decision: board.DecisionRefuse, Reason: "held"})
	if err := json.Unmarshal(out.Stdout, &m); err != nil || m["decision"] != "deny" || m["reason"] != "held" || len(m) != 2 {
		t.Fatalf("refuse = %q", out.Stdout)
	}
	out = gm.Render(pre, board.HookResult{Decision: board.DecisionAsk, Reason: "r"})
	if !strings.Contains(string(out.Stdout), `"decision":"deny"`) || !strings.Contains(string(out.Stdout), "Ask your user") {
		t.Fatalf("ask = %q", out.Stdout)
	}
	if out := gm.Render(pre, board.HookResult{Decision: board.DecisionAllow, Context: "late"}); len(out.Stdout) != 0 {
		t.Fatalf("allow printed %q", out.Stdout)
	}
	for _, name := range []string{"SessionStart", "BeforeAgent", "AfterTool"} {
		var o struct {
			H hookSpecific `json:"hookSpecificOutput"`
		}
		out := gm.Render(Event{Name: name}, board.HookResult{Context: "news"})
		if err := json.Unmarshal(out.Stdout, &o); err != nil || o.H.AdditionalContext != "news" || o.H.HookEventName != name {
			t.Errorf("%s = %q", name, out.Stdout)
		}
	}
	if out := gm.Render(Event{Name: "AfterAgent"}, board.HookResult{Context: "news"}); len(out.Stdout) != 0 {
		t.Errorf("AfterAgent printed %q", out.Stdout)
	}
}

// With GPT and codex models Copilot edits only through apply_patch, which
// reaches the Claude-style hook as tool "Edit" with the patch as a bare string.
func TestCopilotApplyPatch(t *testing.T) {
	cp := ClaudeCode{Copilot: true}
	payload := `{"hook_event_name":"PreToolUse","session_id":"p1","cwd":"/w","tool_name":"Edit",
"tool_input":"*** Begin Patch\n*** Update File: services/payments/retry.go\n@@\n-package payments\n+package payments // patched\n` +
		`*** Add File: services/payments/jitter.go\n+package payments\n*** End Patch\n"}`
	ev, err := cp.Parse([]byte(payload))
	want := []string{"services/payments/retry.go", "services/payments/jitter.go"}
	if err != nil || ev.Kind != board.KindPreEdit || !reflect.DeepEqual(ev.Paths, want) {
		t.Fatalf("parse = %+v, %v", ev, err)
	}
	ev, err = cp.Parse([]byte(strings.Replace(payload, `"PreToolUse"`, `"PostToolUse"`, 1)))
	if err != nil || ev.Kind != board.KindPostEdit || !reflect.DeepEqual(ev.Paths, want) {
		t.Fatalf("post = %+v, %v", ev, err)
	}
	ev, err = cp.Parse([]byte(strings.Replace(payload, `"tool_name":"Edit"`, `"tool_name":"apply_patch"`, 1)))
	if err != nil || ev.Kind != board.KindPreEdit || len(ev.Paths) != 2 {
		t.Fatalf("as apply_patch = %+v, %v", ev, err)
	}
}

func TestDetect(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	tests := []struct {
		name, arg, payload string
		getenv             func(string) string
		want               Adapter
	}{
		{"claude, unnamed", "", ccPreEdit, env(), ClaudeCode{}},
		{"claude, named", "claude-code", ccPreEdit, env(), ClaudeCode{}},
		// Cursor runs the hooks it imports from .claude/settings.json with its own payloads.
		{"cursor through the claude hook", "", cuPre, env(), Cursor{}},
		{"cursor through the old claude wiring", "claude-code", cuPre, env("CLAUDE_PROJECT_DIR", "/ws"), Cursor{}},
		{"cursor without a version", "", `{"conversation_id":"c","hook_event_name":"stop"}`, env(), Cursor{}},
		{"copilot", "", `{"session_id":"p","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"path":"/a"}}`, env("COPILOT_CLI", "1"), ClaudeCode{Copilot: true}},
		{"codex", "codex", `{"session_id":"x","hook_event_name":"PreToolUse","tool_name":"apply_patch"}`, env("COPILOT_CLI", "1"), Codex{}},
		{"garbage keeps the named adapter", "codex", `{`, env(), Codex{}},
	}
	for _, tt := range tests {
		got, err := Detect(tt.arg, []byte(tt.payload), tt.getenv)
		if err != nil || got != tt.want {
			t.Errorf("%s: %#v, %v", tt.name, got, err)
		}
	}
	if _, err := Detect("aider", []byte(ccPreEdit), env()); err == nil {
		t.Error("Detect accepted an unknown agent")
	}
}

func TestFor(t *testing.T) {
	for _, n := range []string{"", "claude-code", "claude", "codex", "cursor", "copilot", "gemini"} {
		if _, err := For(n); err != nil {
			t.Errorf("For(%q): %v", n, err)
		}
	}
	if _, err := For("aider"); err == nil {
		t.Error("For accepted an unknown agent")
	}
}
