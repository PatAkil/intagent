package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// agentWiring is what intagent knows about wiring one agent into a
// repository: the files init edits, what doctor looks for in them, the user
// settings --user also wires, and what init tells the person afterwards.
type agentWiring struct {
	name  string // as --agents names it
	title string // as doctor names it
	// files are the JSON files init edits, relative to the repository root;
	// init reads them all before it writes any.
	files []string
	// install wires the agent in and says what it wrote, one "file (what)"
	// per file it changed.
	install func(root string) ([]string, error)
	checks  []wiringCheck
	// userFile is the user settings file, relative to the home directory,
	// that init --user wires with installUser; "" when there is none.
	userFile    string
	installUser func(path string) (bool, error)
	// note is what the agent will ask of its person, said after init.
	note string
}

// wiringCheck is one thing doctor looks for in a file: a needle, or with mcp,
// an "intagent" entry under "mcpServers".
type wiringCheck struct {
	rel, needle, what string
	mcp               bool
}

func (c wiringCheck) wired(data []byte) bool {
	if !c.mcp {
		return strings.Contains(string(data), c.needle)
	}
	var m struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	return json.Unmarshal(data, &m) == nil && m.Servers["intagent"] != nil
}

// wirings lists the agents init wires, in order. Copilot CLI runs Claude
// Code's hooks, and Cursor imports them too.
var wirings = []agentWiring{
	{
		name: "claude-code", title: "Claude Code and Copilot CLI",
		files:   []string{".claude/settings.json", ".mcp.json"},
		install: installClaudeCode,
		checks: []wiringCheck{
			{rel: ".claude/settings.json", needle: hookCommand, what: "Claude Code hooks (Copilot CLI runs them too)"},
			{rel: ".mcp.json", mcp: true, what: "Claude Code MCP server"},
		},
		userFile: ".claude/settings.json", installUser: installClaude,
		note: "Claude Code asks once per member to approve the project's MCP server; accept 'intagent' when it does.",
	},
	{
		name: "codex", title: "Codex",
		files:   []string{".codex/hooks.json"},
		install: installedAs("Codex", installCodex),
		checks: []wiringCheck{
			{rel: ".codex/hooks.json", needle: codexCommand, what: "Codex hooks"},
			{rel: ".codex/config.toml", needle: "[mcp_servers.intagent]", what: "Codex MCP server"},
		},
	},
	{
		name: "cursor", title: "Cursor",
		files:   []string{".cursor/hooks.json", ".cursor/mcp.json"},
		install: installedAs("Cursor", installCursor),
		checks: []wiringCheck{
			{rel: ".cursor/hooks.json", needle: hookCommand, what: "Cursor hooks"},
			{rel: ".cursor/mcp.json", mcp: true, what: "Cursor MCP server"},
		},
	},
	{
		name: "gemini", title: "Gemini CLI",
		files:   []string{".gemini/settings.json"},
		install: installGeminiProject,
		checks: []wiringCheck{
			{rel: ".gemini/settings.json", needle: geminiCommand, what: "Gemini CLI hooks"},
			{rel: ".gemini/settings.json", mcp: true, what: "Gemini CLI MCP server"},
		},
		userFile: ".gemini/settings.json", installUser: installGeminiUser,
		note: "Gemini CLI runs a project's hooks only in folders you trust; trust this one when Gemini asks.",
	},
}

// agentNames lists the agents' names, in order.
var agentNames = func() []string {
	names := make([]string, len(wirings))
	for i, w := range wirings {
		names[i] = w.name
	}
	return names
}()

// wiringFor returns the wiring of a known agent.
func wiringFor(name string) agentWiring {
	for _, w := range wirings {
		if w.name == name {
			return w
		}
	}
	panic("intagent: no wiring for agent " + name)
}

func installClaudeCode(root string) ([]string, error) {
	var done []string
	if ok, err := installClaude(filepath.Join(root, ".claude", "settings.json")); err != nil {
		return nil, err
	} else if ok {
		done = append(done, ".claude/settings.json (Claude Code hooks, also run by Cursor and Copilot CLI)")
	}
	if ok, err := installMCPJSON(filepath.Join(root, ".mcp.json"), true); err != nil {
		return nil, err
	} else if ok {
		done = append(done, ".mcp.json (intagent MCP server)")
	}
	return done, nil
}

func installGeminiProject(root string) ([]string, error) {
	ok, err := installGemini(root)
	if err != nil || !ok {
		return nil, err
	}
	return []string{".gemini/settings.json (Gemini CLI hooks and MCP server)"}, nil
}

// installedAs adapts an installer that returns the paths it wrote.
func installedAs(what string, install func(root string) ([]string, error)) func(string) ([]string, error) {
	return func(root string) ([]string, error) {
		wrote, err := install(root)
		if err != nil {
			return nil, err
		}
		done := make([]string, len(wrote))
		for i, p := range wrote {
			rel, _ := filepath.Rel(root, p)
			done[i] = filepath.ToSlash(rel) + " (" + what + ")"
		}
		return done, nil
	}
}
