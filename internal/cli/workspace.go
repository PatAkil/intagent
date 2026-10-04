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
	wt.Cache = cacheDir()
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

// choose picks what a footprint keeps of more than limit files, which are
// sorted, and the directories it reports for the files it leaves out. It
// keeps one file of each area that changed, so that a teammate's edit
// anywhere near this work is at least warned about; then the files the
// branch has not committed, the work in progress, a new package included;
// then the branch's commits; then the files of directories the worktree
// added whole that are too large to be work (see bulkShare), the smallest
// first. Each group goes in path order, and the server keeps the first
// files it can take, so they go in that order. It fills the cap, and every
// directory added whole whose files are not all kept goes in dirs.
func (w *workspace) choose(ctx context.Context, files []string, base string, limit int) (kept, dirs []string) {
	added := w.addedDirs(ctx, files)
	bulk := make([]bool, len(files))
	var bulkDirs []addedDir
	for _, d := range added {
		if d.size() > limit/bulkShare {
			bulkDirs = append(bulkDirs, d)
			for i := d.lo; i < d.hi; i++ {
				bulk[i] = true
			}
		}
	}
	committed := map[string]bool{}
	if list, err := w.wt.Committed(ctx, base); err == nil {
		for _, f := range list {
			committed[f] = true
		}
	}
	// ordered holds indexes into files.
	ordered := make([]int, 0, len(files))
	for _, inCommits := range []bool{false, true} {
		for i, f := range files {
			if !bulk[i] && committed[f] == inCommits {
				ordered = append(ordered, i)
			}
		}
	}
	work := len(ordered)
	sort.Slice(bulkDirs, func(i, j int) bool {
		if n, m := bulkDirs[i].size(), bulkDirs[j].size(); n != m {
			return n < m
		}
		return bulkDirs[i].path < bulkDirs[j].path
	})
	for _, d := range bulkDirs {
		for i := d.lo; i < d.hi; i++ {
			ordered = append(ordered, i)
		}
	}
	// One file of each area, looked for outside the bulk directories: dirs
	// stands for those.
	taken := make([]bool, len(files))
	kept = make([]string, 0, limit)
	seen := map[string]bool{}
	for n, i := range ordered[:work] {
		if len(kept) == limit || n == maxAreaScan || ctx.Err() != nil {
			break
		}
		if a := w.areas.Of(files[i]); !seen[a] {
			seen[a], taken[i] = true, true
			kept = append(kept, files[i])
		}
	}
	for _, i := range ordered {
		if len(kept) == limit {
			break
		}
		if !taken[i] {
			taken[i] = true
			kept = append(kept, files[i])
		}
	}
	return kept, leftOut(added, taken)
}

// bulkShare sets the size from which a directory the worktree added whole is
// bulk rather than work in progress: more than one in bulkShare of the files
// a footprint keeps. A new package is work; a .venv or build output nobody
// ignored, whose thousands of files only this worktree has, is bulk, and
// goes after the branch's commits.
const bulkShare = 4

// addedDir is a directory the worktree added whole, whose files are
// files[lo:hi] of the sorted list they are in.
type addedDir struct {
	path   string
	lo, hi int
}

func (d addedDir) size() int { return d.hi - d.lo }

// addedDirs finds, among sorted files, the directories the worktree added
// whole, in path order.
func (w *workspace) addedDirs(ctx context.Context, files []string) []addedDir {
	untracked, err := w.wt.UntrackedDirs(ctx)
	if err != nil {
		return nil
	}
	var dirs []addedDir
	for _, d := range untracked {
		// Every path under d/ sorts between d+"/" and d+"0", '0' following '/'.
		lo, hi := sort.SearchStrings(files, d+"/"), sort.SearchStrings(files, d+"0")
		if hi > lo {
			dirs = append(dirs, addedDir{d, lo, hi})
		}
	}
	return dirs
}

// leftOut returns, sorted, the directories of added with files not taken: at
// most maxFootprintDirs, those that leave the most out first.
func leftOut(added []addedDir, taken []bool) []string {
	type count struct {
		path string
		n    int
	}
	var out []count
	for _, d := range added {
		n := 0
		for i := d.lo; i < d.hi; i++ {
			if !taken[i] {
				n++
			}
		}
		if n > 0 {
			out = append(out, count{d.path, n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].path < out[j].path
	})
	var dirs []string
	for _, c := range out[:min(len(out), maxFootprintDirs)] {
		dirs = append(dirs, c.path)
	}
	sort.Strings(dirs)
	return dirs
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
