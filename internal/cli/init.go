package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/fsutil"
	"github.com/patakil/intagent/internal/gitx"
)

// parseAgents reads --agents: known names, each once, in a stable order.
func parseAgents(list string) ([]string, error) {
	want := map[string]bool{}
	for _, ag := range splitList(list) {
		if ag == "claude" {
			ag = "claude-code"
		}
		if !slices.Contains(agentNames, ag) {
			return nil, fmt.Errorf("unknown agent %q: want %s (Copilot CLI uses claude-code's hooks)", ag, strings.Join(agentNames, ", "))
		}
		want[ag] = true
	}
	var out []string
	for _, ag := range agentNames {
		if want[ag] {
			out = append(out, ag)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("--agents names no agent")
	}
	return out, nil
}

func (a *App) initRepo(ctx context.Context, args []string) error {
	fs := a.flags("init", "init [--url <server>] [--agents claude-code,codex,cursor,gemini] [--git-hook] [--user] [--trust-codex]")
	url := fs.String("url", "", "the team server for this repository")
	agentList := fs.String("agents", strings.Join(agentNames, ","), "agents to wire up (Copilot CLI uses claude-code's)")
	gitHook := fs.Bool("git-hook", false, "install a pre-commit hook that runs 'intagent guard'")
	user := fs.Bool("user", false, "also install Claude Code and Gemini CLI hooks in your user settings, for agents started in a subdirectory")
	trust := fs.Bool("trust-codex", false, "mark this project and intagent's hooks as trusted in your ~/.codex/config.toml")
	areas := fs.String("areas", "", "comma-separated globs that define areas, e.g. 'services/*,libs/*'")
	if err := parse(fs, args); err != nil {
		return err
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
	rc, _, err := client.LoadRepoConfig(root)
	if err != nil {
		return err
	}
	// Without --agents, a re-run keeps the agents the team chose.
	if !flagSet(fs, "agents") && len(rc.Agents) > 0 {
		*agentList = strings.Join(rc.Agents, ",")
	}
	agents, err := parseAgents(*agentList)
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
	if err := client.CheckURL(rc.URL); err != nil {
		return err
	}
	if *areas != "" {
		rc.Areas = splitList(*areas)
	}
	rc.Agents = agents
	userFiles, err := userSettings(agents, *user)
	if err != nil {
		return err
	}
	if err := preflight(root, agents, userFiles); err != nil {
		return err
	}

	data, _ := json.MarshalIndent(rc, "", "  ")
	data = append(data, '\n')
	var done []string
	repoFile := filepath.Join(root, client.RepoFileName)
	if old, err := os.ReadFile(repoFile); err != nil || !bytes.Equal(old, data) {
		if err := fsutil.WriteFile(repoFile, data, fsutil.ModeOr(repoFile, 0o644)); err != nil {
			return err
		}
		done = append(done, client.RepoFileName+" (server "+rc.URL+")")
	}
	for _, ag := range agents {
		wrote, err := wiringFor(ag).install(root)
		if err != nil {
			return err
		}
		done = append(done, wrote...)
	}
	repoChanged := len(done) > 0
	for ag, p := range userFiles {
		ok, err := wiringFor(ag).installUser(p)
		if err != nil {
			return err
		}
		if ok {
			done = append(done, p+" (hooks for every enrolled repository, wherever the agent starts)")
		}
	}
	if *trust && slices.Contains(agents, "codex") {
		ct, err := codexTrustFor(ctx, wt)
		if err != nil {
			return err
		}
		notes, err := ct.apply()
		if err != nil {
			return err
		}
		for _, n := range notes {
			done = append(done, ct.config+": "+n)
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
	codexUntrusted := false
	if slices.Contains(agents, "codex") && !*trust {
		if ct, err := codexTrustFor(ctx, wt); err == nil {
			missing, err := ct.problems()
			codexUntrusted = err != nil || len(missing) > 0
		}
	}
	a.initReport(root, rc.URL, agents, done, repoChanged, codexUntrusted)
	return nil
}

// userSettings names the user settings files --user wires, by agent.
func userSettings(agents []string, user bool) (map[string]string, error) {
	out := map[string]string{}
	if !user {
		return out, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	for _, ag := range agents {
		if w := wiringFor(ag); w.userFile != "" {
			out[ag] = filepath.Join(home, filepath.FromSlash(w.userFile))
		}
	}
	return out, nil
}

// preflight reads every file init would edit, so that one it cannot (a file
// with comments, say) stops init before anything is written.
func preflight(root string, agents []string, userFiles map[string]string) error {
	var files []string
	for _, ag := range agents {
		for _, rel := range wiringFor(ag).files {
			files = append(files, filepath.Join(root, filepath.FromSlash(rel)))
		}
	}
	for _, p := range userFiles {
		files = append(files, p)
	}
	for _, p := range files {
		if _, err := readJSONObject(p); err != nil {
			return fmt.Errorf("%w\nNothing was written. intagent edits these files as plain JSON and cannot keep comments: "+
				"remove them, or leave that agent out with --agents", err)
		}
	}
	return nil
}

func (a *App) initReport(root, url string, agents, done []string, repoChanged, codexUntrusted bool) {
	if repoChanged {
		fmt.Fprintf(a.Out, "Enrolled %s in intagent.\n", root)
	} else {
		fmt.Fprintf(a.Out, "%s is already enrolled; nothing in the repository changed.\n", root)
	}
	if len(done) > 0 {
		fmt.Fprintln(a.Out, "\nWrote:")
		for _, d := range done {
			fmt.Fprintf(a.Out, "  %s\n", d)
		}
	}
	if repoChanged {
		fmt.Fprintf(a.Out, "\nNext:\n  1. Commit these files so every member's agents connect.\n  2. Each member runs, in their clone: "+
			"intagent login --url %s, then intagent doctor\n", url)
	}
	if _, err := exec.LookPath("intagent"); err != nil {
		fmt.Fprintln(a.Out, "\nNote: 'intagent' is not on your PATH. Agents run it by name, so install it there (go install github.com/patakil/intagent/cmd/intagent@latest).")
	}
	if codexUntrusted {
		fmt.Fprintln(a.Out, "\nCodex runs project hooks only for trusted projects and trusted hooks. Run 'intagent init --trust-codex' or approve them in Codex's /hooks screen.")
	}
	for _, ag := range agents {
		if note := wiringFor(ag).note; note != "" {
			fmt.Fprintln(a.Out, "\n"+note)
		}
	}
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
