package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // keep the hook log out of the real cache
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
	srv, err := server.New(server.Options{Members: ms, Board: board.DefaultConfig()})
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
	app := &App{In: strings.NewReader(stdin), Out: &out, Err: &errb, Version: "test", Dir: dir}
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
	if _, _, ctx := decision(t, out); !strings.Contains(ctx, `Note from bob's agent: "I need retry.go for a hotfix"`) {
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
	for _, f := range []string{".intagent.json", ".mcp.json", ".codex/hooks.json", ".codex/config.toml", ".cursor/hooks.json", ".cursor/mcp.json", ".git/hooks/pre-commit"} {
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

func snapshotFiles(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range []string{".intagent.json", ".claude/settings.json", ".mcp.json", ".codex/hooks.json", ".codex/config.toml", ".cursor/hooks.json", ".cursor/mcp.json"} {
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

	t.Setenv("CLAUDE_PROJECT_DIR", a)
	calls := []string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"declare_intent","arguments":{"summary":"Retry rewrite","paths":["svc/pay/**"],"mode":"shared"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"check_paths","arguments":{"paths":["` + filepath.Join(a, "svc/pay/retry.go") + `"]}}}`,
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
	out, _, _ := tm.as("alice", a, "", "board")
	if !strings.Contains(out, "svc/pay/new.go") || !strings.Contains(out, "watch") {
		t.Fatalf("board after watch:\n%s", out)
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
	out, _, _ = tm.as("alice", a, "", "doctor")
	if !strings.Contains(out, "knows you as alice") || !strings.Contains(out, "Claude Code hooks: .claude/settings.json") {
		t.Fatalf("doctor after init:\n%s", out)
	}
	if out, _, code := tm.as("alice", a, "", "help"); code != 0 || !strings.Contains(out, "intagent init") {
		t.Fatalf("help: %d %s", code, out)
	}
	if _, errOut, code := tm.as("alice", a, "", "frobnicate"); code != 2 || !strings.Contains(errOut, "unknown command") {
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
