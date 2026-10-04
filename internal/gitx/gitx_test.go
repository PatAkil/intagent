package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repository with an "origin" whose default branch is main.
func newRepo(t *testing.T) (origin, clone string) {
	t.Helper()
	base := t.TempDir()
	origin = filepath.Join(base, "origin.git")
	git(t, base, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(base, "seed")
	git(t, base, "init", "-q", "-b", "main", seed)
	write(t, seed, "README.md", "hi\n")
	write(t, seed, "services/payments/go.mod", "module pay\n")
	write(t, seed, "services/payments/retry.go", "package pay\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "remote", "add", "origin", origin)
	git(t, seed, "push", "-q", "origin", "main")
	clone = filepath.Join(base, "clone")
	git(t, base, "clone", "-q", origin, clone)
	return origin, clone
}

func TestNormalizeRemote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"git@github.com:Acme/Mono.git":             "github.com/acme/mono",
		"https://github.com/acme/mono":             "github.com/acme/mono",
		"https://user:pw@github.com/acme/mono.git": "github.com/acme/mono",
		"ssh://git@gitlab.example.com:22/a/b.git":  "gitlab.example.com/a/b",
		"git://host.xz/path/to/repo.git/":          "host.xz/path/to/repo",
		"/srv/git/mono.git":                        "file/srv/git/mono",
		"":                                         "",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAndChanges(t *testing.T) {
	ctx := context.Background()
	origin, clone := newRepo(t)
	git(t, clone, "checkout", "-q", "-b", "feat/retry")

	w, err := Open(ctx, filepath.Join(clone, "services"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Branch != "feat/retry" || w.RepoID() != NormalizeRemote(origin) {
		t.Fatalf("worktree = %+v, repo %q", w, w.RepoID())
	}
	if def, err := w.DefaultBranch(ctx); err != nil || def != "origin/main" {
		t.Fatalf("DefaultBranch = %q, %v", def, err)
	}

	files, base, err := w.Changes(ctx)
	if err != nil || len(files) != 0 || base != git(t, clone, "rev-parse", "origin/main") {
		t.Fatalf("clean worktree changes = %v against %s, %v", files, base, err)
	}
	write(t, clone, "services/payments/retry.go", "package pay // changed\n")
	write(t, clone, "services/payments/backoff.go", "package pay\n")
	git(t, clone, "add", "services/payments/backoff.go")
	git(t, clone, "commit", "-q", "-m", "backoff")
	write(t, clone, "notes/new file.txt", "untracked\n")
	write(t, clone, ".gitignore", "build/\n")
	write(t, clone, "build/out.bin", "ignored\n")

	files, _, err = w.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := ".gitignore,notes/new file.txt,services/payments/backoff.go,services/payments/retry.go"
	if got := strings.Join(files, ","); got != want {
		t.Fatalf("changes = %s, want %s", got, want)
	}

	// Undoing a committed change in the worktree undoes it in the footprint.
	git(t, clone, "rm", "-q", "--cached", "services/payments/backoff.go")
	if err := os.Remove(filepath.Join(clone, "services/payments/backoff.go")); err != nil {
		t.Fatal(err)
	}
	files, _, _ = w.Changes(ctx)
	if got := strings.Join(files, ","); got != ".gitignore,notes/new file.txt,services/payments/retry.go" {
		t.Fatalf("after undoing backoff.go: %s", got)
	}
	git(t, clone, "reset", "-q", "HEAD")
	write(t, clone, "services/payments/backoff.go", "package pay\n")

	// Once the branch is merged and pushed, nothing differs from the default branch.
	git(t, clone, "add", "-A")
	git(t, clone, "commit", "-q", "-m", "rest")
	git(t, clone, "push", "-q", "origin", "HEAD:main")
	git(t, clone, "fetch", "-q", "origin")
	if files, _, _ := w.Changes(ctx); len(files) != 0 {
		t.Fatalf("merged branch still shows changes: %v", files)
	}
}

func TestWorktreesShareRepoID(t *testing.T) {
	ctx := context.Background()
	_, clone := newRepo(t)
	second := filepath.Join(t.TempDir(), "wt2")
	git(t, clone, "worktree", "add", "-q", "-b", "other", second)
	a, err := Open(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if a.RepoID() != b.RepoID() || a.Root == b.Root || b.Branch != "other" {
		t.Fatalf("worktrees: %+v %+v", a, b)
	}
	if hooks, err := b.HooksDir(ctx); err != nil || !strings.Contains(hooks, "hooks") {
		t.Fatalf("HooksDir = %q, %v", hooks, err)
	}
}

func TestRepoWithoutCommitsOrRemote(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "Fresh")
	git(t, t.TempDir(), "init", "-q", "-b", "main", dir)
	write(t, dir, "a.txt", "x")
	w, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if w.RepoID() != "local/fresh" {
		t.Fatalf("RepoID = %q", w.RepoID())
	}
	files, base, err := w.Changes(ctx)
	if err != nil || strings.Join(files, ",") != "a.txt" || base != "HEAD" {
		t.Fatalf("changes = %v, %v", files, err)
	}
	if _, err := Open(ctx, t.TempDir()); err == nil {
		t.Fatal("Open succeeded outside a repository")
	}
}

func TestRelAndStaged(t *testing.T) {
	ctx := context.Background()
	_, clone := newRepo(t)
	w, _ := Open(ctx, clone)
	for in, want := range map[string]string{
		filepath.Join(clone, "services", "x.go"): "services/x.go",
		"services/y.go":                          "services/y.go",
	} {
		if got, ok := w.Rel(in); !ok || got != want {
			t.Errorf("Rel(%q) = %q, %v", in, got, ok)
		}
	}
	for _, out := range []string{"/tmp/elsewhere.go", filepath.Join(clone, ".git", "config"), clone, filepath.Join(clone, "..", "x")} {
		if got, ok := w.Rel(out); ok {
			t.Errorf("Rel(%q) = %q, want outside", out, got)
		}
	}
	// A symlinked path to the worktree resolves to the same relative path.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(clone, link); err == nil {
		if got, ok := w.Rel(filepath.Join(link, "services", "new.go")); !ok || got != "services/new.go" {
			t.Errorf("Rel through symlink = %q, %v", got, ok)
		}
	}
	write(t, clone, "s.txt", "x")
	git(t, clone, "add", "s.txt")
	if got, err := w.Staged(ctx); err != nil || strings.Join(got, ",") != "s.txt" {
		t.Fatalf("Staged = %v, %v", got, err)
	}
}

func TestAreas(t *testing.T) {
	_, clone := newRepo(t)
	write(t, clone, "apps/web/package.json", "{}")
	write(t, clone, "apps/web/src/deep/x.ts", "")
	write(t, clone, "libs/net/Net.csproj", "")
	write(t, clone, "docs/guide/intro.md", "")
	a := NewAreas(clone, nil)
	for rel, want := range map[string]string{
		"services/payments/retry.go":     "services/payments",
		"services/payments/sub/x.go":     "services/payments",
		"apps/web/src/deep/x.ts":         "apps/web",
		"libs/net/Client.cs":             "libs/net",
		"docs/guide/intro.md":            "docs",
		"README.md":                      "",
		"services/payments/deleted/y.go": "services/payments",
	} {
		if got := a.Of(rel); got != want {
			t.Errorf("Of(%q) = %q, want %q", rel, got, want)
		}
	}
	cfg := NewAreas(clone, []string{"docs/*", "apps/*/src"})
	if got := cfg.Of("docs/guide/intro.md"); got != "docs/guide" {
		t.Errorf("configured area = %q", got)
	}
	if got := cfg.Of("apps/web/src/deep/x.ts"); got != "apps/web/src" {
		t.Errorf("configured area = %q", got)
	}
	if got := cfg.Of("apps/web/package.json"); got != "apps/web" {
		t.Errorf("fallback to markers = %q", got)
	}
}

// Before the first commit, staged and untracked files are both new.
func TestChangesBeforeTheFirstCommit(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "svc/a.go", "package svc\n")
	write(t, dir, "svc/b.go", "package svc\n")
	git(t, dir, "add", "svc/a.go")
	w, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := w.Changes(context.Background())
	if err != nil || strings.Join(files, ",") != "svc/a.go,svc/b.go" {
		t.Fatalf("changes = %v, %v", files, err)
	}
}
