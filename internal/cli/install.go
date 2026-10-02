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

const codexCommand = "intagent hook codex"

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

// isIntagentHandler recognises a handler intagent installed, in exec or shell form.
func isIntagentHandler(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	if strings.HasPrefix(cmd, "intagent hook") || strings.Contains(cmd, "/intagent hook") {
		return true
	}
	if filepath.Base(cmd) != "intagent" {
		return false
	}
	args := asSlice(h["args"])
	return len(args) > 0 && args[0] == "hook"
}

// mergeHooks adds intagent's handlers to a Claude-style hooks object, once per event.
func mergeHooks(hooks map[string]any, wiring []hookWire, handler func(hookWire) map[string]any) bool {
	changed := false
	for _, w := range wiring {
		groups := asSlice(hooks[w.event])
		present := false
		for _, g := range groups {
			for _, h := range asSlice(asMap(g)["hooks"]) {
				if isIntagentHandler(asMap(h)) {
					present = true
				}
			}
		}
		if present {
			continue
		}
		group := map[string]any{"hooks": []any{handler(w)}}
		if w.matcher != "" {
			group["matcher"] = w.matcher
		}
		hooks[w.event] = append(groups, group)
		changed = true
	}
	return changed
}

// installClaude wires Claude Code's hooks and permissions into a settings file.
func installClaude(settingsPath string) (bool, error) {
	m, err := readJSONObject(settingsPath)
	if err != nil {
		return false, err
	}
	hooks := asMap(m["hooks"])
	changed := mergeHooks(hooks, claudeWiring, func(w hookWire) map[string]any {
		return map[string]any{"type": "command", "command": "intagent", "args": []any{"hook", "claude-code"}, "timeout": w.timeout}
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

// trustCodex appends project trust and hook trust for intagent's handlers to
// the user's Codex config. It only appends entries that are not present, so
// it never rewrites a table the user or Codex already wrote.
func trustCodex(codexConfig string, roots []string, hooksFiles []string) ([]string, error) {
	existing, err := os.ReadFile(codexConfig)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	text := string(existing)
	var add strings.Builder
	var notes []string
	for _, root := range roots {
		header := "[projects." + tomlString(root) + "]"
		if strings.Contains(text, header) {
			notes = append(notes, "project already listed: "+root)
			continue
		}
		fmt.Fprintf(&add, "\n%s\ntrust_level = \"trusted\"\n", header)
		notes = append(notes, "trusted project "+root)
	}
	for _, hf := range hooksFiles {
		entries, err := codexTrustEntries(hf)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if strings.Contains(text, tomlString(k)) {
				notes = append(notes, "hook already listed: "+k)
				continue
			}
			fmt.Fprintf(&add, "\n[hooks.state.%s]\ntrusted_hash = %s\n", tomlString(k), tomlString(entries[k]))
			notes = append(notes, "trusted hook "+k)
		}
	}
	if add.Len() == 0 {
		return notes, nil
	}
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(codexConfig, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString("\n# Added by 'intagent init --trust-codex'" + add.String()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return notes, f.Close()
}

// installCursor writes .cursor/hooks.json and .cursor/mcp.json.
func installCursor(root string) ([]string, error) {
	var wrote []string
	hooksPath := filepath.Join(root, ".cursor", "hooks.json")
	m, err := readJSONObject(hooksPath)
	if err != nil {
		return nil, err
	}
	if _, ok := m["version"]; !ok {
		m["version"] = 1
	}
	hooks := asMap(m["hooks"])
	changed := false
	for _, event := range cursorEvents {
		list := asSlice(hooks[event])
		present := false
		for _, h := range list {
			if isIntagentHandler(asMap(h)) {
				present = true
			}
		}
		if !present {
			hooks[event] = append(list, map[string]any{"command": "intagent hook cursor"})
			changed = true
		}
	}
	if changed {
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

var cursorEvents = []string{"beforeSubmitPrompt", "afterFileEdit", "stop"}

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
	script := "#!/bin/sh\n# intagent: refuse commits that touch files a teammate's active agent reserved.\nexec intagent guard\n"
	return p, os.WriteFile(p, []byte(script), 0o755)
}
