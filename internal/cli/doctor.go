package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// doctor checks every link between this worktree, its agents and the server.
func (a *App) doctor(ctx context.Context, args []string) error {
	fs := a.flags("doctor", "doctor")
	if err := fs.Parse(args); err != nil {
		return errUsage
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
		ok("enrolled: %s points at %s", ".intagent.json", ws.settings.URL)
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
		if err != nil {
			bad("cannot reach %s: %v", ws.settings.URL, err)
		} else {
			ok("server %s knows you as %s (server %s)", ws.settings.URL, who.Member, who.Version)
			ok("policy: block=%s overlap=%s nearby=%s", who.Policy.Block, who.Policy.Overlap, who.Policy.Nearby)
		}
	}
	check := func(rel, needle, what string) {
		data, err := os.ReadFile(filepath.Join(root, rel))
		switch {
		case err != nil:
			info("%s: %s not found", what, rel)
		case strings.Contains(string(data), needle):
			ok("%s: %s", what, rel)
		default:
			bad("%s: %s exists but does not mention intagent; run 'intagent init'", what, rel)
		}
	}
	check(".claude/settings.json", "intagent", "Claude Code hooks")
	check(".mcp.json", "intagent", "Claude Code MCP server")
	check(".codex/hooks.json", "intagent hook codex", "Codex hooks")
	check(".codex/config.toml", "[mcp_servers.intagent]", "Codex MCP server")
	check(".cursor/hooks.json", "intagent hook cursor", "Cursor hooks")
	if dir != root {
		info("Claude Code reads project hooks only from the directory it starts in; start it at %s, or run 'intagent init --user'", root)
	}
	if problems > 0 {
		return exitError{code: 1, msg: fmt.Sprintf("\n%d thing(s) to fix.", problems)}
	}
	fmt.Fprintln(a.Out, "\nAll connected.")
	return nil
}
