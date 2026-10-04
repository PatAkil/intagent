package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/gitx"
	"github.com/patakil/intagent/internal/glob"
)

// maxFootprint caps the changed files a client reports.
const maxFootprint = 2000

// maxFootprintDirs caps the directories a footprint reports in place of
// their files.
const maxFootprintDirs = 200

// maxAreaScan caps the files whose areas a footprint over maxFootprint looks
// up to choose one file of each area.
const maxAreaScan = 20000

// workspace is a worktree together with the settings and server it uses.
type workspace struct {
	wt       *gitx.Worktree
	settings client.Settings
	client   *client.Client
	where    board.Where
	areas    *gitx.Areas
	// ignore holds the repository's ignore patterns, cleaned once.
	ignore []string
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
	for _, pat := range st.Repo.Ignore {
		if c, err := glob.CleanPattern(pat); err == nil {
			w.ignore = append(w.ignore, c)
		}
	}
	if st.Ready() {
		w.client = client.New(st.URL, st.Token, st.Timeout)
	}
	return w, nil
}

// enrolledRoot finds, without git, the root of the worktree containing dir,
// and returns it if a team enrolled the repository; "" otherwise. When the
// workspace cannot be opened, it tells a team repository from any other.
func enrolledRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			if _, err := os.Stat(filepath.Join(d, client.RepoFileName)); err == nil {
				return d
			}
			return ""
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// within reports whether any of paths, relative to base unless absolute, is
// inside root, following symbolic links as the worktree's own check does.
func within(root, base string, paths []string) bool {
	if root == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		p = gitx.ResolveExisting(filepath.Clean(p))
		if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
	for _, pat := range w.ignore {
		if glob.Match(pat, rel) {
			return true
		}
	}
	return false
}

// footprint reports the worktree's changes against the default branch: the
// changed files the repository does not ignore, with their areas, and if
// they are more than maxFootprint, what choose keeps of them.
func (w *workspace) footprint(ctx context.Context) (*board.Footprint, error) {
	return w.footprintUpTo(ctx, maxFootprint)
}

// footprintUpTo is footprint with a cap of limit files.
func (w *workspace) footprintUpTo(ctx context.Context, limit int) (*board.Footprint, error) {
	all, base, err := w.wt.Changes(ctx)
	if err != nil {
		return nil, err
	}
	// Ignored files go before the cap, so generated files cannot crowd the
	// real changes out of it.
	files := make([]string, 0, len(all))
	for _, f := range all {
		if !w.ignored(f) {
			files = append(files, f)
		}
	}
	if len(files) <= limit {
		return &board.Footprint{Files: w.pathRefs(ctx, files)}, nil
	}
	files, dirs := w.choose(ctx, files, base, limit)
	fp := &board.Footprint{Files: w.pathRefs(ctx, files), Truncated: true}
	for _, d := range dirs {
		ref := board.PathRef{Path: d}
		if ctx.Err() == nil {
			ref.Area = w.areas.OfDir(d)
		}
		fp.Dirs = append(fp.Dirs, ref)
	}
	return fp, nil
}

// choose picks what a footprint keeps of more than limit files, sorted.
// First, directories the worktree added whole each stand for their files,
// the largest first, until the files fit: a .venv or build output nobody
// ignored, whose files only this worktree has. If the files still do not
// fit, it keeps one file of each area that changed, so that a teammate's
// edit anywhere near this work is at least warned about; then the files the
// branch has not committed, the work in progress; then the rest; each in
// path order. The server keeps the first files it can take, so they go in
// that order.
func (w *workspace) choose(ctx context.Context, files []string, base string, limit int) (kept, dirs []string) {
	files, dirs = w.collapse(ctx, files, limit)
	if len(files) <= limit {
		return files, dirs
	}
	committed := map[string]bool{}
	if list, err := w.wt.Committed(ctx, base); err == nil {
		for _, f := range list {
			committed[f] = true
		}
	}
	ordered := make([]string, 0, len(files))
	for _, f := range files {
		if !committed[f] {
			ordered = append(ordered, f)
		}
	}
	for _, f := range files {
		if committed[f] {
			ordered = append(ordered, f)
		}
	}
	taken := make(map[string]bool, limit)
	kept = make([]string, 0, limit)
	seen := map[string]bool{}
	for i, f := range ordered {
		if len(kept) == limit || i == maxAreaScan || ctx.Err() != nil {
			break
		}
		if a := w.areas.Of(f); !seen[a] {
			seen[a], taken[f] = true, true
			kept = append(kept, f)
		}
	}
	for _, f := range ordered {
		if len(kept) == limit {
			break
		}
		if !taken[f] {
			kept = append(kept, f)
		}
	}
	return kept, dirs
}

// collapse replaces the files of directories the worktree added whole with
// the directories, the largest first, until at most limit files are left.
func (w *workspace) collapse(ctx context.Context, files []string, limit int) (rest, dirs []string) {
	untracked, err := w.wt.UntrackedDirs(ctx)
	if err != nil {
		return files, nil
	}
	type dir struct {
		path   string
		lo, hi int // its files are files[lo:hi]
	}
	var cands []dir
	for _, d := range untracked {
		// Every path under d/ sorts between d+"/" and d+"0", '0' following '/'.
		lo, hi := sort.SearchStrings(files, d+"/"), sort.SearchStrings(files, d+"0")
		if hi > lo {
			cands = append(cands, dir{d, lo, hi})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if n, m := cands[i].hi-cands[i].lo, cands[j].hi-cands[j].lo; n != m {
			return n > m
		}
		return cands[i].path < cands[j].path
	})
	drop := make([]bool, len(files))
	left := len(files)
	for _, c := range cands {
		if left <= limit || len(dirs) == maxFootprintDirs {
			break
		}
		dirs = append(dirs, c.path)
		left -= c.hi - c.lo
		for i := c.lo; i < c.hi; i++ {
			drop[i] = true
		}
	}
	if len(dirs) == 0 {
		return files, nil
	}
	rest = make([]string, 0, left)
	for i, f := range files {
		if !drop[i] {
			rest = append(rest, f)
		}
	}
	sort.Strings(dirs)
	return rest, dirs
}

// pathRefs gives files their areas while ctx lasts, which is git's time: the
// files left when it ends go without one, which the server reads as an area
// it does not know, rather than not at all.
func (w *workspace) pathRefs(ctx context.Context, files []string) []board.PathRef {
	refs := make([]board.PathRef, len(files))
	for i, f := range files {
		refs[i].Path = f
		if ctx.Err() == nil {
			refs[i].Area = w.areas.Of(f)
		}
	}
	return refs
}
