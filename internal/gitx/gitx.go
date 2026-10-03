// Package gitx answers the few questions intagent asks git: which repository
// and worktree a directory belongs to, which branch is checked out, which
// files differ from the default branch, and which area a file belongs to.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
}

// defaultTimeout bounds a git command when the caller set no deadline.
const defaultTimeout = 10 * time.Second

// run executes git in dir and returns its stdout.
func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = time.Second // a helper git started may hold the output open after git is killed
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
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
		root, abs = r, evalExisting(abs)
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

// evalExisting resolves symlinks in the longest existing prefix of p, so a
// file that does not exist yet still resolves through a symlinked directory.
func evalExisting(p string) string {
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

// Changes lists the files that differ from the default branch: committed
// changes since the merge base, uncommitted changes, and untracked files. It
// keeps at most limit paths and reports whether it dropped any.
//
// It scans the working tree once, with git status, so git's untracked cache
// and fsmonitor apply; committed changes come from a tree-to-tree diff, which
// does not touch the working tree at all.
func (w *Worktree) Changes(ctx context.Context, limit int) ([]string, bool, error) {
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
	tracked, err := run(ctx, w.Root, "diff", "--name-only", "--no-renames", "-z", base, "--")
	if err != nil {
		if base != "HEAD" {
			return nil, false, err
		}
		// No commits yet: everything in the index is new.
		if tracked, err = run(ctx, w.Root, "ls-files", "-z"); err != nil {
			return nil, false, err
		}
	}
	add(tracked)
	untracked, err := run(ctx, w.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, false, err
	}
	add(untracked)
	sort.Strings(files)
	if limit > 0 && len(files) > limit {
		return files[:limit], true, nil
	}
	return files, false, nil
}

// Staged lists the paths staged for the next commit.
func (w *Worktree) Staged(ctx context.Context) ([]string, error) {
	out, err := run(ctx, w.Root, "diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			files = append(files, string(p))
		}
	}
	return files, nil
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
