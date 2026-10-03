package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/gitx"
	"github.com/patakil/intagent/internal/hook"
)

// maxHookInput caps the hook payload read from stdin.
const maxHookInput = 32 << 20

// hookBudget bounds one hook run, git included. It stays under the 10 seconds
// intagent init gives each hook, because an agent that kills a slow hook lets
// the edit through, even under INTAGENT_FAIL=closed.
var hookBudget = 8 * time.Second

// hook handles one agent hook event. It never blocks an agent because of its
// own failure: any error is logged and the hook exits 0 with no output, unless
// INTAGENT_FAIL=closed asks for edits to be refused while the server is unreachable.
func (a *App) hook(ctx context.Context, args []string) (err error) {
	// A crash must not become a refusal: Go exits 2 on a panic, and exit 2
	// refuses the tool call in Claude Code, Cursor, Copilot CLI and Codex.
	defer func() {
		if r := recover(); r != nil {
			hookLog("panic: %v\n%s", r, debug.Stack())
			err = nil
		}
	}()
	fs := a.flags("hook", "hook [claude-code|codex|cursor|copilot|gemini] < event.json")
	agentFlag := fs.String("agent", "", "the agent (alternative to the positional argument)")
	userLevel := fs.Bool("user", false, "installed in the user's settings: stand aside where the project wires this agent itself")
	if err := parse(fs, args); err != nil {
		return err
	}
	name := *agentFlag
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	data, err := io.ReadAll(io.LimitReader(a.In, maxHookInput))
	if err != nil {
		hookLog("read stdin: %v", err)
		return nil
	}
	ad, err := hook.Detect(name, data, os.Getenv)
	if err != nil {
		hookLog("%v", err)
		return nil // a misconfigured hook must never block an agent
	}
	ev, err := ad.Parse(data)
	if err != nil {
		hookLog("%v", err)
		return nil
	}
	if ev.Skip || (*userLevel && projectWires(ad.Agent(), ev.Cwd)) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()
	out, err := a.handleHook(ctx, ad, ev)
	if err != nil {
		hookLog("%s %s: %v", ad.Agent(), ev.Name, err)
	}
	if len(out.Stdout) > 0 {
		_, _ = a.Out.Write(out.Stdout)
	}
	if out.Exit != 0 {
		return exitError{code: out.Exit}
	}
	return nil
}

func (a *App) handleHook(ctx context.Context, ad hook.Adapter, ev hook.Event) (hook.Output, error) {
	cwd := ev.Cwd
	if cwd == "" {
		var err error
		if cwd, err = a.workdir(); err != nil {
			return hook.Output{}, err
		}
	}
	ws, err := openWorkspace(ctx, cwd)
	switch {
	case errors.Is(err, gitx.ErrNotRepo):
		return hook.Output{}, nil // not a git repository: nothing to coordinate
	case err != nil:
		// A broken .intagent.json or a failing git: say so in the hook log, and
		// under fail-closed refuse edits rather than pass them unchecked.
		if ev.Kind == board.KindPreEdit && os.Getenv("INTAGENT_FAIL") == "closed" {
			return ad.Render(ev, board.HookResult{Decision: board.DecisionRefuse, Reason: fmt.Sprintf("[intagent] intagent could not read "+
				"this repository's setup (%v), and INTAGENT_FAIL=closed is set in your user's environment, so edits are refused "+
				"until it is fixed. Tell your user.", err)}), err
		}
		return hook.Output{}, err
	}
	if !ws.settings.Enrolled || ws.settings.Disabled {
		return hook.Output{}, nil // not a team repository, or switched off on purpose
	}
	refs := ws.refs(cwd, ev.Paths)
	if (ev.Kind == board.KindPreEdit || ev.Kind == board.KindPostEdit) && len(refs) == 0 {
		return hook.Output{}, nil // writes outside the repository
	}
	login := "intagent login --url " + ws.settings.URL
	if ws.settings.Token == "" {
		return offBoard(ad, ev, ws, "this computer is not signed in to "+ws.settings.URL, "run: "+login), nil
	}
	hev := board.HookEvent{
		Kind: ev.Kind, Agent: ad.Agent(), SessionID: ev.SessionID, Where: ws.where, Tool: ev.Tool, ToolUseID: ev.ToolUseID, Paths: refs,
		LateContext: ev.LateContext, NoAsk: ev.NoAsk,
	}
	if ws.settings.SharePrompts {
		hev.Prompt = ev.Prompt
	}
	ctx, cancel := context.WithTimeout(ctx, ws.settings.Timeout+2*time.Second)
	defer cancel()
	if ev.Footprint && (ev.Kind != board.KindToolEnd || footprintDue(ws.wt.Root, ev.SessionID)) {
		fp, err := ws.footprint(ctx)
		if err != nil {
			hookLog("footprint: %v", err)
		} else {
			hev.Footprint = fp
		}
	}
	res, err := ws.client.Hook(ctx, hev)
	switch {
	case client.IsUnauthorized(err):
		return offBoard(ad, ev, ws, ws.settings.URL+" rejected this computer's token (it may have been rotated)",
			"get a new token from whoever runs the server, then run: "+login), err
	case err != nil:
		if ws.settings.FailClosed && ev.Kind == board.KindPreEdit {
			res = board.HookResult{Decision: board.DecisionRefuse, Reason: fmt.Sprintf("[intagent] The team's intagent server at %s did not "+
				"answer, and INTAGENT_FAIL=closed is set in your user's environment, so edits are refused until it does. "+
				"Tell your user. (%v)", ws.settings.URL, err)}
			return ad.Render(ev, res), err
		}
		return hook.Output{}, err
	}
	return ad.Render(ev, res), nil
}

// offBoard answers when the repository is a team's but this member cannot
// reach the board: the agent hears it once, at the start of its session, so
// its person can fix it; under INTAGENT_FAIL=closed its edits are refused.
func offBoard(ad hook.Adapter, ev hook.Event, ws *workspace, why, fix string) hook.Output {
	switch {
	case ev.Kind == board.KindSessionStart:
		return ad.Render(ev, board.HookResult{Context: fmt.Sprintf("[intagent] This repository uses intagent, but %s, so "+
			"teammates cannot see this session and you will not hear about their work. Tell your user to %s.", why, fix)})
	case ev.Kind == board.KindPreEdit && ws.settings.FailClosed:
		return ad.Render(ev, board.HookResult{Decision: board.DecisionRefuse, Reason: fmt.Sprintf("[intagent] %s, and "+
			"INTAGENT_FAIL=closed is set in your user's environment, so edits are refused until this is fixed. "+
			"Tell your user to %s.", capitalize(why), fix)})
	}
	return hook.Output{}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// footprintEvery limits how often a shell command triggers a footprint scan,
// which on a very large monorepo can take a noticeable fraction of a second.
const footprintEvery = 15 * time.Second

// footprintDue reports whether a session's worktree is due for a scan after a
// shell command, and records that one is happening now.
func footprintDue(root, session string) bool {
	dir, err := os.UserCacheDir()
	if err != nil {
		return true
	}
	sum := sha256.Sum256([]byte(root + "\x00" + session))
	stamp := filepath.Join(dir, "intagent", "scan-"+hex.EncodeToString(sum[:8]))
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < footprintEvery {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err == nil {
		_ = os.WriteFile(stamp, nil, 0o600)
	}
	return true
}

// hookLog appends to intagent's hook log, since a hook's stdout belongs to the
// agent and its stderr is rarely seen.
func hookLog(format string, args ...any) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "intagent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	p := filepath.Join(dir, "hook.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.New(f, "", log.LstdFlags).Printf(format, args...)
	_ = f.Close() // nothing useful to do if the log cannot be written
}

// projectWires reports whether the project at dir wires an agent's hooks
// itself, where that agent reads them, so a user-level copy need not run.
func projectWires(agent board.Agent, dir string) bool {
	if agent != board.AgentGemini || dir == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	return err == nil && strings.Contains(string(data), "intagent hook gemini")
}
