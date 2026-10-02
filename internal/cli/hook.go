package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/hook"
)

// maxHookInput caps the hook payload read from stdin.
const maxHookInput = 32 << 20

// hook handles one agent hook event. It never blocks an agent because of its
// own failure: any error is logged and the hook exits 0 with no output, unless
// INTAGENT_FAIL=closed asks for edits to be refused while the server is unreachable.
func (a *App) hook(ctx context.Context, args []string) error {
	fs := a.flags("hook <claude-code|codex|cursor>", "hook <agent> < event.json")
	agentFlag := fs.String("agent", "", "the agent (alternative to the positional argument)")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	name := *agentFlag
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	ad, err := hook.For(name)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(a.In, maxHookInput))
	if err != nil {
		hookLog("read stdin: %v", err)
		return nil
	}
	ev, err := ad.Parse(data)
	if err != nil {
		hookLog("%v", err)
		return nil
	}
	if ev.Skip {
		return nil
	}
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
	if err != nil {
		return hook.Output{}, nil // not a git repository: nothing to coordinate
	}
	if !ws.settings.Enrolled || !ws.settings.Ready() {
		return hook.Output{}, nil // repository not enrolled, or member not logged in
	}
	refs := ws.refs(cwd, ev.Paths)
	if (ev.Kind == board.KindPreEdit || ev.Kind == board.KindPostEdit) && len(refs) == 0 {
		return hook.Output{}, nil // writes outside the repository
	}
	hev := board.HookEvent{
		Kind: ev.Kind, Agent: ad.Agent(), SessionID: ev.SessionID, Where: ws.where, Tool: ev.Tool, Paths: refs,
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
	if err != nil {
		if ws.settings.FailClosed && ev.Kind == board.KindPreEdit {
			res = board.HookResult{Decision: board.Refuse, Reason: fmt.Sprintf(
				"[intagent] The team's intagent server at %s did not answer, and this repository is set to refuse edits until it does. Tell your user. (%v)", ws.settings.URL, err)}
			return ad.Render(ev, res), err
		}
		return hook.Output{}, err
	}
	return ad.Render(ev, res), nil
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
