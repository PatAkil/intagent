package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/gitx"
)

func (a *App) initRepo(ctx context.Context, args []string) error {
	fs := a.flags("init", "init [--url <server>] [--agents claude-code,codex,cursor,gemini] [--git-hook] [--user] [--trust-codex]")
	url := fs.String("url", "", "the team server for this repository")
	agents := fs.String("agents", "claude-code,codex,cursor,gemini", "agents to wire up (Copilot CLI uses claude-code's)")
	gitHook := fs.Bool("git-hook", false, "install a pre-commit hook that runs 'intagent guard'")
	user := fs.Bool("user", false, "also install Claude Code hooks in your user settings, so they run when Claude starts in a subdirectory")
	trust := fs.Bool("trust-codex", false, "mark this project and intagent's hooks as trusted in your ~/.codex/config.toml")
	areas := fs.String("areas", "", "comma-separated globs that define areas, e.g. 'services/*,libs/*'")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dir, err := a.workdir()
	if err != nil {
		return err
	}
	wt, err := gitx.Open(ctx, dir)
	if err != nil {
		return err
	}
	root := wt.Root
	var done []string

	rc, _, err := client.LoadRepoConfig(root)
	if err != nil {
		return err
	}
	if *url != "" {
		rc.URL = client.NormalizeURL(*url)
	}
	if rc.URL == "" {
		if uc, _, err := client.LoadUserConfig(); err == nil && uc.Default != "" {
			rc.URL = uc.Default
		}
	}
	if rc.URL == "" {
		return errors.New("which server? pass --url https://your-intagent-server")
	}
	if *areas != "" {
		rc.Areas = splitList(*areas)
	}
	data, _ := json.MarshalIndent(rc, "", "  ")
	if err := os.WriteFile(filepath.Join(root, client.RepoFileName), append(data, '\n'), 0o644); err != nil {
		return err
	}
	done = append(done, client.RepoFileName+" (server "+rc.URL+")")

	for _, ag := range splitList(*agents) {
		switch ag {
		case "claude-code", "claude":
			if ok, err := installClaude(filepath.Join(root, ".claude", "settings.json")); err != nil {
				return err
			} else if ok {
				done = append(done, ".claude/settings.json (Claude Code hooks)")
			}
			if ok, err := installMCPJSON(filepath.Join(root, ".mcp.json"), true); err != nil {
				return err
			} else if ok {
				done = append(done, ".mcp.json (intagent MCP server)")
			}
			if *user {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				p := filepath.Join(home, ".claude", "settings.json")
				if ok, err := installClaude(p); err != nil {
					return err
				} else if ok {
					done = append(done, p+" (Claude Code hooks for every enrolled repository)")
				}
			}
		case "codex":
			wrote, err := installCodex(root)
			if err != nil {
				return err
			}
			for _, w := range wrote {
				rel, _ := filepath.Rel(root, w)
				done = append(done, rel+" (Codex)")
			}
			if *trust {
				notes, err := a.trustCodexFor(ctx, wt)
				if err != nil {
					return err
				}
				done = append(done, notes...)
			}
		case "cursor":
			wrote, err := installCursor(root)
			if err != nil {
				return err
			}
			for _, w := range wrote {
				rel, _ := filepath.Rel(root, w)
				done = append(done, rel+" (Cursor)")
			}
		case "gemini":
			if ok, err := installGemini(root); err != nil {
				return err
			} else if ok {
				done = append(done, ".gemini/settings.json (Gemini CLI hooks and MCP server)")
			}
		default:
			return fmt.Errorf("unknown agent %q: want claude-code, codex, cursor or gemini", ag)
		}
	}
	if *gitHook {
		hooksDir, err := wt.HooksDir(ctx)
		if err != nil {
			return err
		}
		p, err := installGitHook(hooksDir)
		if err != nil {
			fmt.Fprintf(a.Err, "note: %v\n", err)
		} else if p != "" {
			done = append(done, p+" (pre-commit guard)")
		}
	}

	fmt.Fprintf(a.Out, "Enrolled %s in intagent.\n\nWrote:\n", root)
	for _, d := range done {
		fmt.Fprintf(a.Out, "  %s\n", d)
	}
	fmt.Fprintf(a.Out, "\nNext:\n  1. Commit these files so every member's agents connect.\n  2. Each member runs: intagent login --url %s\n  3. Check the setup with: intagent doctor\n", rc.URL)
	if _, err := exec.LookPath("intagent"); err != nil {
		fmt.Fprintln(a.Out, "\nNote: 'intagent' is not on your PATH. Agents run it by name, so install it there (go install github.com/patakil/intagent/cmd/intagent@latest).")
	}
	if strings.Contains(*agents, "codex") && !*trust {
		fmt.Fprintln(a.Out, "\nCodex runs project hooks only for trusted projects and trusted hooks. Run 'intagent init --trust-codex' or approve them in Codex's /hooks screen.")
	}
	if strings.Contains(*agents, "claude") {
		fmt.Fprintln(a.Out, "\nClaude Code asks once per member to approve the project's MCP server; accept 'intagent' when it does.")
	}
	if strings.Contains(*agents, "gemini") {
		fmt.Fprintln(a.Out, "\nGemini CLI runs a project's hooks only in folders you trust; trust this one when Gemini asks.")
	}
	return nil
}

// trustCodexFor trusts the project in Codex. Codex reads hooks for linked
// worktrees from the main checkout, so both roots are trusted.
func (a *App) trustCodexFor(ctx context.Context, wt *gitx.Worktree) ([]string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(h, ".codex")
	}
	roots := []string{wt.Root}
	if main, err := wt.MainRoot(ctx); err == nil && main != wt.Root {
		roots = append(roots, main)
	}
	var hookFiles []string
	for _, r := range roots {
		p := filepath.Join(r, ".codex", "hooks.json")
		if _, err := os.Stat(p); err == nil {
			hookFiles = append(hookFiles, p)
		}
	}
	return trustCodex(filepath.Join(home, "config.toml"), roots, hookFiles)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
