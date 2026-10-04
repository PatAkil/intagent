package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
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
		scanned := time.Now()
		if fp, err := ws.footprint(ctx); err == nil {
			fp.AgeMS = time.Since(scanned).Milliseconds()
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
		if client.FailClosedByEnv() && !client.DisabledByEnv() && enrolledRoot(dir) != "" {
			return exitError{code: 1, msg: fmt.Sprintf("intagent guard: %v; INTAGENT_FAIL=closed is set, so the commit is refused.", err)}
		}
		fmt.Fprintf(a.Err, "intagent guard: %v, so this commit was not checked against teammates' work.\n", err)
		return nil
	}
	if !ws.settings.Enrolled || ws.settings.Disabled {
		return nil
	}
	// Unchecked is said out loud, and refused under INTAGENT_FAIL=closed.
	unchecked := func(why, what string) error {
		msg := fmt.Sprintf("intagent guard: %s, so %s not checked against teammates' work.", why, what)
		if ws.settings.FailClosed {
			return exitError{code: 1, msg: msg + " INTAGENT_FAIL=closed is set, so the commit is refused."}
		}
		fmt.Fprintln(a.Err, msg)
		return nil
	}
	staged, err := ws.wt.Staged(ctx)
	if err != nil {
		return unchecked(fmt.Sprintf("git could not list the staged files (%v)", err), "this commit was")
	}
	if len(staged) == 0 {
		return nil
	}
	if ws.settings.Token == "" {
		return unchecked("not signed in to "+ws.settings.URL+" (run: intagent login --url "+ws.settings.URL+")", "this commit was")
	}
	refs := ws.refs(ws.wt.Root, staged)
	conflicts, left, err := checkInBatches(ctx, ws, refs)
	var blocked, overlap []board.Conflict
	for _, c := range conflicts {
		switch {
		case c.Severity == board.SeverityBlock && !c.SameClaim:
			blocked = append(blocked, c)
		case c.Severity == board.SeverityOverlap && !c.SameClaim:
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
	why := ""
	switch {
	case client.IsUnauthorized(err):
		why = ws.settings.URL + " rejected your token (rotated?); get a new one and run: intagent login --url " + ws.settings.URL
	case err != nil:
		why = fmt.Sprintf("%s did not answer (%v)", ws.settings.URL, err)
	default:
		return nil
	}
	what := "this commit was"
	if left < len(refs) {
		what = fmt.Sprintf("%d of the %d files in this commit were", left, len(refs))
	}
	return unchecked(why, what)
}

// pathsPerCheck is how many paths one check carries: the server checks at
// most 200 at once and counts the rest as unchecked, and older servers
// checked only the first 200 and said nothing of the rest.
const pathsPerCheck = 200

// maxBusyRetries bounds how often a check is sent again when the server
// says it is busy, as it does when one worktree sends checks faster than it
// takes them.
const maxBusyRetries = 5

// checkInBatches checks refs in batches of pathsPerCheck, each with the
// request timeout to itself, and merges what the server found. It stops at
// the first batch the server does not answer, and reports how many paths
// were left unchecked.
func checkInBatches(ctx context.Context, ws *workspace, refs []board.PathRef) (conflicts []board.Conflict, left int, err error) {
	for start := 0; start < len(refs); start += pathsPerCheck {
		res, err := checkBatch(ctx, ws, refs[start:min(start+pathsPerCheck, len(refs))])
		if err != nil {
			return conflicts, len(refs) - start, err
		}
		conflicts = append(conflicts, res.Conflicts...)
	}
	return conflicts, 0, nil
}

// checkPaths asks who else is working on refs, for 'intagent check' and the
// MCP check_paths tool, and answers in the server's words. More paths than
// one check carries go in several, and what they find is told together, as
// of now.
func checkPaths(ctx context.Context, ws *workspace, refs []board.PathRef, now time.Time) (string, error) {
	if len(refs) <= pathsPerCheck {
		res, err := checkBatch(ctx, ws, refs)
		return res.Text, err
	}
	conflicts, left, err := checkInBatches(ctx, ws, refs)
	if err != nil {
		return "", fmt.Errorf("%d of the %d paths were not checked: %w", left, len(refs), err)
	}
	return board.RenderConflicts(now, conflicts), nil
}

// checkBatch sends one check, again after the wait the server asks for when
// it answers that it is busy.
func checkBatch(ctx context.Context, ws *workspace, batch []board.PathRef) (board.CheckResult, error) {
	for attempt := 0; ; attempt++ {
		bctx, cancel := context.WithTimeout(ctx, ws.settings.Timeout)
		res, err := ws.client.Check(bctx, board.CheckRequest{Where: ws.where, Paths: batch})
		cancel()
		var ae *client.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests || attempt == maxBusyRetries {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(min(cmp.Or(ae.RetryAfter, time.Second), 5*time.Second)):
		}
	}
}
