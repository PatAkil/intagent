package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/server"
)

// team is a running server, an origin repository and one clone per member.
type team struct {
	t      *testing.T
	url    string
	srv    *server.Server
	origin string
	dir    string
	tokens map[string]string
	// hookBudget, when set, replaces the hooks' time budget.
	hookBudget time.Duration
	// now, when set, replaces the clock that paces footprint scans.
	now func() time.Time
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTeam(t *testing.T, members ...string) *team {
	t.Helper()
	return newTeamWith(t, board.DefaultConfig(), members...)
}

func newTeamWith(t *testing.T, cfg board.Config, members ...string) *team {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // keep the hook log out of the real cache
	t.Setenv("HOME", t.TempDir())           // and init --user and Codex trust out of real settings
	t.Setenv("CODEX_HOME", t.TempDir())
	bin := t.TempDir() // doctor wants intagent on PATH; a stub will do
	writeFile(t, filepath.Join(bin, "intagent"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "intagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"INTAGENT_URL", "INTAGENT_TOKEN", "INTAGENT_DISABLE", "INTAGENT_FAIL", "CLAUDE_PROJECT_DIR"} {
		t.Setenv(k, "")
	}
	tm := &team{t: t, dir: t.TempDir(), tokens: map[string]string{}}
	var ms []server.Member
	for _, m := range members {
		tok, hash, err := server.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		tm.tokens[m] = tok
		ms = append(ms, server.Member{Name: m, TokenSHA256: hash})
	}
	srv, err := server.New(server.Options{Members: ms, Board: cfg})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	tm.url, tm.srv = hs.URL, srv

	tm.origin = filepath.Join(tm.dir, "origin.git")
	gitRun(t, tm.dir, "init", "-q", "--bare", "-b", "main", tm.origin)
	seed := filepath.Join(tm.dir, "seed")
	gitRun(t, tm.dir, "clone", "-q", tm.origin, seed)
	writeFile(t, filepath.Join(seed, "svc/pay/go.mod"), "module pay\n")
	writeFile(t, filepath.Join(seed, "svc/pay/retry.go"), "package pay\n")
	writeFile(t, filepath.Join(seed, "README.md"), "hi\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "seed")
	gitRun(t, seed, "push", "-q", "origin", "main")
	return tm
}

// clone checks out the repository for a member.
func (tm *team) clone(member string) string {
	p := filepath.Join(tm.dir, member)
	gitRun(tm.t, tm.dir, "clone", "-q", tm.origin, p)
	return p
}

// as runs intagent as a member, in dir, with stdin.
func (tm *team) as(member, dir, stdin string, args ...string) (string, string, int) {
	tm.t.Helper()
	tm.t.Setenv("INTAGENT_CONFIG", filepath.Join(tm.dir, member+".json"))
	tm.t.Setenv("INTAGENT_HOST", member+"-laptop")
	var out, errb bytes.Buffer
	app := &App{In: strings.NewReader(stdin), Out: &out, Err: &errb, Version: "test", Dir: dir, HookBudget: tm.hookBudget, Now: tm.now}
	code := app.Run(context.Background(), args)
	return out.String(), errb.String(), code
}

// enrol logs every member in and enrols their clones.
func (tm *team) enrol(clones map[string]string) {
	tm.t.Helper()
	for m, dir := range clones {
		if out, errOut, code := tm.as(m, dir, "", "login", "--url", tm.url, "--token", tm.tokens[m]); code != 0 {
			tm.t.Fatalf("login %s: %s %s", m, out, errOut)
		}
		if out, errOut, code := tm.as(m, dir, "", "init", "--url", tm.url); code != 0 {
			tm.t.Fatalf("init %s: %s %s", m, out, errOut)
		}
	}
}

func claudeEvent(sessionID, cwd, event string, extra map[string]any) string {
	m := map[string]any{"session_id": sessionID, "cwd": cwd, "hook_event_name": event}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func decision(t *testing.T, out string) (string, string, string) {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		return "", "", ""
	}
	var o struct {
		H map[string]string `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("hook printed %q: %v", out, err)
	}
	return o.H["permissionDecision"], o.H["permissionDecisionReason"], o.H["additionalContext"]
}

func TestTwoMembersCollideThroughHooks(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})

	out, _, _ := tm.as("alice", a, claudeEvent("a1", a, "SessionStart", map[string]any{"source": "startup"}), "hook", "claude-code")
	if _, _, ctx := decision(t, out); !strings.Contains(ctx, "alice's agent") {
		t.Fatalf("session start context = %q", out)
	}
	tm.as("alice", a, claudeEvent("a1", a, "UserPromptSubmit", map[string]any{"prompt": "Move the retry policy"}), "hook", "claude-code")
	if out, errOut, code := tm.as("alice", a, "", "declare", "-x", "-m", "Move the retry policy", "svc/pay/**"); code != 0 {
		t.Fatalf("declare: %s %s", out, errOut)
	}
	edit := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "svc/pay/retry.go")}}
	if out, _, _ := tm.as("alice", a, claudeEvent("a1", a, "PreToolUse", edit), "hook", "claude-code"); out != "" {
		t.Fatalf("alice's own edit produced %q", out)
	}
	tm.as("alice", a, claudeEvent("a1", a, "PostToolUse", edit), "hook", "claude-code")

	bobEdit := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(b, "svc/pay/retry.go")}}
	out, _, code := tm.as("bob", b, claudeEvent("b1", b, "PreToolUse", bobEdit), "hook", "claude-code")
	dec, reason, _ := decision(t, out)
	if code != 0 || dec != "deny" || !strings.Contains(reason, "alice's agent") || !strings.Contains(reason, "Move the retry policy") {
		t.Fatalf("bob's edit: code %d decision %q reason %q", code, dec, reason)
	}

	// Codex, through an apply_patch that creates a new file under the reserved directory.
	patch := "*** Begin Patch\n*** Add File: svc/pay/jitter.go\n+package pay\n*** End Patch\n"
	cx, _ := json.Marshal(map[string]any{"session_id": "x1", "cwd": b, "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_input": map[string]string{"command": patch}})
	out, _, _ = tm.as("bob", b, string(cx), "hook", "codex")
	if dec, _, _ := decision(t, out); dec != "deny" {
		t.Fatalf("codex patch: %q", out)
	}

	// Cursor runs the Claude hook it imports, with no agent named and its own
	// payload, and must get Cursor's answer: a deny it understands.
	cu, _ := json.Marshal(map[string]any{"conversation_id": "u1", "session_id": "u1", "hook_event_name": "preToolUse", "cursor_version": "1.0.35",
		"workspace_roots": []string{b}, "tool_name": "Write", "tool_input": map[string]string{"file_path": filepath.Join(b, "svc/pay/retry.go")}})
	out, _, code = tm.as("bob", b, string(cu), "hook")
	var cur map[string]any
	if err := json.Unmarshal([]byte(out), &cur); err != nil || code != 0 || cur["permission"] != "deny" || !strings.Contains(fmt.Sprint(cur["agent_message"]), "alice's agent") {
		t.Fatalf("cursor edit: code %d %q", code, out)
	}

	// Writes outside the repository and to unrelated files pass silently.
	for _, p := range []string{"/tmp/elsewhere.txt", filepath.Join(b, "README.md")} {
		ev := map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": p}}
		if out, _, _ := tm.as("bob", b, claudeEvent("b1", b, "PreToolUse", ev), "hook", "claude-code"); out != "" {
			t.Fatalf("write to %s produced %q", p, out)
		}
	}

	// The board, check and note commands see the same picture.
	out, _, _ = tm.as("bob", b, "", "board")
	if !strings.Contains(out, "alice on main (active") || !strings.Contains(out, "exclusive intent svc/pay/**") {
		t.Fatalf("board:\n%s", out)
	}
	out, _, _ = tm.as("bob", b, "", "check", "svc/pay/retry.go")
	if !strings.Contains(out, "[block] alice's agent") {
		t.Fatalf("check:\n%s", out)
	}
	if out, errOut, code := tm.as("bob", b, "", "note", "alice", "I", "need", "retry.go", "for", "a", "hotfix"); code != 0 {
		t.Fatalf("note: %s %s", out, errOut)
	}
	out, _, _ = tm.as("alice", a, claudeEvent("a1", a, "PostToolUse", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "go test"}}), "hook", "claude-code")
	if _, _, ctx := decision(t, out); !strings.Contains(ctx, `Note from bob: "I need retry.go for a hotfix"`) {
		t.Fatalf("alice did not get the note: %q", out)
	}

	// The guard refuses bob's commit of the reserved file.
	writeFile(t, filepath.Join(b, "svc/pay/retry.go"), "package pay // hotfix\n")
	gitRun(t, b, "add", "svc/pay/retry.go")
	_, errOut, code := tm.as("bob", b, "", "guard")
	if code != 1 || !strings.Contains(errOut, "commit refused") {
		t.Fatalf("guard: code %d %s", code, errOut)
	}

	// Alice releases; bob may now commit.
	tm.as("alice", a, "", "release")
	if _, errOut, code := tm.as("bob", b, "", "guard"); code != 0 {
		t.Fatalf("guard after release: %d %s", code, errOut)
	}
}

func TestHookStaysSilentWhenItCannotHelp(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	ev := claudeEvent("a1", a, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}})

	// Not enrolled: nothing.
	if out, _, code := tm.as("alice", a, ev, "hook", "claude-code"); out != "" || code != 0 {
		t.Fatalf("not enrolled: %q %d", out, code)
	}
	tm.enrol(map[string]string{"alice": a})
	// Garbage and unknown events: nothing, exit 0.
	for _, in := range []string{"", "{", `{"hook_event_name":"Notification","session_id":"a1"}`} {
		if out, _, code := tm.as("alice", a, in, "hook", "claude-code"); out != "" || code != 0 {
			t.Fatalf("payload %q: %q %d", in, out, code)
		}
	}
	// Outside a repository: nothing.
	outside := claudeEvent("a1", t.TempDir(), "SessionStart", nil)
	if out, _, code := tm.as("alice", a, outside, "hook", "claude-code"); out != "" || code != 0 {
		t.Fatalf("outside a repo: %q %d", out, code)
	}
	// Server unreachable: fail open by default...
	t.Setenv("INTAGENT_URL", "http://127.0.0.1:1")
	longDown(t, "http://127.0.0.1:1")
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	t.Setenv("INTAGENT_TIMEOUT", "200ms")
	if out, _, code := tm.as("alice", a, ev, "hook", "claude-code"); out != "" || code != 0 {
		t.Fatalf("unreachable server: %q %d", out, code)
	}
	// ...and refuse edits when the team asked for fail-closed.
	t.Setenv("INTAGENT_FAIL", "closed")
	out, _, _ := tm.as("alice", a, ev, "hook", "claude-code")
	if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "did not answer") {
		t.Fatalf("fail closed: %q", out)
	}
	// Disabled wins over everything.
	t.Setenv("INTAGENT_DISABLE", "1")
	if out, _, _ := tm.as("alice", a, ev, "hook", "claude-code"); out != "" {
		t.Fatalf("disabled: %q", out)
	}
}

func TestInitIsIdempotentAndPreservesSettings(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	writeFile(t, filepath.Join(a, ".claude/settings.json"), `{"permissions":{"allow":["Bash(go test:*)"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"make lint"}]}]},"model":"opus"}`)
	out, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url, "--git-hook")
	if code != 0 {
		t.Fatalf("init: %s %s", out, errOut)
	}
	var s map[string]any
	data, _ := os.ReadFile(filepath.Join(a, ".claude/settings.json"))
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s["model"] != "opus" {
		t.Fatal("unrelated setting lost")
	}
	stop := s["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 2 || !strings.Contains(string(data), "make lint") {
		t.Fatalf("existing Stop hook not kept alongside intagent's: %s", data)
	}
	allow := s["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[1] != "mcp__intagent__*" {
		t.Fatalf("permissions = %v", allow)
	}
	for _, f := range []string{".intagent.json", ".mcp.json", ".codex/hooks.json", ".codex/config.toml", ".cursor/hooks.json", ".cursor/mcp.json", ".gemini/settings.json", ".git/hooks/pre-commit"} {
		if _, err := os.Stat(filepath.Join(a, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	before := snapshotFiles(t, a)
	out, _, _ = tm.as("alice", a, "", "init", "--url", tm.url, "--git-hook")
	if after := snapshotFiles(t, a); after != before {
		t.Fatalf("second init changed files:\n%s", out)
	}

	writeFile(t, filepath.Join(a, ".mcp.json"), "{oops")
	if _, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url); code == 0 || !strings.Contains(errOut, "not valid JSON") {
		t.Fatalf("init over invalid JSON: %d %s", code, errOut)
	}
}

func TestInitMigratesOldWiring(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	// What an earlier intagent wrote, next to the team's own hooks.
	writeFile(t, filepath.Join(a, ".claude/settings.json"), `{"hooks":{
"PreToolUse":[{"matcher":"Edit|Write","hooks":[{"type":"command","command":"intagent","args":["hook","claude-code"]},{"type":"command","command":"./lint.sh"}]}],
"SessionEnd":[{"hooks":[{"type":"command","command":"intagent","args":["hook","claude-code"],"timeout":5}]}],
"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/intagent hook claude-code"}]}],
"SessionStart":[{"hooks":[{"type":"command","command":"intagent hook"}]}],
"Notification":[]}}`)
	writeFile(t, filepath.Join(a, ".cursor/hooks.json"), `{"version":1,"hooks":{
"afterFileEdit":[{"command":"intagent hook cursor"}],
"stop":[{"command":"intagent hook cursor"},{"command":"./notify.sh"}],
"beforeShellExecution":[{"command":"./audit.sh"}]}}`)
	if out, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url); code != 0 {
		t.Fatalf("init: %s %s", out, errOut)
	}

	var claude struct {
		Hooks map[string][]struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	readJSON(t, filepath.Join(a, ".claude/settings.json"), &claude)
	if _, ok := claude.Hooks["Notification"]; !ok {
		t.Error("an empty event list of the team's was dropped")
	}
	ours := map[string]int{}
	var lint int
	for event, groups := range claude.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				switch h["command"] {
				case hookCommand:
					ours[event]++
					if h["args"] != nil {
						t.Errorf("%s: handler still has args: %v", event, h)
					}
				case "./lint.sh":
					lint++
				default:
					t.Errorf("%s: unexpected handler %v", event, h)
				}
			}
		}
	}
	if lint != 1 {
		t.Errorf("the team's lint hook was not kept exactly once: %d", lint)
	}

	var cursor struct {
		Version int                         `json:"version"`
		Hooks   map[string][]map[string]any `json:"hooks"`
	}
	readJSON(t, filepath.Join(a, ".cursor/hooks.json"), &cursor)
	if _, ok := cursor.Hooks["afterFileEdit"]; ok {
		t.Error("the old afterFileEdit wiring was left behind")
	}
	if len(cursor.Hooks["beforeShellExecution"]) != 1 || len(cursor.Hooks["stop"]) != 2 || cursor.Hooks["stop"][0]["command"] != "./notify.sh" {
		t.Errorf("the team's Cursor hooks were not kept: %v", cursor.Hooks)
	}
	if cursor.Hooks["preToolUse"][0]["matcher"] != "^(Write|Delete|Shell)$" {
		t.Errorf("preToolUse = %v", cursor.Hooks["preToolUse"])
	}
	// Cursor runs both files; it drops an imported Claude hook only when the
	// command is identical to its own hook's for the same event.
	imported := map[string]string{"SessionStart": "sessionStart", "UserPromptSubmit": "beforeSubmitPrompt", "PreToolUse": "preToolUse",
		"PostToolUse": "postToolUse", "Stop": "stop", "SessionEnd": "sessionEnd"}
	for ce, cu := range imported {
		if ours[ce] != 1 {
			t.Errorf("%s has %d intagent handlers, want 1", ce, ours[ce])
		}
		n := 0
		for _, h := range cursor.Hooks[cu] {
			if h["command"] == hookCommand {
				n++
			}
		}
		if n != 1 {
			t.Errorf("Cursor's %s has %d handlers matching Claude's %s", cu, n, ce)
		}
	}
	// Claude's PostToolUseFailure is not imported by Cursor, which has its own.
	// Its SubagentStop is, and Cursor runs it once: Cursor has none of its
	// own, as its subagentStop does not say which subagent stopped in the
	// terms its tool calls do (parent_tool_call_id).
	if len(ours) != len(imported)+2 || ours["PostToolUseFailure"] != 1 || len(cursor.Hooks["postToolUseFailure"]) != 1 ||
		ours["SubagentStop"] != 1 || len(cursor.Hooks["subagentStop"]) != 0 {
		t.Errorf("intagent handlers on %v", ours)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, data)
	}
}

func snapshotFiles(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range []string{".intagent.json", ".claude/settings.json", ".mcp.json", ".codex/hooks.json", ".codex/config.toml", ".cursor/hooks.json", ".cursor/mcp.json", ".gemini/settings.json"} {
		data, _ := os.ReadFile(filepath.Join(root, f))
		b.WriteString(f + "\n" + string(data))
	}
	return b.String()
}

func TestCodexTrustMatchesCodexHashes(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if out, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url, "--agents", "codex", "--trust-codex"); code != 0 {
		t.Fatalf("init: %s %s", out, errOut)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// Reference values from Codex's own hook-trust recipe (codex-rs discovery.rs),
	// computed independently for intagent's wiring.
	want := map[string]string{
		"pre_tool_use:0:0":       "sha256:28c54e9c1f2b4a5a86ba8fa2b5dcc15e251e51cd66a0c0693ec28e94ba14d334",
		"post_tool_use:0:0":      "sha256:6273974240a6a060c1e5cdcc4eca9272e47e631ce6bd1ba428ee52483946a29e",
		"session_end:0:0":        "sha256:75f93db2519531d9cbebe7755fa647b7ed79843aa7464b61967130d5acb09278",
		"session_start:0:0":      "sha256:c1f60ebbf7215586341886f25b5a244062cb377277a5534cb5d1218b4c107859",
		"stop:0:0":               "sha256:d08341e91293205d828df27ace39584c7e0005d40934fb7fd307fdccdcd16704",
		"user_prompt_submit:0:0": "sha256:7f282a978b686245fede043efb38517cb8d64acdbf31ca103b52e68199f85c7a",
	}
	text := string(cfg)
	if !strings.Contains(text, `[projects."`+a+`"]`) || !strings.Contains(text, `trust_level = "trusted"`) {
		t.Fatalf("project not trusted:\n%s", text)
	}
	for k, h := range want {
		header := `[hooks.state."` + filepath.Join(a, ".codex/hooks.json") + ":" + k + `"]`
		i := strings.Index(text, header)
		if i < 0 || !strings.Contains(text[i:], `trusted_hash = "`+h+`"`) {
			t.Errorf("missing %s = %s in:\n%s", header, h, text)
		}
	}
	// Running it again appends nothing.
	tm.as("alice", a, "", "init", "--url", tm.url, "--agents", "codex", "--trust-codex")
	again, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(again) != text {
		t.Fatal("trust entries appended twice")
	}
}

func TestMCPToolsAgainstServer(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	tm.as("bob", b, claudeEvent("b1", b, "PostToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(b, "svc/pay/retry.go")}}), "hook", "claude-code")

	// Started in a subdirectory: tool paths are still repository-relative.
	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Join(a, "svc"))
	calls := []string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"declare_intent","arguments":{"summary":"Retry rewrite","paths":["svc/pay/**"],"mode":"shared"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"check_paths","arguments":{"paths":["svc/pay/retry.go","` + filepath.Join(a, "svc/pay/go.mod") + `"]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"team_board","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"send_note","arguments":{"to":"bob","text":"heads up"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"release_intent","arguments":{}}}`,
	}
	out, errOut, code := tm.as("alice", a, strings.Join(calls, "\n")+"\n", "mcp")
	if code != 0 {
		t.Fatalf("mcp: %d %s", code, errOut)
	}
	var texts []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		var r struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Result.IsError {
			t.Fatalf("tool error: %s", line)
		}
		texts = append(texts, r.Result.Content[0].Text)
	}
	checks := []string{"Declared shared intent on svc/pay/**", "[overlap] bob's agent", "bob on main", "Note queued for 1 claim", "Released 1 intent"}
	for i, want := range checks {
		if !strings.Contains(texts[i], want) {
			t.Errorf("call %d: %q does not contain %q", i+1, texts[i], want)
		}
	}
	// team_board asks for the work nearest the caller's, bounded, not the whole board.
	if !strings.Contains(texts[2], "Other agents' work in ") || strings.Contains(texts[2], "alice on") {
		t.Errorf("team_board: %q", texts[2])
	}
	if strings.Contains(texts[0], "svc/svc") || !strings.Contains(texts[1], "svc/pay/retry.go") {
		t.Errorf("paths resolved against the start directory: %q / %q", texts[0], texts[1])
	}

	// A person typing in a subdirectory means paths and globs from there, as git does.
	out, errOut, code = tm.as("alice", filepath.Join(a, "svc"), "", "declare", "-m", "Pay", "pay/**", "../README.md")
	if code != 0 || !strings.Contains(out, "svc/pay/**, README.md") {
		t.Fatalf("declare from a subdirectory: %s %s", out, errOut)
	}
}

func TestWatchReportsFootprint(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	gitRun(t, a, "add", "-A")
	gitRun(t, a, "commit", "-q", "-m", "enrol")
	gitRun(t, a, "push", "-q", "origin", "main")
	writeFile(t, filepath.Join(a, "svc/pay/new.go"), "package pay\n")
	if out, errOut, code := tm.as("alice", a, "", "watch", "--once"); code != 0 {
		t.Fatalf("watch: %s %s", out, errOut)
	}
	// The file is on the board, and no session is left looking alive.
	out, _, _ := tm.as("alice", a, "", "board")
	if !strings.Contains(out, "svc/pay/new.go") || !strings.Contains(out, "0 live sessions") {
		t.Fatalf("board after watch --once:\n%s", out)
	}
}

func TestLoginChecksTheURLAndSwitchesPromptSharing(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	if _, errOut, code := tm.as("alice", a, "", "login", "--url", "127.0.0.1:7400", "--token", "x"); code == 0 || !strings.Contains(errOut, "not a server URL") {
		t.Fatalf("login without a scheme: %d %s", code, errOut)
	}
	tm.enrol(map[string]string{"alice": a})
	if out, _, code := tm.as("alice", a, "", "login", "--share-prompts", "off"); code != 0 || !strings.Contains(out, "Prompts stay private") {
		t.Fatalf("share-prompts off: %d %s", code, out)
	}
	tm.as("alice", a, claudeEvent("a1", a, "UserPromptSubmit", map[string]any{"prompt": "Draft the layoff memo"}), "hook")
	if out, _, _ := tm.as("alice", a, "", "board"); strings.Contains(out, "layoff") {
		t.Fatalf("a private prompt reached the board:\n%s", out)
	}
}

func TestDoctorAndHelp(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	out, _, code := tm.as("alice", a, "", "doctor")
	if code != 1 || !strings.Contains(out, "not enrolled") {
		t.Fatalf("doctor before init: %d\n%s", code, out)
	}
	tm.enrol(map[string]string{"alice": a})
	out, _, code = tm.as("alice", a, "", "doctor")
	// No Codex on this machine: an aside, not something to fix.
	if code != 0 || !strings.Contains(out, "Codex: if you use it here") {
		t.Fatalf("doctor without Codex: %d\n%s", code, out)
	}
	if !strings.Contains(out, "knows you as alice") || !strings.Contains(out, "Claude Code hooks (Copilot CLI runs them too): .claude/settings.json") {
		t.Fatalf("doctor after init:\n%s", out)
	}
	for _, want := range []string{"Cursor MCP server: .cursor/mcp.json", "Gemini CLI MCP server: .gemini/settings.json", "Claude Code MCP server: .mcp.json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor after init does not check %q:\n%s", want, out)
		}
	}
	// An MCP server someone removed is missed by the agent, and by doctor.
	cursorMCP, _ := os.ReadFile(filepath.Join(a, ".cursor/mcp.json"))
	writeFile(t, filepath.Join(a, ".cursor/mcp.json"), `{"mcpServers":{"other":{"command":"intagent-not"}}}`)
	if out, _, code := tm.as("alice", a, "", "doctor"); code != 1 || !strings.Contains(out, "Cursor MCP server: .cursor/mcp.json is not wired") {
		t.Fatalf("doctor without Cursor's MCP server: %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(a, ".cursor/mcp.json"), string(cursorMCP))
	// Wiring from before the shell form breaks under Cursor; doctor says so.
	writeFile(t, filepath.Join(a, ".claude/settings.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"intagent","args":["hook","claude-code"]}]}]}}`)
	if out, _, code := tm.as("alice", a, "", "doctor"); code != 1 || !strings.Contains(out, "settings.json is not wired for this version") {
		t.Fatalf("doctor over old wiring: %d\n%s", code, out)
	}
	if out, _, code := tm.as("alice", a, "", "help"); code != 0 || !strings.Contains(out, "intagent init") {
		t.Fatalf("help: %d %s", code, out)
	}
	if _, errOut, code := tm.as("alice", a, "", "frobnicate"); code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown command: %d %s", code, errOut)
	}
	if out, _, _ := tm.as("alice", a, "", "version"); strings.TrimSpace(out) != "intagent test" {
		t.Fatalf("version: %q", out)
	}
}

func TestTokenAndLogin(t *testing.T) {
	tm := newTeam(t, "alice")
	cfg := filepath.Join(t.TempDir(), "team.json")
	out, _, code := tm.as("alice", tm.dir, "", "token", "add", "dana", "--config", cfg)
	if code != 0 || !strings.Contains(out, "ia_") {
		t.Fatalf("token add: %d %s", code, out)
	}
	if _, errOut, code := tm.as("alice", tm.dir, "", "token", "add", "dana", "--config", cfg); code == 0 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("duplicate: %d %s", code, errOut)
	}
	if out, _, code := tm.as("alice", tm.dir, "", "token", "add", "dana", "--rotate", "--config", cfg); code != 0 {
		t.Fatalf("rotate: %d %s", code, out)
	}
	if _, errOut, code := tm.as("alice", tm.dir, "", "login", "--url", tm.url, "--token", "ia_wrong"); code == 0 || !strings.Contains(errOut, "does not recognise") {
		t.Fatalf("bad token login: %d %s", code, errOut)
	}
	out, _, code = tm.as("alice", tm.dir, tm.tokens["alice"]+"\n", "login", "--url", tm.url+"/")
	if code != 0 || !strings.Contains(out, "as alice") {
		t.Fatalf("login from stdin: %d %s", code, out)
	}
	if fi, err := os.Stat(filepath.Join(tm.dir, "alice.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("user config: %v %v", err, fi)
	}
}

func TestDemoStoryCollides(t *testing.T) {
	b := board.New(board.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	simulate(ctx, b, time.Millisecond)
	v := b.View(time.Now(), demoRepo)
	if len(v.Claims) != 4 || v.Stats.Refused == 0 || v.Stats.Bumped == 0 || v.Stats.Notes == 0 {
		t.Fatalf("demo did not play its story: %d claims, stats %+v", len(v.Claims), v.Stats)
	}
}

// The hook wiring is committed, so it runs for teammates without intagent too.
func TestCommittedHooksAreHarmlessWithoutIntagent(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	run := func(path string, stdin string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(sh, args...)
		cmd.Env = []string{"PATH=" + path}
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			return string(out), exit.ExitCode()
		case err != nil:
			t.Fatal(err)
		}
		return string(out), 0
	}
	without := t.TempDir()
	if out, code := run(without, `{"hook_event_name":"PreToolUse"}`, "-c", hookCommand); out != "" || code != 0 {
		t.Fatalf("without intagent: %q, exit %d", out, code)
	}
	with := t.TempDir()
	fake := filepath.Join(with, "intagent")
	writeFile(t, fake, "#!/bin/sh\necho \"ran $*\"\nread line\necho \"$line\"\nexit ${FAKE_EXIT:-0}\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := run(with, `{"x":1}`, "-c", hookCommand); out != "ran hook\n{\"x\":1}\n" || code != 0 {
		t.Fatalf("with intagent: %q, exit %d", out, code)
	}
	// Cursor appends the payload to the command as a here-document.
	heredoc := hookCommand + " <<'CURSOR_HOOK_EOF'\n{\"x\":2}\nCURSOR_HOOK_EOF"
	if out, code := run(with, "", "-c", heredoc); out != "ran hook\n{\"x\":2}\n" || code != 0 {
		t.Fatalf("payload as a here-document: %q, exit %d", out, code)
	}
	if out, code := run(without, "", "-c", heredoc); out != "" || code != 0 {
		t.Fatalf("here-document without intagent: %q, exit %d", out, code)
	}

	hooks := t.TempDir()
	script, err := installGitHook(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := run(without, "", script); code != 0 {
		t.Fatalf("pre-commit without intagent: exit %d", code)
	}
	writeFile(t, fake, "#!/bin/sh\necho \"ran $*\"\nexit 1\n")
	if out, code := run(with, "", script); out != "ran guard\n" || code != 1 {
		t.Fatalf("pre-commit with intagent: %q, exit %d", out, code)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("stdin exploded") }

// Go exits 2 on a panic, which every agent reads as a refusal.
func TestHookRecoversFromAPanic(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	var out, errb bytes.Buffer
	app := &App{In: panicReader{}, Out: &out, Err: &errb, Version: "test", Dir: t.TempDir()}
	if code := app.Run(context.Background(), []string{"hook"}); code != 0 || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	logged, _ := os.ReadFile(filepath.Join(cache, "intagent", "hook.log"))
	if !strings.Contains(string(logged), "panic: stdin exploded") {
		t.Fatalf("hook log: %s", logged)
	}
}

func TestServeHTTPS(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "intagent test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeFile(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))

	var out, errb bytes.Buffer
	app := &App{In: strings.NewReader(""), Out: &out, Err: &errb, Version: "test", Dir: dir}
	if code := app.Run(context.Background(), []string{"token", "add", "alice", "--config", filepath.Join(dir, "team.json")}); code != 0 {
		t.Fatalf("token add: %s", errb.String())
	}
	if code := app.Run(context.Background(), []string{"serve", "--tls-cert", certFile}); code == 0 || !strings.Contains(errb.String(), "go together") {
		t.Fatalf("cert without key: %d %s", code, errb.String())
	}
	addr := startServe(t, app, "--config", filepath.Join(dir, "team.json"), "--tls-cert", certFile, "--tls-key", keyFile)
	pool := x509.NewCertPool()
	pool.AddCert(must(x509.ParseCertificate(der)))
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true},
		Timeout: 2 * time.Second}
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = hc.Get("https://" + addr + "/healthz"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// HTTP/2, so that open dashboards share one connection.
	if err != nil || resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.ProtoMajor != 2 {
		t.Fatalf("https healthz: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

// startServe runs serve on a free port until the test ends, and returns the
// address it listens on.
func startServe(t *testing.T, app *App, args ...string) string {
	t.Helper()
	return start(t, app, append([]string{"serve", "--data", ""}, args...)...)
}

// start runs a command that listens (serve or demo) on a free port until the
// test ends, and returns the address it listens on.
func start(t *testing.T, app *App, args ...string) string {
	t.Helper()
	listening := make(chan string, 1)
	app.Listening = func(addr string) { listening <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- app.Run(ctx, append(args, "--addr", "127.0.0.1:0")) }()
	select {
	case addr := <-listening:
		t.Cleanup(func() { cancel(); <-done })
		return addr
	case code := <-done:
		cancel()
		t.Fatalf("%s exited with %d before it listened", args[0], code)
		return ""
	}
}

// The demo needs no setup: its dashboard and board open without a token, say
// they are a demo, and fill with simulated agents.
func TestDemoServesItsDashboard(t *testing.T) {
	var out, errb safeBuffer
	addr := start(t, &App{In: strings.NewReader(""), Out: &out, Err: &errb, Version: "test", Dir: t.TempDir()}, "demo")
	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get("/"); code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Fatalf("dashboard: %d %.200s", code, body)
	}
	if code, body := get("/v1/whoami"); code != http.StatusOK || !strings.Contains(body, `"demo":true`) {
		t.Fatalf("whoami: %d %s", code, body)
	}
	if !strings.Contains(out.String(), "open http://"+addr+"/") {
		t.Fatalf("demo did not say where to look: %q", out.String())
	}
}

// safeBuffer is a buffer a running command writes while its test reads it.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Codex cannot ask its person before an edit: under policy "ask" the first
// attempt is refused with the question, and the retry goes through.
func TestAskThroughCodexLetsTheRetryThrough(t *testing.T) {
	cfg := board.DefaultConfig()
	cfg.Policy.Overlap = board.ActionAsk
	tm := newTeamWith(t, cfg, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	edit := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "svc/pay/retry.go")}}
	tm.as("alice", a, claudeEvent("a1", a, "UserPromptSubmit", map[string]any{"prompt": "Retry rework"}), "hook")
	tm.as("alice", a, claudeEvent("a1", a, "PostToolUse", edit), "hook")

	patch := "*** Begin Patch\n*** Update File: svc/pay/retry.go\n@@\n-package pay\n+package pay // x\n*** End Patch\n"
	cx, _ := json.Marshal(map[string]any{"session_id": "x1", "cwd": b, "hook_event_name": "PreToolUse", "tool_name": "apply_patch",
		"tool_input": map[string]string{"command": patch}})
	out, _, _ := tm.as("bob", b, string(cx), "hook", "codex")
	if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "If they agree, retry the same edit") {
		t.Fatalf("first attempt: %q", out)
	}
	if out, _, _ := tm.as("bob", b, string(cx), "hook", "codex"); out != "" {
		t.Fatalf("the retry was not let through: %q", out)
	}
}

// INTAGENT_URL names a server; it must not make hooks report a repository
// that never enrolled (a personal project, with user-level hooks installed).
func TestHooksIgnoreRepositoriesThatDidNotEnrol(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	personal := filepath.Join(tm.dir, "side-project")
	gitRun(t, tm.dir, "init", "-q", personal)
	gitRun(t, personal, "remote", "add", "origin", "git@github.com:bob/side-project.git")
	t.Setenv("INTAGENT_URL", tm.url)
	t.Setenv("INTAGENT_TOKEN", tm.tokens["bob"])
	for _, name := range []string{"SessionStart", "UserPromptSubmit"} {
		ev := claudeEvent("b1", personal, name, map[string]any{"prompt": "Draft my resignation letter"})
		if out, _, code := tm.as("bob", personal, ev, "hook"); out != "" || code != 0 {
			t.Fatalf("%s: %q %d", name, out, code)
		}
	}
	if repos := tm.srv.Board().Repos(time.Now()); len(repos) != 0 {
		t.Fatalf("a repository that did not enrol was reported: %+v", repos)
	}
	// Commands a person types still work with the override.
	if out, errOut, code := tm.as("bob", personal, "", "board"); code != 0 {
		t.Fatalf("board: %s %s", out, errOut)
	}
}

// init reads every file it would edit first: one it cannot edit leaves the
// repository untouched, instead of half wired.
func TestInitWritesNothingWhenAFileCannotBeEdited(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	writeFile(t, filepath.Join(a, ".gemini/settings.json"), "{\n  // the team's comment\n  \"theme\": \"dark\"\n}\n")
	before := snapshotFiles(t, a)
	_, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url)
	if code == 0 || !strings.Contains(errOut, "Nothing was written") || !strings.Contains(errOut, "--agents") {
		t.Fatalf("init over a commented file: %d %s", code, errOut)
	}
	if after := snapshotFiles(t, a); after != before {
		t.Fatal("init wrote files before failing")
	}
	for _, bad := range []string{"127.0.0.1:7400", "intagent.example.com", "ftp://x"} {
		if _, errOut, code := tm.as("alice", a, "", "init", "--url", bad, "--agents", "claude-code"); code == 0 || !strings.Contains(errOut, "not a server URL") {
			t.Fatalf("init --url %s: %d %s", bad, code, errOut)
		}
	}
	if _, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url, "--agents", "claude-code,aider"); code == 0 || !strings.Contains(errOut, `unknown agent "aider"`) {
		t.Fatalf("unknown agent: %d %s", code, errOut)
	}
	// Leaving Gemini out works, and doctor does not ask to wire it.
	if out, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url, "--agents", "claude-code"); code != 0 {
		t.Fatalf("init --agents claude-code: %s %s", out, errOut)
	}
	tm.as("alice", a, "", "login", "--url", tm.url, "--token", tm.tokens["alice"])
	out, _, code := tm.as("alice", a, "", "doctor")
	if code != 0 || !strings.Contains(out, "Gemini CLI: not wired in this repository") || strings.Contains(out, "FIX") {
		t.Fatalf("doctor with Claude Code only: %d\n%s", code, out)
	}
}

// --trust-codex corrects what would stop Codex (a project marked untrusted, a
// hash from an older command) and keeps everything else in the file.
func TestTrustCodexCorrectsEntriesAndDoctorChecksThem(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	cfg := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	key := filepath.Join(a, ".codex/hooks.json") + ":stop:0:0"
	writeFile(t, cfg, "model = \"gpt-6\"  # mine\n\n[projects.\""+a+"\"]\ntrust_level = \"untrusted\"\n\n[hooks.state.\""+key+"\"]\ntrusted_hash = \"sha256:old\"\n")
	out, _, code := tm.as("alice", a, "", "doctor")
	if code != 1 || !strings.Contains(out, "Codex ignores intagent's hooks here until you run 'intagent init --trust-codex'") || !strings.Contains(out, "of intagent's 7 hooks") {
		t.Fatalf("doctor before trusting: %d\n%s", code, out)
	}
	out, errOut, code := tm.as("alice", a, "", "init", "--trust-codex")
	if code != 0 || !strings.Contains(out, `it had trust_level = "untrusted"`) || !strings.Contains(out, `it had trusted_hash = "sha256:old"`) {
		t.Fatalf("init --trust-codex: %d %s %s", code, out, errOut)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.HasPrefix(string(data), "model = \"gpt-6\"  # mine\n") || strings.Contains(string(data), "untrusted") || strings.Count(string(data), "[projects.") != 1 {
		t.Fatalf("config.toml:\n%s", data)
	}
	if out, _, code := tm.as("alice", a, "", "doctor"); code != 0 || !strings.Contains(out, "Codex trusts this project") {
		t.Fatalf("doctor after trusting: %d\n%s", code, out)
	}
	if out, _, _ := tm.as("alice", a, "", "init", "--trust-codex"); strings.Contains(out, "it had") {
		t.Fatalf("second --trust-codex changed something:\n%s", out)
	}
}

func TestFlagsMayFollowArguments(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	out, errOut, code := tm.as("alice", a, "", "declare", "-m", "rework web", "web/**", "-x")
	if code != 0 || !strings.Contains(out, "Declared exclusive intent on web/**.") {
		t.Fatalf("declare with -x last: %d %s %s", code, out, errOut)
	}
	if _, errOut, code := tm.as("alice", a, "", "declare", "-h"); code != 0 || !strings.Contains(errOut, "usage: intagent declare") {
		t.Fatalf("declare -h: %d %s", code, errOut)
	}
}

// Gemini runs project and user hooks alike: the user-level copy stands aside
// where the directory Gemini started in wires intagent itself.
func TestGeminiUserHookStandsAsideForTheProject(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	if out, errOut, code := tm.as("bob", b, "", "init", "--user"); code != 0 || !strings.Contains(out, filepath.Join(os.Getenv("HOME"), ".gemini", "settings.json")) {
		t.Fatalf("init --user: %d %s %s", code, out, errOut)
	}
	tm.as("alice", a, "", "declare", "-x", "-m", "Pay", "svc/pay/**")
	tm.as("alice", a, claudeEvent("a1", a, "UserPromptSubmit", map[string]any{"prompt": "pay"}), "hook")
	before := func(cwd string) string {
		ev, _ := json.Marshal(map[string]any{"session_id": "g1", "cwd": cwd, "hook_event_name": "BeforeTool", "tool_name": "write_file",
			"tool_input": map[string]string{"file_path": filepath.Join(b, "svc/pay/retry.go")}})
		out, _, _ := tm.as("bob", b, string(ev), "hook", "gemini", "--user")
		return out
	}
	if out := before(b); out != "" {
		t.Fatalf("at the root the project's hook answers, not the user's: %q", out)
	}
	if out := before(filepath.Join(b, "svc")); !strings.Contains(out, `"decision":"deny"`) {
		t.Fatalf("in a subdirectory the user's hook must answer: %q", out)
	}
}

// Re-running init (to trust Codex, add the git hook or after an upgrade)
// keeps the agents the team chose.
func TestInitRerunKeepsTheTeamsAgents(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	if out, errOut, code := tm.as("alice", a, "", "init", "--url", tm.url, "--agents", "claude-code,codex"); code != 0 {
		t.Fatalf("init: %s %s", out, errOut)
	}
	writeFile(t, filepath.Join(a, ".gemini/settings.json"), "{ // the team's own, with comments\n}\n")
	if out, errOut, code := tm.as("alice", a, "", "init", "--trust-codex"); code != 0 || !strings.Contains(out, "already enrolled; nothing in the repository changed") ||
		strings.Contains(out, "Commit these files") {
		t.Fatalf("re-run: %s %s", out, errOut)
	}
	var rc struct{ Agents []string }
	readJSON(t, filepath.Join(a, ".intagent.json"), &rc)
	if strings.Join(rc.Agents, ",") != "claude-code,codex" {
		t.Fatalf("agents = %v", rc.Agents)
	}
	if _, err := os.Stat(filepath.Join(a, ".cursor")); err == nil {
		t.Fatal("the re-run wired Cursor")
	}
}

func TestCLIArgumentsKeepTheirShape(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	// A directory typed with a slash in a subdirectory stays a directory.
	if out, errOut, code := tm.as("alice", filepath.Join(a, "svc"), "", "declare", "-x", "-m", "conf", "conf.d/"); code != 0 || !strings.Contains(out, "svc/conf.d/**") {
		t.Fatalf("declare conf.d/: %d %s %s", code, out, errOut)
	}
	// Note text may hold dashes, even -h.
	tm.as("bob", b, claudeEvent("b1", b, "UserPromptSubmit", map[string]any{"prompt": "x"}), "hook")
	for _, text := range [][]string{{"renaming", "retry", "->", "backoff"}, {"intagent", "-h", "lists", "commands"}} {
		args := append([]string{"note", "bob"}, text...)
		if out, errOut, code := tm.as("alice", a, "", args...); code != 0 || !strings.Contains(out, "Note queued") {
			t.Fatalf("note %v: %d %s %s", text, code, out, errOut)
		}
	}
	out, _, _ := tm.as("bob", b, claudeEvent("b1", b, "PostToolUse", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}}), "hook")
	if !strings.Contains(out, "renaming retry -> backoff") || !strings.Contains(out, "intagent -h lists commands") {
		t.Fatalf("notes as delivered: %s", out)
	}
}

// A teammate who is not signed in, or whose token was rotated, hears so;
// before, the hook went quiet and they were invisible to the team.
func TestMembersOffTheBoardAreTold(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	start := claudeEvent("d1", a, "SessionStart", map[string]any{"source": "startup"})
	out, _, code := tm.as("dave", a, start, "hook") // dave never ran intagent login
	if _, _, ctx := decision(t, out); code != 0 || !strings.Contains(ctx, "this computer is not signed in to "+tm.url) ||
		!strings.Contains(ctx, "intagent login --url "+tm.url) {
		t.Fatalf("not signed in: %d %q", code, out)
	}
	t.Setenv("INTAGENT_TOKEN", "ia_rotated")
	out, _, _ = tm.as("alice", a, start, "hook")
	if _, _, ctx := decision(t, out); !strings.Contains(ctx, "rejected this computer's token") {
		t.Fatalf("rejected token: %q", out)
	}
	// Only the session's start says it; edits stay quiet...
	edit := claudeEvent("d1", a, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}})
	if out, _, _ := tm.as("alice", a, edit, "hook"); out != "" {
		t.Fatalf("edit while rejected: %q", out)
	}
	// ...unless the person asked for fail-closed.
	t.Setenv("INTAGENT_FAIL", "closed")
	out, _, _ = tm.as("alice", a, edit, "hook")
	if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "INTAGENT_FAIL=closed is set in your user's environment") {
		t.Fatalf("fail closed with a rejected token: %q", out)
	}
	// Writes outside the repository, such as an agent's plans, are not intagent's to refuse.
	outside := claudeEvent("d1", a, "PreToolUse", map[string]any{"tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(t.TempDir(), "plan.md")}})
	if out, _, code := tm.as("alice", a, outside, "hook"); out != "" || code != 0 {
		t.Fatalf("writing outside the repository with a rejected token, fail-closed: %d %q", code, out)
	}
	t.Setenv("INTAGENT_TOKEN", "")
	if out, _, code := tm.as("dave", a, outside, "hook"); out != "" || code != 0 {
		t.Fatalf("writing outside the repository, not signed in, fail-closed: %d %q", code, out)
	}
	t.Setenv("INTAGENT_TOKEN", "ia_rotated")
	// The guard says it did not check, and refuses under fail-closed.
	writeFile(t, filepath.Join(a, "x.go"), "package x\n")
	gitRun(t, a, "add", "x.go")
	if _, errOut, code := tm.as("alice", a, "", "guard"); code != 1 || !strings.Contains(errOut, "rejected your token") {
		t.Fatalf("guard, fail-closed: %d %s", code, errOut)
	}
	t.Setenv("INTAGENT_FAIL", "")
	if _, errOut, code := tm.as("alice", a, "", "guard"); code != 0 || !strings.Contains(errOut, "was not checked") {
		t.Fatalf("guard: %d %s", code, errOut)
	}
}

// Members added or rotated while the server runs take effect without a
// restart, and a rotated token stops working.
func TestServeReloadsMembers(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "team.json")
	run := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		app := &App{In: strings.NewReader(""), Out: &out, Err: &errb, Version: "test", Dir: dir}
		if code := app.Run(context.Background(), args); code != 0 {
			t.Fatalf("%v: %s", args, errb.String())
		}
		return strings.TrimSpace(strings.Split(out.String(), "\n")[2])
	}
	run("token", "add", "alice", "--config", cfg)
	var out, errb bytes.Buffer
	addr := startServe(t, &App{In: strings.NewReader(""), Out: &out, Err: &errb, Version: "test", Dir: dir, TeamFilePoll: 20 * time.Millisecond},
		"--config", cfg)
	whoami := func(tok string) int {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	waitFor := func(tok string, want int) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if whoami(tok) == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("token %s… never answered %d", tok[:6], want)
	}
	carol := run("token", "add", "carol", "--config", cfg)
	waitFor(carol, http.StatusOK)
	rotated := run("token", "add", "carol", "--config", cfg, "--rotate")
	waitFor(rotated, http.StatusOK)
	if code := whoami(carol); code != http.StatusUnauthorized {
		t.Fatalf("the rotated-out token still answers %d", code)
	}
}

// A stream cap of 0 allows no dashboards, as it reads: --max-public-streams
// 0 used to allow 100 without a token, the default, and said nothing.
func TestServeStreamCapsOfZeroAllowNone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "team.json")
	var out, errb bytes.Buffer
	app := &App{In: strings.NewReader(""), Out: &out, Err: &errb, Version: "test", Dir: dir}
	if code := app.Run(context.Background(), []string{"token", "add", "alice", "--config", cfg}); code != 0 {
		t.Fatalf("token add: %s", errb.String())
	}
	alice := strings.TrimSpace(strings.Split(out.String(), "\n")[2])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a server that starts stops
	defer cancel()
	if code := app.Run(ctx, []string{"serve", "--config", cfg, "--data", "", "--addr", "127.0.0.1:0", "--max-streams", "-1"}); code == 0 ||
		!strings.Contains(errb.String(), "--max-streams cannot be negative") {
		t.Fatalf("a negative cap: %d %s", code, errb.String())
	}
	addr := startServe(t, &App{In: strings.NewReader(""), Out: &out, Err: &errb, Version: "test", Dir: dir},
		"--config", cfg, "--public-read", "--max-public-streams", "0")
	stream := func(token string) int {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/stream", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := stream(""); code != http.StatusTooManyRequests {
		t.Fatalf("a stream without a token, with --max-public-streams 0: %d", code)
	}
	if code := stream(alice); code != http.StatusOK {
		t.Fatalf("alice's stream: %d", code)
	}
}

// A broken .intagent.json is not "outside a repository": doctor names it,
// the hook logs it, and fail-closed refuses edits instead of passing them.
func TestBrokenRepoFileIsReported(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	writeFile(t, filepath.Join(a, ".intagent.json"), `{"url": "`+tm.url+`",}`)
	if out, _, code := tm.as("alice", a, "", "doctor"); code != 1 || !strings.Contains(out, ".intagent.json") || strings.Contains(out, "not inside a git repository") {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	edit := claudeEvent("a1", a, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}})
	if out, _, code := tm.as("alice", a, edit, "hook"); out != "" || code != 0 {
		t.Fatalf("hook: %q %d", out, code)
	}
	logged, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "intagent", "hook.log"))
	if !strings.Contains(string(logged), ".intagent.json") {
		t.Fatalf("hook log: %s", logged)
	}
	t.Setenv("INTAGENT_FAIL", "closed")
	out, _, _ := tm.as("alice", a, edit, "hook")
	if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "could not read this repository's setup") {
		t.Fatalf("fail closed: %q", out)
	}
}

func TestHookKeepsToItsBudget(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	tm.hookBudget = 300 * time.Millisecond
	// A git that hangs, as one can on a network file system or behind a stuck lock.
	slow := t.TempDir()
	writeFile(t, filepath.Join(slow, "git"), "#!/bin/sh\nexec sleep 30\n")
	if err := os.Chmod(filepath.Join(slow, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", slow+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INTAGENT_FAIL", "closed")
	edit := claudeEvent("a1", a, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}})
	start := time.Now()
	out, _, _ := tm.as("alice", a, edit, "hook")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the hook took %s with a budget of %s", took, tm.hookBudget)
	}
	if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "could not read this repository's setup") {
		t.Fatalf("a hook out of time under fail-closed: %q", out)
	}
}

// Under fail-closed, a setup intagent cannot read refuses edits in the team's
// repository only: not writes outside it, not repositories no team enrolled,
// and not while INTAGENT_DISABLE switches intagent off.
func TestBrokenSetupRefusesOnlyTeamEdits(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	writeFile(t, filepath.Join(a, ".intagent.json"), `{"url": "`+tm.url+`",}`)
	t.Setenv("INTAGENT_FAIL", "Closed")
	inside := claudeEvent("a1", a, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}})
	if out, _, _ := tm.as("alice", a, inside, "hook"); !strings.Contains(out, `"deny"`) {
		t.Fatalf("an edit in the team's repository: %q", out)
	}
	outside := claudeEvent("a1", a, "PreToolUse", map[string]any{"tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(t.TempDir(), "plan.md")}})
	if out, _, _ := tm.as("alice", a, outside, "hook"); out != "" {
		t.Fatalf("a write outside the repository: %q", out)
	}
	t.Setenv("INTAGENT_DISABLE", "1")
	if out, _, _ := tm.as("alice", a, inside, "hook"); out != "" {
		t.Fatalf("with INTAGENT_DISABLE=1: %q", out)
	}
	t.Setenv("INTAGENT_DISABLE", "")
	// A corrupt user config breaks every repository's setup, a personal one's too.
	personal := t.TempDir()
	gitRun(t, personal, "init", "-q")
	writeFile(t, filepath.Join(tm.dir, "alice.json"), "{oops")
	edit := claudeEvent("p1", personal, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(personal, "main.go")}})
	if out, _, _ := tm.as("alice", personal, edit, "hook"); out != "" {
		t.Fatalf("a repository no team enrolled: %q", out)
	}
}

// A hook finishes before the agent that runs it gives up on it, whichever
// agent runs the wiring: Copilot CLI runs Claude Code's, and so does Cursor.
func TestHookBudgetFitsEveryWiredTimeout(t *testing.T) {
	for _, c := range []struct {
		wiring []hookWire
		agents []board.Agent
	}{
		{claudeWiring, []board.Agent{board.AgentClaudeCode, board.AgentCopilot, board.AgentCursor}},
		{codexWiring, []board.Agent{board.AgentCodex}},
		{geminiWiring, []board.Agent{board.AgentGemini}},
		{cursorWiring, []board.Agent{board.AgentCursor}},
	} {
		for _, agent := range c.agents {
			for _, w := range c.wiring {
				kind := board.KindPrompt
				if strings.EqualFold(w.event, "SessionEnd") {
					kind = board.KindSessionEnd
				}
				budget, limit := hookBudget(agent, kind, func(string) string { return "" }), time.Duration(w.timeout)*time.Second
				if budget > limit-time.Second {
					t.Errorf("%s running %s: hook budget %s, agent timeout %s", agent, w.event, budget, limit)
				}
			}
		}
	}
}

// At a session's end git gets the time the agent allows, less what the
// request needs; Claude Code's own bound, when its person set one, is kept.
func TestSessionEndBudgetFollowsTheAgent(t *testing.T) {
	for _, c := range []struct {
		agent board.Agent
		env   string
		want  time.Duration
	}{
		{board.AgentClaudeCode, "", 4 * time.Second},
		{board.AgentGemini, "", 4 * time.Second},
		{board.AgentCursor, "", 4 * time.Second},
		{board.AgentCodex, "", 2 * time.Second},
		{board.AgentCopilot, "", 2 * time.Second},
		{"aider", "", 2 * time.Second},
		{board.AgentClaudeCode, "3000", 2250 * time.Millisecond},
		{board.AgentClaudeCode, "1000", 500 * time.Millisecond},
		{board.AgentClaudeCode, "60000", 4 * time.Second},
		{board.AgentClaudeCode, "soon", 4 * time.Second},
		{board.AgentGemini, "1000", 4 * time.Second},
	} {
		getenv := func(k string) string {
			if k == "CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS" {
				return c.env
			}
			return ""
		}
		if got := hookBudget(c.agent, board.KindSessionEnd, getenv); got != c.want {
			t.Errorf("%s with %q: %s, want %s", c.agent, c.env, got, c.want)
		}
	}
	if got := hookBudget(board.AgentCodex, board.KindStop, os.Getenv); got != 8*time.Second {
		t.Errorf("Stop: %s", got)
	}
}

// Under fail-closed, an edit named through a symbolic link to the team's
// repository (macOS's /var and /private/var, a ~/work link) is still inside it.
func TestWithinFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	link := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".intagent.json"), "{}")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	for _, c := range []struct{ cwd, path string }{
		{root, filepath.Join(link, "README.md")},
		{link, filepath.Join(root, "new", "file.go")},
		{link, "README.md"},
	} {
		if !within(enrolledRoot(c.cwd), c.cwd, []string{c.path}) {
			t.Errorf("in %s, %s was not inside the repository", c.cwd, c.path)
		}
	}
	if within(enrolledRoot(link), link, []string{filepath.Join(dir, "elsewhere.md")}) {
		t.Error("a file beside the repository counted as inside it")
	}
}

// In a large repository git may take seconds to list the changes: it gets
// what the server request leaves of the hook's time, not half the request's.
func TestSlowGitStillReportsTheFootprint(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	writeFile(t, filepath.Join(a, "svc/pay/new.go"), "package pay\n")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	slow := t.TempDir()
	writeFile(t, filepath.Join(slow, "git"), "#!/bin/sh\ncase \" $* \" in *\" diff \"*|*\" ls-files \"*) sleep 1.3;; esac\nexec "+real+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(slow, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", slow+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, errOut, code := tm.as("alice", a, claudeEvent("a1", a, "SessionStart", map[string]any{"source": "startup"}), "hook"); code != 0 {
		t.Fatalf("hook: %d %s", code, errOut)
	}
	for _, r := range tm.srv.Board().Repos(time.Now()) {
		for _, c := range tm.srv.Board().View(time.Now(), r.Repo).Claims {
			if c.FileCount > 0 {
				return
			}
		}
	}
	logged, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "intagent", "hook.log"))
	t.Fatalf("no footprint reached the board; hook log:\n%s", logged)
}

func TestCapTextCutsAtALine(t *testing.T) {
	text := "head\n- one\n- two\n- three"
	if got := capText(text, len(text)); got != text {
		t.Fatalf("cut a text that fits: %q", got)
	}
	got := capText(text, 14)
	if got != "head\n- one\n- and more; use the intagent check_paths tool for the files you plan to change." {
		t.Fatalf("capText = %q", got)
	}
}
