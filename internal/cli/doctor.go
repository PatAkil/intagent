package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/gitx"
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
	switch {
	case errors.Is(err, gitx.ErrNotRepo):
		bad("%s is not inside a git repository", dir)
		return exitError{code: 1}
	case err != nil:
		bad("%v", err)
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
		for _, w := range wirings {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(w.files[0]))); err == nil {
				wired = append(wired, w.name)
			}
		}
	}
	for _, w := range wirings {
		if !slices.Contains(wired, w.name) {
			info("%s: not wired in this repository", w.title)
			continue
		}
		for _, c := range w.checks {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(c.rel)))
			switch {
			case err != nil:
				bad("%s: %s is missing; run 'intagent init'", c.what, c.rel)
			case c.wired(data):
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
