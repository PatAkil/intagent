package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The wiring each agent gets. Matchers and timeouts follow what the agents
// were verified to accept; see docs/integrations.md.

type hookWire struct {
	event   string
	matcher string
	timeout int
}

var claudeWiring = []hookWire{
	{"SessionStart", "", 10},
	{"UserPromptSubmit", "", 10},
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit|Bash", 10},
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit|Bash", 10},
	{"PostToolUseFailure", "Edit|Write|MultiEdit|NotebookEdit|Bash", 10},
	{"Stop", "", 10},
	{"SessionEnd", "", 5},
}

var codexWiring = []hookWire{
	{"SessionStart", "", 10},
	{"UserPromptSubmit", "", 10},
	{"PreToolUse", "^(apply_patch|Bash)$", 10},
	{"PostToolUse", "^(apply_patch|Bash)$", 10},
	{"Stop", "", 10},
	{"SessionEnd", "", 3},
}

// hookCommand is the handler for Claude Code and Cursor. It is written in shell
// form, with no "args", because Cursor imports Claude Code's hooks and keeps
// only the command string; and it is the same string in both files, so Cursor
// recognises the imported copy as a duplicate of its own and runs it once.
// The adapter is chosen from the payload.
//
// The files are committed, so the hook also runs for teammates who have not
// installed intagent. It then does nothing: GitHub Copilot CLI, which runs
// these hooks too, refuses every edit when a pre-tool hook fails.
//
// The braces matter: Cursor passes the payload by appending a here-document
// to the command, which binds to the last simple command, so without the
// group it would feed 'true' rather than intagent.
const hookCommand = "{ command -v intagent >/dev/null && intagent hook || true; }"

const codexCommand = "intagent hook codex"

// geminiCommand is guarded like hookCommand. Gemini CLI does not read Claude
// Code's hooks, so it names its adapter.
const geminiCommand = "{ command -v intagent >/dev/null && intagent hook gemini || true; }"

// geminiUserCommand is the same hook in the user's settings, for Gemini
// started in a subdirectory. Gemini runs both when it starts at the root, so
// this one stands aside where the project wires Gemini itself.
const geminiUserCommand = "{ command -v intagent >/dev/null && intagent hook gemini --user || true; }"

// geminiWiring times out in seconds here; Gemini CLI counts milliseconds.
var geminiWiring = []hookWire{
	{"SessionStart", "", 10},
	{"BeforeAgent", "", 10},
	{"BeforeTool", "^(write_file|replace|run_shell_command)$", 10},
	{"AfterTool", "^(write_file|replace|run_shell_command)$", 10},
	{"AfterAgent", "", 10},
	{"SessionEnd", "", 5},
}

// cursorWiring uses only events Cursor accepts: an unknown event name makes
// Cursor drop the whole file.
var cursorWiring = []hookWire{
	{"sessionStart", "", 10},
	{"beforeSubmitPrompt", "", 10},
	{"preToolUse", "^(Write|Delete|Shell)$", 10},
	{"postToolUse", "^(Write|Delete|Shell)$", 10},
	{"postToolUseFailure", "^(Write|Delete|Shell)$", 10},
	{"stop", "", 10},
	{"sessionEnd", "", 10},
}

// readJSONObject reads a JSON object file; a missing file is an empty object.
// It refuses to continue on invalid JSON rather than overwrite someone's file.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON, so intagent left it alone: %w", path, err)
	}
	return m, nil
}

func writeJSONObject(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// intagentHook matches a shell command that runs 'intagent hook', from a path
// or not, guarded or not.
var intagentHook = regexp.MustCompile(`(?:^|[\s/&|;])intagent(?:\.exe)? hook(?:\s|$)`)

// isIntagentHandler recognises a handler intagent installed, in exec or shell form.
func isIntagentHandler(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	if intagentHook.MatchString(cmd) {
		return true
	}
	if filepath.Base(cmd) != "intagent" {
		return false
	}
	args := asSlice(h["args"])
	return len(args) > 0 && args[0] == "hook"
}

// mergeHooks makes intagent's handlers in a Claude-style hooks object exactly
// the given wiring: older intagent handlers are removed from every event, in
// whatever form they were written, one current handler is added per wired
// event, and everyone else's handlers stay as they were. It reports whether
// anything changed.
func mergeHooks(hooks map[string]any, wiring []hookWire, handler func(hookWire) map[string]any) bool {
	return rewire(hooks, wiring, func(groups []any) []any {
		var kept []any
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			handlers := asSlice(group["hooks"])
			others := withoutIntagent(handlers)
			if len(others) == 0 && len(handlers) > 0 {
				continue
			}
			if len(others) < len(handlers) {
				group["hooks"] = others
			}
			kept = append(kept, group)
		}
		return kept
	}, func(w hookWire) any {
		group := map[string]any{"hooks": []any{handler(w)}}
		if w.matcher != "" {
			group["matcher"] = w.matcher
		}
		return group
	})
}

// mergeFlatHooks does the same for Cursor's flat lists of handlers.
func mergeFlatHooks(hooks map[string]any, wiring []hookWire, handler func(hookWire) map[string]any) bool {
	return rewire(hooks, wiring, withoutIntagent, func(w hookWire) any { return handler(w) })
}

// rewire strips intagent's entries from every event list, drops lists that
// only held intagent's, and appends the current entry to each wired event.
func rewire(hooks map[string]any, wiring []hookWire, strip func([]any) []any, entry func(hookWire) any) bool {
	before, _ := json.Marshal(hooks)
	for event, v := range hooks {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		// A group strip keeps is edited in place, so an unchanged length means
		// the list itself needs no rewriting.
		switch kept := strip(list); {
		case len(kept) == len(list):
		case len(kept) == 0:
			delete(hooks, event)
		default:
			hooks[event] = kept
		}
	}
	for _, w := range wiring {
		hooks[w.event] = append(asSlice(hooks[w.event]), entry(w))
	}
	after, _ := json.Marshal(hooks)
	return !bytes.Equal(before, after)
}

func withoutIntagent(handlers []any) []any {
	var others []any
	for _, h := range handlers {
		if !isIntagentHandler(asMap(h)) {
			others = append(others, h)
		}
	}
	return others
}

// installClaude wires Claude Code's hooks and permissions into a settings file.
func installClaude(settingsPath string) (bool, error) {
	m, err := readJSONObject(settingsPath)
	if err != nil {
		return false, err
	}
	hooks := asMap(m["hooks"])
	changed := mergeHooks(hooks, claudeWiring, func(w hookWire) map[string]any {
		return map[string]any{"type": "command", "command": hookCommand, "timeout": w.timeout}
	})
	m["hooks"] = hooks
	perms := asMap(m["permissions"])
	allow := asSlice(perms["allow"])
	if !slices.Contains(allow, any("mcp__intagent__*")) {
		perms["allow"] = append(allow, "mcp__intagent__*")
		m["permissions"] = perms
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, writeJSONObject(settingsPath, m)
}

// installMCPJSON registers the intagent MCP server in a .mcp.json-style file.
func installMCPJSON(path string, withType bool) (bool, error) {
	m, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	servers := asMap(m["mcpServers"])
	if _, ok := servers["intagent"]; ok {
		return false, nil
	}
	entry := map[string]any{"command": "intagent", "args": []any{"mcp"}}
	if withType {
		entry["type"] = "stdio"
	}
	servers["intagent"] = entry
	m["mcpServers"] = servers
	return true, writeJSONObject(path, m)
}

// installCodex writes .codex/hooks.json and the MCP server in .codex/config.toml.
// Codex rejects unknown keys in hooks.json, so only "description" and "hooks" are written.
func installCodex(root string) ([]string, error) {
	var wrote []string
	hooksPath := filepath.Join(root, ".codex", "hooks.json")
	m, err := readJSONObject(hooksPath)
	if err != nil {
		return nil, err
	}
	hooks := asMap(m["hooks"])
	if mergeHooks(hooks, codexWiring, func(w hookWire) map[string]any {
		return map[string]any{"type": "command", "command": codexCommand, "timeout": w.timeout}
	}) {
		m["hooks"] = hooks
		if _, ok := m["description"]; !ok {
			m["description"] = "intagent: tells this agent what teammates' agents are changing"
		}
		if err := writeJSONObject(hooksPath, m); err != nil {
			return nil, err
		}
		wrote = append(wrote, hooksPath)
	}
	cfgPath := filepath.Join(root, ".codex", "config.toml")
	existing, err := os.ReadFile(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !bytes.Contains(existing, []byte("[mcp_servers.intagent]")) {
		block := "\n[mcp_servers.intagent]\ncommand = \"intagent\"\nargs = [\"mcp\"]\nenv_vars = [\"INTAGENT_URL\", \"INTAGENT_TOKEN\", \"INTAGENT_CONFIG\"]\ntool_timeout_sec = 30\n"
		if len(existing) == 0 {
			block = strings.TrimPrefix(block, "\n")
		}
		// A repository file, meant to be committed and read by everyone's Codex.
		f, err := os.OpenFile(cfgPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(block); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		wrote = append(wrote, cfgPath)
	}
	return wrote, nil
}

// codexEventLabels are the names Codex uses in hook trust keys.
var codexEventLabels = map[string]string{
	"PreToolUse": "pre_tool_use", "PermissionRequest": "permission_request", "PostToolUse": "post_tool_use",
	"PreCompact": "pre_compact", "PostCompact": "post_compact", "SessionStart": "session_start",
	"SessionEnd": "session_end", "UserPromptSubmit": "user_prompt_submit", "SubagentStart": "subagent_start",
	"SubagentStop": "subagent_stop", "Stop": "stop", "Interrupt": "interrupt",
}

// codexTrustEntries computes the hook-trust keys and hashes Codex expects in
// the user's config for every intagent handler in a hooks.json file. The
// recipe mirrors codex-rs hooks discovery: a canonical, key-sorted JSON
// identity of the handler, hashed with SHA-256.
func codexTrustEntries(hooksPath string) (map[string]string, error) {
	m, err := readJSONObject(hooksPath)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for event, groups := range asMap(m["hooks"]) {
		label, ok := codexEventLabels[event]
		if !ok {
			continue
		}
		for gi, g := range asSlice(groups) {
			group := asMap(g)
			for hi, h := range asSlice(group["hooks"]) {
				hm := asMap(h)
				if hm["type"] != "command" || !isIntagentHandler(hm) {
					continue
				}
				hash, err := codexHookHash(event, label, group, hm)
				if err != nil {
					return nil, err
				}
				out[fmt.Sprintf("%s:%s:%d:%d", hooksPath, label, gi, hi)] = hash
			}
		}
	}
	return out, nil
}

func codexHookHash(event, label string, group, h map[string]any) (string, error) {
	timeout := 600.0
	if t, ok := h["timeout"].(float64); ok {
		timeout = t
	}
	switch event {
	case "SessionEnd", "Interrupt":
		if _, ok := h["timeout"].(float64); !ok {
			timeout = 1
		}
		timeout = max(1, min(3, timeout))
	default:
		timeout = max(1, timeout)
	}
	async, _ := h["async"].(bool)
	handler := map[string]any{"type": "command", "command": h["command"], "timeout": int(timeout), "async": async}
	if s, ok := h["statusMessage"]; ok && s != nil {
		handler["statusMessage"] = s
	}
	if l, ok := h["additionalContextLimit"].(float64); ok && l != 2500 {
		handler["additionalContextLimit"] = int(l)
	}
	ident := map[string]any{"event_name": label, "hooks": []any{handler}}
	if matcher, ok := group["matcher"]; ok && event != "UserPromptSubmit" && event != "Stop" && event != "Interrupt" {
		ident["matcher"] = matcher
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ident); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// installGemini writes Gemini CLI's hooks and MCP server into
// .gemini/settings.json.
func installGemini(root string) (bool, error) {
	p := filepath.Join(root, ".gemini", "settings.json")
	m, err := readJSONObject(p)
	if err != nil {
		return false, err
	}
	hooks := asMap(m["hooks"])
	changed := mergeHooks(hooks, geminiWiring, func(w hookWire) map[string]any {
		return map[string]any{"type": "command", "command": geminiCommand, "name": "intagent", "timeout": w.timeout * 1000}
	})
	m["hooks"] = hooks
	servers := asMap(m["mcpServers"])
	if _, ok := servers["intagent"]; !ok {
		servers["intagent"] = map[string]any{"command": "intagent", "args": []any{"mcp"}}
		m["mcpServers"] = servers
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, writeJSONObject(p, m)
}

// installGeminiUser writes Gemini CLI's hooks into a user settings file.
func installGeminiUser(p string) (bool, error) {
	m, err := readJSONObject(p)
	if err != nil {
		return false, err
	}
	hooks := asMap(m["hooks"])
	changed := mergeHooks(hooks, geminiWiring, func(w hookWire) map[string]any {
		return map[string]any{"type": "command", "command": geminiUserCommand, "name": "intagent", "timeout": w.timeout * 1000}
	})
	if !changed {
		return false, nil
	}
	m["hooks"] = hooks
	return true, writeJSONObject(p, m)
}

// installCursor writes .cursor/hooks.json and .cursor/mcp.json.
func installCursor(root string) ([]string, error) {
	var wrote []string
	hooksPath := filepath.Join(root, ".cursor", "hooks.json")
	m, err := readJSONObject(hooksPath)
	if err != nil {
		return nil, err
	}
	_, hasVersion := m["version"]
	if !hasVersion {
		m["version"] = 1
	}
	hooks := asMap(m["hooks"])
	if mergeFlatHooks(hooks, cursorWiring, func(w hookWire) map[string]any {
		h := map[string]any{"command": hookCommand, "timeout": w.timeout}
		if w.matcher != "" {
			h["matcher"] = w.matcher
		}
		return h
	}) || !hasVersion {
		m["hooks"] = hooks
		if err := writeJSONObject(hooksPath, m); err != nil {
			return nil, err
		}
		wrote = append(wrote, hooksPath)
	}
	mcpPath := filepath.Join(root, ".cursor", "mcp.json")
	if ok, err := installMCPJSON(mcpPath, false); err != nil {
		return nil, err
	} else if ok {
		wrote = append(wrote, mcpPath)
	}
	return wrote, nil
}

// installGitHook adds a pre-commit hook running 'intagent guard', unless a
// pre-commit hook already exists that intagent did not write.
func installGitHook(hooksDir string) (string, error) {
	p := filepath.Join(hooksDir, "pre-commit")
	existing, err := os.ReadFile(p)
	switch {
	case err == nil && bytes.Contains(existing, []byte("intagent guard")):
		return "", nil
	case err == nil:
		return "", fmt.Errorf("%s already exists; add the line 'intagent guard || exit 1' to it yourself", p)
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return "", err
	}
	// Git GUIs often run hooks with a shorter PATH; without intagent, commit as usual.
	script := "#!/bin/sh\n# intagent: refuse commits that touch files a teammate's active agent reserved.\n" +
		"command -v intagent >/dev/null || exit 0\nexec intagent guard\n"
	return p, os.WriteFile(p, []byte(script), 0o755)
}
