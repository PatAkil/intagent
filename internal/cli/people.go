package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
)

func (a *App) here(ctx context.Context) (*workspace, string, error) {
	dir, err := a.workdir()
	if err != nil {
		return nil, "", err
	}
	ws, err := openWorkspace(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	if err := ws.requireConnection(); err != nil {
		return nil, "", err
	}
	return ws, dir, nil
}

func (a *App) board(ctx context.Context, args []string) error {
	fs := a.flags("board", "board [--repo <id>] [--json] [--watch <interval>]")
	repo := fs.String("repo", "", "repository id (default: this repository)")
	asJSON := fs.Bool("json", false, "print the board as JSON")
	every := fs.Duration("watch", 0, "redraw at this interval, e.g. 5s")
	if err := parse(fs, args); err != nil {
		return err
	}
	ws, _, err := a.here(ctx)
	if err != nil {
		return err
	}
	if *repo == "" {
		*repo = ws.where.Repo
	}
	for {
		if *asJSON {
			v, err := ws.client.Board(ctx, *repo)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(a.Out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(v); err != nil {
				return err
			}
		} else {
			text, err := ws.client.BoardText(ctx, *repo)
			if err != nil {
				return err
			}
			if *every > 0 {
				fmt.Fprint(a.Out, "\033[H\033[2J")
			}
			fmt.Fprint(a.Out, text)
		}
		if *every <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}

func (a *App) check(ctx context.Context, args []string) error {
	fs := a.flags("check", "check <path>...")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errUsage
	}
	ws, dir, err := a.here(ctx)
	if err != nil {
		return err
	}
	refs := ws.refs(dir, fs.Args())
	if len(refs) == 0 {
		return fmt.Errorf("none of these paths are inside %s", ws.wt.Root)
	}
	text, err := checkPaths(ctx, ws, refs, a.now())
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Out, text)
	return nil
}

func (a *App) declare(ctx context.Context, args []string) error {
	fs := a.flags("declare", "declare [-x] -m <summary> <glob>...")
	summary := fs.String("m", "", "one line: what you are changing and why (required)")
	exclusive := fs.Bool("x", false, "exclusive: refuse other members' agents while this worktree is active")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 || strings.TrimSpace(*summary) == "" {
		fs.Usage()
		return errUsage
	}
	ws, dir, err := a.here(ctx)
	if err != nil {
		return err
	}
	mode := board.ModeShared
	if *exclusive {
		mode = board.ModeExclusive
	}
	res, err := ws.client.Declare(ctx, board.DeclareRequest{Where: ws.where, Summary: *summary, Patterns: ws.patterns(dir, fs.Args()), Mode: mode})
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Out, res.Text)
	if len(res.Accepted) == 0 {
		return exitError{code: 1}
	}
	return nil
}

func (a *App) release(ctx context.Context, args []string) error {
	fs := a.flags("release", "release [<glob>...]")
	if err := parse(fs, args); err != nil {
		return err
	}
	ws, dir, err := a.here(ctx)
	if err != nil {
		return err
	}
	n, err := ws.client.Release(ctx, board.ReleaseRequest{Where: ws.where, Patterns: ws.patterns(dir, fs.Args())})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Released %d intent(s).\n", n)
	return nil
}

func (a *App) note(ctx context.Context, args []string) error {
	fs := a.flags("note", "note <member|claim|path> <text>...")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return errUsage
	}
	ws, dir, err := a.here(ctx)
	if err != nil {
		return err
	}
	to := fs.Arg(0)
	if refs := ws.refs(dir, []string{to}); strings.ContainsAny(to, "/.") && len(refs) == 1 {
		to = refs[0].Path
	}
	res, err := ws.client.Note(ctx, board.NoteRequest{Where: ws.where, To: to, Text: strings.Join(fs.Args()[1:], " "), ByPerson: true})
	if err != nil {
		return err
	}
	if res.HeldFor != "" {
		fmt.Fprintf(a.Out, "Note held for %s's next session in this repository.\n", res.HeldFor)
		return nil
	}
	fmt.Fprintf(a.Out, "Note queued for %d claim(s): %s\n", len(res.Delivered), strings.Join(res.Delivered, ", "))
	return nil
}
