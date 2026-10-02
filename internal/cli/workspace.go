package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/gitx"
	"github.com/patakil/intagent/internal/glob"
)

// maxFootprint caps the changed files a client reports.
const maxFootprint = 2000

// workspace is a worktree together with the settings and server it uses.
type workspace struct {
	wt       *gitx.Worktree
	settings client.Settings
	client   *client.Client
	where    board.Where
	areas    *gitx.Areas
}

func hostname() string {
	if h := os.Getenv("INTAGENT_HOST"); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-host"
}

// openWorkspace resolves dir to a worktree and its settings. It does not
// require the settings to be complete; callers check settings.Ready.
func openWorkspace(ctx context.Context, dir string) (*workspace, error) {
	wt, err := gitx.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	st, err := client.Resolve(wt.Root)
	if err != nil {
		return nil, err
	}
	repo := st.Repo.Repo
	if repo == "" {
		repo = wt.RepoID()
	}
	w := &workspace{
		wt:       wt,
		settings: st,
		where:    board.Where{Repo: repo, Host: hostname(), Worktree: wt.Root, Branch: wt.Branch},
		areas:    gitx.NewAreas(wt.Root, st.Repo.Areas),
	}
	if st.Ready() {
		w.client = client.New(st.URL, st.Token, st.Timeout)
	}
	return w, nil
}

// errNotConnected explains how to connect when settings are incomplete.
func (w *workspace) errNotConnected() error {
	s := w.settings
	switch {
	case s.Disabled:
		return errors.New("intagent is disabled by INTAGENT_DISABLE")
	case s.URL == "":
		return errors.New("no server configured for this repository: run 'intagent init --url <server>' or set INTAGENT_URL")
	case s.Token == "":
		return fmt.Errorf("no token for %s: run 'intagent login --url %s'", s.URL, s.URL)
	}
	return nil
}

// requireConnection reports why the workspace cannot reach a server, if it cannot.
func (w *workspace) requireConnection() error {
	if w.client == nil {
		return w.errNotConnected()
	}
	return nil
}

// refs turns paths (absolute, or relative to base) into repo-relative path
// references, dropping paths outside the worktree and ignored paths.
func (w *workspace) refs(base string, paths []string) []board.PathRef {
	var out []board.PathRef
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		rel, ok := w.wt.Rel(p)
		if !ok || w.ignored(rel) {
			continue
		}
		out = append(out, board.PathRef{Path: rel, Area: w.areas.Of(rel)})
	}
	return out
}

func (w *workspace) ignored(rel string) bool {
	for _, pat := range w.settings.Repo.Ignore {
		if c, err := glob.CleanPattern(pat); err == nil && glob.Match(c, rel) {
			return true
		}
	}
	return false
}

// footprint reports the worktree's changes against the default branch.
func (w *workspace) footprint(ctx context.Context) (*board.Footprint, error) {
	files, truncated, err := w.wt.Changes(ctx, maxFootprint)
	if err != nil {
		return nil, err
	}
	fp := &board.Footprint{Files: make([]board.PathRef, 0, len(files)), Truncated: truncated}
	for _, f := range files {
		if w.ignored(f) {
			continue
		}
		fp.Files = append(fp.Files, board.PathRef{Path: f, Area: w.areas.Of(f)})
	}
	return fp, nil
}
