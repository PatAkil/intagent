// Package gitx answers the few questions intagent asks git: which repository
// and worktree a directory belongs to, which branch is checked out, which
// files differ from the default branch, and which area a file belongs to.
package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotRepo reports a directory outside any git worktree.
var ErrNotRepo = errors.New("not inside a git repository")

// Worktree is a checked-out working tree of a repository.
type Worktree struct {
	// Root is the absolute path of the worktree's top directory.
	Root string
	// Branch is the checked-out branch, or "HEAD" when detached.
	Branch string
	// Remote is the origin URL, if any.
	Remote string
	// Cache, if set, is a directory of intagent's own, where Changes keeps
	// the copy of the index git refreshed for the next scan to start from
	// (see indexCopy). Without one, each scan's copy goes in the system's
	// temporary directory and is removed after it.
	Cache string
}

// defaultTimeout bounds a git command when the caller set no deadline.
const defaultTimeout = 10 * time.Second

// stopDelay is how long git has to stop once its deadline has passed, and a
// helper it started to let go of its output, before both are killed.
const stopDelay = 200 * time.Millisecond

// run executes git in dir and returns its stdout.
func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runEnv(ctx, dir, nil, args...)
}

// runEnv is run with env added to git's environment.
func runEnv(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	// At the deadline git is asked to stop rather than killed: it removes its
	// lock files as it stops, while a git killed as it rewrites the index
	// leaves .git/index.lock behind, and that refuses every later git add and
	// commit in the worktree until someone deletes it.
	cmd.Cancel = func() error { return terminate(cmd.Process) }
	cmd.WaitDelay = stopDelay
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Open finds the worktree containing dir.
func Open(ctx context.Context, dir string) (*Worktree, error) {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD")
	if err != nil {
		// A repository without commits has no HEAD to abbreviate.
		top, err2 := run(ctx, dir, "rev-parse", "--show-toplevel")
		switch {
		case err2 != nil && strings.Contains(err2.Error(), "not a git repository"):
			return nil, fmt.Errorf("%w: %s", ErrNotRepo, dir)
		case err2 != nil:
			// git missing, timed out or failing: not the same as "no repository".
			return nil, err2
		}
		out = append(append([]byte{}, top...), "HEAD\n"...)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("%w: %s", ErrNotRepo, dir)
	}
	w := &Worktree{Root: filepath.Clean(lines[0]), Branch: strings.TrimSpace(lines[1])}
	if remote, err := run(ctx, w.Root, "config", "--get", "remote.origin.url"); err == nil {
		w.Remote = strings.TrimSpace(string(remote))
	}
	return w, nil
}

// RepoID names the repository the same way on every member's machine: the
// normalised origin URL, or "local/<directory name>" without a remote.
func (w *Worktree) RepoID() string {
	if id := NormalizeRemote(w.Remote); id != "" {
		return id
	}
	return "local/" + strings.ToLower(filepath.Base(w.Root))
}

// NormalizeRemote turns any form of a remote URL into host/path, lower-case,
// without credentials, port or a trailing ".git":
//
//	git@github.com:Acme/Mono.git           -> github.com/acme/mono
//	https://user@github.com/acme/mono      -> github.com/acme/mono
//	ssh://git@gitlab.example.com:22/a/b.git -> gitlab.example.com/a/b
func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var host, p string
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		host, p = u.Hostname(), u.Path
	} else if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		// scp-like syntax: [user@]host:path
		host, p = raw[:i], raw[i+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	} else {
		// A local path used as a remote.
		host, p = "file", raw
	}
	p = strings.TrimSuffix(strings.Trim(filepath.ToSlash(p), "/"), ".git")
	p = path.Clean("/" + p)[1:]
	if p == "" {
		return ""
	}
	return strings.ToLower(host + "/" + p)
}

// Rel converts an absolute path to a slash-separated path relative to the
// worktree root. It reports false for paths outside the worktree.
func (w *Worktree) Rel(abs string) (string, bool) {
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.Root, abs)
	}
	root := w.Root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root, abs = r, ResolveExisting(abs)
	}
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return "", false
	}
	return rel, true
}

// ResolveExisting resolves symlinks in the longest existing prefix of p, so a
// file that does not exist yet still resolves through a symlinked directory.
func ResolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// DefaultBranch returns the ref the worktree's work will merge into.
func (w *Worktree) DefaultBranch(ctx context.Context) (string, error) {
	if out, err := run(ctx, w.Root, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref, nil
		}
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := run(ctx, w.Root, "rev-parse", "--verify", "-q", ref+"^{commit}"); err == nil {
			return ref, nil
		}
	}
	return "", errors.New("no default branch found")
}

// Changes lists every file that differs from the default branch, sorted:
// committed changes since the merge base, uncommitted changes, and untracked
// files. It also returns the commit it compared with: the merge base, or
// "HEAD" when there is no default branch to compare with.
//
// The diff runs on a private copy of the index (see indexCopy), and git
// ls-files honours GIT_OPTIONAL_LOCKS=0, so neither takes .git/index.lock.
func (w *Worktree) Changes(ctx context.Context) ([]string, string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(list []byte) {
		for _, p := range bytes.Split(list, []byte{0}) {
			if len(p) > 0 && !seen[string(p)] {
				seen[string(p)] = true
				files = append(files, string(p))
			}
		}
	}
	base := ""
	if def, err := w.DefaultBranch(ctx); err == nil {
		if out, err := run(ctx, w.Root, "merge-base", "HEAD", def); err == nil {
			base = strings.TrimSpace(string(out))
		}
	}
	if base == "" {
		base = "HEAD" // no default branch to compare with: uncommitted work only
	}
	// The worktree against the base, committed or not: a change made on the
	// branch and then undone is no change at all.
	tracked, err := w.diff(ctx, base)
	if err != nil {
		if base != "HEAD" {
			return nil, "", err
		}
		// No commits yet: everything in the index is new.
		if tracked, err = run(ctx, w.Root, "ls-files", "-z"); err != nil {
			return nil, "", err
		}
	}
	add(tracked)
	untracked, err := run(ctx, w.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, "", err
	}
	add(untracked)
	sort.Strings(files)
	return files, base, nil
}

// Committed lists the files the branch's own commits changed since base, as
// Changes returned it: a tree-to-tree diff, which reads neither the working
// tree nor the index. With no default branch to compare with ("HEAD") there
// are none.
func (w *Worktree) Committed(ctx context.Context, base string) ([]string, error) {
	if base == "" || base == "HEAD" {
		return nil, nil
	}
	out, err := run(ctx, w.Root, "diff", "--name-only", "--no-renames", "-z", base, "HEAD", "--")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// UntrackedDirs lists the directories the worktree added whole: untracked
// directories holding files git does not ignore, the outermost only, without
// a trailing slash. Changes lists their files one by one.
func (w *Worktree) UntrackedDirs(ctx context.Context) ([]string, error) {
	out, err := run(ctx, w.Root, "ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, p := range splitNul(out) {
		if d, ok := strings.CutSuffix(p, "/"); ok && d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
}

// splitNul splits git's -z output.
func splitNul(out []byte) []string {
	var list []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			list = append(list, string(p))
		}
	}
	return list
}

// diff lists the files that differ between base and the worktree, on a
// private copy of the index.
func (w *Worktree) diff(ctx context.Context, base string) ([]byte, error) {
	args := []string{"diff", "--name-only", "--no-renames", "-z", base, "--"}
	c := w.privateIndex(ctx, true)
	out, err := runEnv(ctx, w.Root, c.env(), args...)
	if err != nil && c.reused() && ctx.Err() == nil {
		// The kept copy may be one git can no longer read, as when it refers
		// to the shared part of a split index that git has since expired:
		// start again from the real index.
		c.forget()
		c = w.privateIndex(ctx, false)
		out, err = runEnv(ctx, w.Root, c.env(), args...)
	}
	if err == nil {
		c.keep()
	}
	c.remove()
	return out, err
}

// An indexCopy is a private copy of a worktree's index, for git diff to read.
//
// git diff refreshes the index it reads: for a file whose timestamps changed
// and whose content did not, as a formatter or code generator leaves files,
// it writes the index back under index.lock, GIT_OPTIONAL_LOCKS=0
// notwithstanding. On the real index that lock refuses the person's and
// other agents' git add and commit while it is held, and outlives a git
// stopped by force while holding it. On a copy, the lock and the rewrite are
// intagent's alone, and the answer is the same.
//
// The refreshed copy is kept, under a name that identifies the real index
// it began as, and the next scan starts from it while the real index stays
// as it was: otherwise every scan after a formatter would check the content
// of every file it touched, not only the first.
type indexCopy struct {
	dir  string // the temporary directory the copy is in
	file string // the copy
	// kept is where the refreshed copy is kept, or "" if it is not.
	kept string
	// fromKept is set when the copy began as the kept one.
	fromKept bool
}

// Prefixes of the names of index copies in a worktree's Cache.
const (
	copyPrefix = "tmp-index-"
	keptPrefix = "index-"
)

// How long index copies are left in a Cache: a scan takes seconds, so a
// temporary copy a minute old is one a scan killed outright left behind; a
// kept copy not refreshed for a day may be of a worktree that is gone, and
// is made again at the next scan if it is not.
const (
	copyFor = time.Minute
	keptFor = 24 * time.Hour
)

// privateIndex copies the worktree's index; with reuse, from the copy an
// earlier scan kept, if that began as the index as it is now. It returns
// nil, and git reads the real index, if no copy can be made.
func (w *Worktree) privateIndex(ctx context.Context, reuse bool) *indexCopy {
	out, err := run(ctx, w.Root, "rev-parse", "--git-path", "index")
	if err != nil {
		return nil
	}
	src := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(src) {
		src = filepath.Join(w.Root, src)
	}
	// A worktree without an index reads as an empty one, and so does a copy
	// that does not exist.
	in, err := os.Open(src)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if in != nil {
		defer func() { _ = in.Close() }() // read only
	}
	parent, kept := os.TempDir(), ""
	if w.Cache != "" {
		parent = w.Cache
		worktree := keptPrefix + hashKey(src) + "-"
		if id, ok := indexID(in); ok {
			kept = worktree + id
		}
		pruneCopies(w.Cache, worktree, kept, time.Now())
	}
	dir, err := os.MkdirTemp(parent, copyPrefix)
	if err != nil {
		return nil
	}
	c := &indexCopy{dir: dir, file: filepath.Join(dir, "index")}
	if kept != "" {
		c.kept = filepath.Join(w.Cache, kept)
		if reuse && copyFile(c.kept, c.file) == nil {
			c.fromKept = true
			return c
		}
		_ = os.Remove(c.file) // what a failed copy may have left; there is usually nothing
	}
	if in != nil && copyFrom(in, c.file) != nil {
		c.remove()
		return nil
	}
	return c
}

// env is what points git at the copy.
func (c *indexCopy) env() []string {
	if c == nil {
		return nil
	}
	return []string{"GIT_INDEX_FILE=" + c.file}
}

// reused reports whether the copy began as the kept one.
func (c *indexCopy) reused() bool { return c != nil && c.fromKept }

// keep makes the copy, which git has read and refreshed, the one the next
// scan starts from.
func (c *indexCopy) keep() {
	if c != nil && c.kept != "" {
		_ = os.Rename(c.file, c.kept) // the next scan copies the real index instead
	}
}

// forget removes the copy and the kept one.
func (c *indexCopy) forget() {
	if c != nil && c.kept != "" {
		_ = os.Remove(c.kept) // gone already, or replaced at the next keep
	}
	c.remove()
}

// remove removes the copy.
func (c *indexCopy) remove() {
	if c != nil {
		_ = os.RemoveAll(c.dir) // left in a Cache, the next scan prunes it
	}
}

// indexID identifies the content of an open index: its size, modification
// time and last 32 bytes, which end with the checksum git writes unless
// index.skipHash is set. git replaces the index whole whenever it changes
// it, so a changed index also has a new modification time.
func indexID(f *os.File) (string, bool) {
	if f == nil {
		return "", false
	}
	fi, err := f.Stat()
	if err != nil {
		return "", false
	}
	tail := make([]byte, min(fi.Size(), 32))
	if _, err := f.ReadAt(tail, fi.Size()-int64(len(tail))); err != nil {
		return "", false
	}
	return hashKey(fmt.Sprintf("%d %d %x", fi.Size(), fi.ModTime().UnixNano(), tail)), true
}

// hashKey is a short hash of s, for file names.
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// pruneCopies removes from dir the index copies no scan will use: temporary
// copies copyFor old, which only a scan killed outright leaves behind; this
// worktree's kept copies, whose names begin with worktree, but for the one
// named kept; and any kept copy not refreshed for keptFor.
func pruneCopies(dir, worktree, kept string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		stale := false
		switch {
		case strings.HasPrefix(name, copyPrefix):
			stale = olderThan(e, now, copyFor)
		case strings.HasPrefix(name, keptPrefix) && name != kept:
			stale = strings.HasPrefix(name, worktree) || olderThan(e, now, keptFor)
		}
		if stale {
			_ = os.RemoveAll(filepath.Join(dir, name)) // another scan may have removed it first
		}
	}
}

// olderThan reports whether e was last modified more than d before now.
func olderThan(e fs.DirEntry, now time.Time, d time.Duration) bool {
	fi, err := e.Info()
	return err == nil && now.Sub(fi.ModTime()) > d
}

// copyFile copies src to dst, which must not exist, and gives dst src's
// modification time: git trusts what an index records about a file only
// when the file is older than the index, and must judge the copy as it would
// the original.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read only
	return copyFrom(in, dst)
}

// copyFrom is copyFile from an open file, read from its start.
func copyFrom(in *os.File, dst string) error {
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(in, 0, fi.Size())); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// Staged lists the paths staged for the next commit.
func (w *Worktree) Staged(ctx context.Context) ([]string, error) {
	out, err := run(ctx, w.Root, "diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// HooksDir returns the directory git runs hooks from for this worktree.
func (w *Worktree) HooksDir(ctx context.Context) (string, error) {
	out, err := run(ctx, w.Root, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	return p, nil
}

// MainRoot returns the root of the repository's main checkout. For a linked
// worktree it differs from Root; tools such as Codex read some files from it.
func (w *Worktree) MainRoot(ctx context.Context) (string, error) {
	out, err := run(ctx, w.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := filepath.Clean(strings.TrimSpace(string(out)))
	if filepath.Base(common) != ".git" {
		return "", fmt.Errorf("repository at %s has no main checkout", common)
	}
	return filepath.Dir(common), nil
}
