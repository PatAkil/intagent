package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/gitx"
)

// watch keeps a worktree on the board for agents that have no hooks, or for
// people working by hand: it reports the git footprint on an interval and
// prints whatever the team's agents should know.
func (a *App) watch(ctx context.Context, args []string) error {
	fs := a.flags("watch", "watch [--interval 20s] [--agent <name>]")
	every := fs.Duration("interval", 20*time.Second, "how often to report")
	agent := fs.String("agent", string(board.AgentWatch), "what to call this session on the board")
	once := fs.Bool("once", false, "report once and exit")
	if err := parse(fs, args); err != nil {
		return err
	}
	ws, _, err := a.here(ctx)
	if err != nil {
		return err
	}
	session := fmt.Sprintf("watch-%s-%d", hostname(), os.Getpid())
	send := func(ctx context.Context, kind board.Kind) error {
		ev := board.HookEvent{Kind: kind, Agent: board.Agent(*agent), SessionID: session, Where: ws.where}
		if fp, err := ws.footprint(ctx); err == nil {
			ev.Footprint = fp
		}
		res, err := ws.client.Hook(ctx, ev)
		if err != nil {
			return err
		}
		if res.Context != "" {
			fmt.Fprintln(a.Out, strings.TrimSpace(res.Context))
		}
		return nil
	}
	if err := send(ctx, board.KindSessionStart); err != nil {
		return err
	}
	if *once {
		// Report the footprint and leave: the board keeps the work, but no
		// session that looks like it is still running.
		return send(ctx, board.KindSessionEnd)
	}
	fmt.Fprintf(a.Err, "Reporting %s to %s every %s. Ctrl-C to stop.\n", ws.wt.Root, ws.settings.URL, *every)
	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			end, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return send(end, board.KindSessionEnd)
		case <-t.C:
			if err := send(ctx, board.KindHeartbeat); err != nil {
				fmt.Fprintf(a.Err, "intagent watch: %v\n", err)
			}
		}
	}
}

// guard is a git pre-commit check: it refuses a commit that touches paths
// another member's active claim holds exclusively, and warns about overlaps.
// It never blocks a commit because the server is unreachable.
func (a *App) guard(ctx context.Context, args []string) error {
	fs := a.flags("guard", "guard")
	if err := parse(fs, args); err != nil {
		return err
	}
	dir, err := a.workdir()
	if err != nil {
		return err
	}
	ws, err := openWorkspace(ctx, dir)
	switch {
	case errors.Is(err, gitx.ErrNotRepo):
		return nil
	case err != nil:
		if os.Getenv("INTAGENT_FAIL") == "closed" {
			return exitError{code: 1, msg: fmt.Sprintf("intagent guard: %v; INTAGENT_FAIL=closed is set, so the commit is refused.", err)}
		}
		fmt.Fprintf(a.Err, "intagent guard: %v, so this commit was not checked against teammates' work.\n", err)
		return nil
	}
	if !ws.settings.Enrolled || ws.settings.Disabled {
		return nil
	}
	staged, err := ws.wt.Staged(ctx)
	if err != nil || len(staged) == 0 {
		return nil
	}
	// Unchecked is said out loud, and refused under INTAGENT_FAIL=closed.
	unchecked := func(why string) error {
		msg := fmt.Sprintf("intagent guard: %s, so this commit was not checked against teammates' work.", why)
		if ws.settings.FailClosed {
			return exitError{code: 1, msg: msg + " INTAGENT_FAIL=closed is set, so the commit is refused."}
		}
		fmt.Fprintln(a.Err, msg)
		return nil
	}
	if ws.settings.Token == "" {
		return unchecked("not signed in to " + ws.settings.URL + " (run: intagent login --url " + ws.settings.URL + ")")
	}
	refs := ws.refs(ws.wt.Root, staged)
	ctx, cancel := context.WithTimeout(ctx, ws.settings.Timeout)
	defer cancel()
	res, err := ws.client.Check(ctx, board.CheckRequest{Where: ws.where, Paths: refs})
	switch {
	case client.IsUnauthorized(err):
		return unchecked(ws.settings.URL + " rejected your token (rotated?); get a new one and run: intagent login --url " + ws.settings.URL)
	case err != nil:
		return unchecked(fmt.Sprintf("%s did not answer (%v)", ws.settings.URL, err))
	}
	var blocked, overlap []board.Conflict
	for _, c := range res.Conflicts {
		switch {
		case c.Severity == board.Block && !c.SameClaim:
			blocked = append(blocked, c)
		case c.Severity == board.Overlap && !c.SameClaim:
			overlap = append(overlap, c)
		}
	}
	if len(overlap) > 0 {
		fmt.Fprintln(a.Err, "intagent: these files are also changed by teammates:")
		for _, c := range overlap {
			fmt.Fprintf(a.Err, "  %s: %s (%s)\n", c.Path, c.Member, c.Why)
		}
	}
	if len(blocked) > 0 {
		var b strings.Builder
		b.WriteString("intagent: commit refused; these files are reserved by an active teammate:\n")
		for _, c := range blocked {
			fmt.Fprintf(&b, "  %s: %s (%s)\n", c.Path, c.Member, c.Why)
		}
		b.WriteString("If you are an agent: do not bypass this; tell your user. A person who has agreed it with them can " +
			"commit with 'git commit --no-verify'.")
		return exitError{code: 1, msg: b.String()}
	}
	return nil
}
