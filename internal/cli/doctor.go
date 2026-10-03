package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/patakil/intagent/internal/client"
)

// doctor checks every link between this worktree, its agents and the server.
func (a *App) doctor(ctx context.Context, args []string) error {
	fs := a.flags("doctor", "doctor")
	if err := parse(fs, args); err != nil {
		return err
	}
	problems := 0
	ok := func(format string, v ...any) { fmt.Fprintf(a.Out, "  ok   "+format+"\n", v...) }
	bad := func(format string, v ...any) { problems++; fmt.Fprintf(a.Out, "  FIX  "+format+"\n", v...) }
	info := func(format string, v ...any) { fmt.Fprintf(a.Out, "  --   "+format+"\n", v...) }

	if p, err := exec.LookPath("intagent"); err == nil {
		ok("intagent on PATH: %s", p)
	} else {
		bad("intagent is not on PATH; agents run it by name")
	}
	dir, err := a.workdir()
	if err != nil {
		return err
	}
	ws, err := openWorkspace(ctx, dir)
	if err != nil {
		bad("%s is not inside a git repository", dir)
		return exitError{code: 1}
	}
	root := ws.wt.Root
	ok("worktree %s on branch %s", root, ws.wt.Branch)
	ok("repository id %s", ws.where.Repo)
	if ws.settings.Enrolled {
		ok("enrolled: %s points at %s", ".intagent.json", ws.settings.Repo.URL)
		if v := os.Getenv("INTAGENT_URL"); v != "" {
			info("INTAGENT_URL overrides it: using %s", ws.settings.URL)
		}
	} else {
		bad("not enrolled: run 'intagent init --url <server>' here")
	}
	switch {
	case ws.settings.Disabled:
		bad("INTAGENT_DISABLE is set")
	case ws.settings.URL == "":
		bad("no server configured")
	case ws.settings.Token == "":
		bad("no token for %s: run 'intagent login --url %s'", ws.settings.URL, ws.settings.URL)
	default:
		who, err := ws.client.Whoami(ctx)
		switch {
		case client.IsUnauthorized(err) || (err == nil && who.Member == ""):
			bad("%s rejected your token (rotated?): get a new one and run 'intagent login --url %s'", ws.settings.URL, ws.settings.URL)
		case err != nil:
			bad("cannot reach %s: %v", ws.settings.URL, err)
		default:
			ok("server %s knows you as %s (server %s)", ws.settings.URL, who.Member, who.Version)
			ok("policy: block=%s overlap=%s nearby=%s", who.Policy.Block, who.Policy.Overlap, who.Policy.Nearby)
		}
	}
	wired := ws.settings.Repo.Agents
	if len(wired) == 0 {
		// Enrolled by an older intagent: check what is there.
		for _, ag := range agentNames {
			if _, err := os.Stat(filepath.Join(root, agentFiles[ag][0])); err == nil {
				wired = append(wired, ag)
			}
		}
	}
	for _, ag := range agentNames {
		if !slices.Contains(wired, ag) {
			info("%s: not wired in this repository", agentTitles[ag])
			continue
		}
		for _, c := range agentChecks[ag] {
			data, err := os.ReadFile(filepath.Join(root, c.rel))
			switch {
			case err != nil:
				bad("%s: %s is missing; run 'intagent init'", c.what, c.rel)
			case strings.Contains(string(data), c.needle):
				ok("%s: %s", c.what, c.rel)
			default:
				bad("%s: %s is not wired for this version of intagent; run 'intagent init'", c.what, c.rel)
			}
		}
	}
	if slices.Contains(wired, "codex") {
		a.doctorCodexTrust(ctx, ws, ok, bad, info)
	}
	if dir != root {
		info("you are in a subdirectory: Claude Code and Gemini CLI read project hooks only from the directory they start in, " +
			"and Cursor from the folder it opened. Start them at " + root + ", or run 'intagent init --user' once to cover " +
			"Claude Code, Gemini CLI and (through Claude's user hooks) Cursor everywhere")
	}
	if problems > 0 {
		return exitError{code: 1, msg: fmt.Sprintf("\n%d thing(s) to fix.", problems)}
	}
	fmt.Fprintln(a.Out, "\nAll connected.")
	return nil
}

var agentTitles = map[string]string{"claude-code": "Claude Code and Copilot CLI", "codex": "Codex", "cursor": "Cursor", "gemini": "Gemini CLI"}

type wiringCheck struct{ rel, needle, what string }

// agentChecks are what doctor looks for in each agent's files.
var agentChecks = map[string][]wiringCheck{
	"claude-code": {
		{".claude/settings.json", hookCommand, "Claude Code hooks (Copilot CLI runs them too)"},
		{".mcp.json", "intagent", "Claude Code MCP server"},
	},
	"codex": {
		{".codex/hooks.json", codexCommand, "Codex hooks"},
		{".codex/config.toml", "[mcp_servers.intagent]", "Codex MCP server"},
	},
	"cursor": {{".cursor/hooks.json", hookCommand, "Cursor hooks"}},
	"gemini": {{".gemini/settings.json", geminiCommand, "Gemini CLI hooks"}},
}

// doctorCodexTrust checks the two trust gates without which Codex silently
// runs none of the project's hooks.
func (a *App) doctorCodexTrust(ctx context.Context, ws *workspace, ok, bad, info func(string, ...any)) {
	ct, err := codexTrustFor(ctx, ws.wt)
	if err != nil {
		bad("Codex trust: %v", err)
		return
	}
	if _, err := os.Stat(ct.config); err != nil {
		// No Codex on this machine, as far as can be told: nothing to fix.
		info("Codex: if you use it here, run 'intagent init --trust-codex' once; Codex ignores hooks it has not trusted")
		return
	}
	missing, err := ct.problems()
	switch {
	case err != nil:
		bad("Codex trust: %v", err)
	case len(missing) > 0:
		bad("Codex ignores intagent's hooks here until you run 'intagent init --trust-codex' (%s does not trust %s)",
			ct.config, strings.Join(missing, ", "))
	default:
		ok("Codex trusts this project and intagent's hooks (%s)", ct.config)
	}
}
