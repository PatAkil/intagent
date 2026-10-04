package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/gitx"
)

// Once git's time is up, the files left go to the server without an area,
// which it reads as an area it does not know, rather than not at all.
func TestFootprintAreasStopWithGitsTime(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "svc/pay/go.mod"), "module pay\n")
	files := []string{"README.md", "svc/pay/retry.go", "svc/pay/api/client.go", "web/app.ts"}
	ctx, cancel := context.WithCancel(context.Background())
	w := &workspace{areas: gitx.NewAreas(root, nil)}
	want := []board.PathRef{{Path: "README.md"}, {Path: "svc/pay/retry.go", Area: "svc/pay"},
		{Path: "svc/pay/api/client.go", Area: "svc/pay"}, {Path: "web/app.ts", Area: "web"}}
	if got := w.pathRefs(ctx, files); !slices.Equal(got, want) {
		t.Fatalf("with time left: %v", got)
	}
	cancel()
	w = &workspace{areas: gitx.NewAreas(root, nil)}
	got := w.pathRefs(ctx, files)
	for i := range want {
		want[i].Area = ""
	}
	if !slices.Equal(got, want) {
		t.Fatalf("out of time: %v, want every path without an area", got)
	}
}

// branchRepo makes a clone whose branch, against origin's main, has
// committed api/a1.go to api/a4.go and web/app.ts, changed svc/pay/retry.go
// and added notes.md, a new package of two files under svc/billing/, five
// files under .venv/ and three ignored ones under gen/, but not committed
// them.
func branchRepo(t *testing.T) *workspace {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	origin, clone := filepath.Join(dir, "origin.git"), filepath.Join(dir, "clone")
	gitRun(t, dir, "init", "-q", "--bare", "-b", "main", origin)
	gitRun(t, dir, "clone", "-q", origin, clone)
	for _, f := range []string{"svc/pay/go.mod", "svc/pay/retry.go", "web/package.json", "web/app.ts", "README.md"} {
		writeFile(t, filepath.Join(clone, f), "x\n")
	}
	gitRun(t, clone, "add", ".")
	gitRun(t, clone, "commit", "-q", "-m", "seed")
	gitRun(t, clone, "push", "-q", "origin", "main")
	gitRun(t, clone, "checkout", "-q", "-b", "feat")
	for i := 1; i <= 4; i++ {
		writeFile(t, filepath.Join(clone, fmt.Sprintf("api/a%d.go", i)), "package api\n")
	}
	writeFile(t, filepath.Join(clone, "web/app.ts"), "y\n")
	gitRun(t, clone, "add", ".")
	gitRun(t, clone, "commit", "-q", "-m", "api")
	writeFile(t, filepath.Join(clone, "svc/pay/retry.go"), "y\n")
	writeFile(t, filepath.Join(clone, "notes.md"), "y\n")
	writeFile(t, filepath.Join(clone, "svc/billing/b1.go"), "package billing\n")
	writeFile(t, filepath.Join(clone, "svc/billing/b2.go"), "package billing\n")
	for i := 1; i <= 5; i++ {
		writeFile(t, filepath.Join(clone, fmt.Sprintf(".venv/lib/m%d.py", i)), "")
	}
	for i := 1; i <= 3; i++ {
		writeFile(t, filepath.Join(clone, fmt.Sprintf("gen/g%d.go", i)), "")
	}
	wt, err := gitx.Open(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	return &workspace{wt: wt, areas: gitx.NewAreas(wt.Root, nil), ignore: []string{"gen/**"}}
}

// Ignored files never take room under the cap. Over it, a footprint keeps
// one file of each changed area, then the work in progress, a new package
// included, then the branch's commits, then the files of directories the
// worktree added whole that are too large to be work, in the order the
// server keeps them, and fills the cap. A directory added whole whose files
// do not all fit is reported in dirs.
func TestFootprintKeepsWhatMattersUnderTheCap(t *testing.T) {
	w := branchRepo(t)
	venv := []board.PathRef{{Path: ".venv", Area: ".venv"}}
	both := []board.PathRef{{Path: ".venv", Area: ".venv"}, {Path: "svc/billing", Area: "svc"}}
	for _, c := range []struct {
		limit int
		files string
		dirs  []board.PathRef
	}{
		{14, ".venv/lib/m1.py .venv/lib/m2.py .venv/lib/m3.py .venv/lib/m4.py .venv/lib/m5.py api/a1.go api/a2.go api/a3.go api/a4.go " +
			"notes.md svc/billing/b1.go svc/billing/b2.go svc/pay/retry.go web/app.ts", nil},
		// .venv is more than a quarter of 12 files, svc/billing is not.
		{12, "notes.md svc/billing/b1.go svc/pay/retry.go api/a1.go web/app.ts svc/billing/b2.go api/a2.go api/a3.go api/a4.go " +
			".venv/lib/m1.py .venv/lib/m2.py .venv/lib/m3.py", venv},
		{9, "notes.md svc/billing/b1.go svc/pay/retry.go api/a1.go web/app.ts svc/billing/b2.go api/a2.go api/a3.go api/a4.go", venv},
		// Over a quarter of 7, a two-file directory is bulk too.
		{7, "notes.md svc/pay/retry.go api/a1.go web/app.ts api/a2.go api/a3.go api/a4.go", both},
		{6, "notes.md svc/pay/retry.go api/a1.go web/app.ts api/a2.go api/a3.go", both},
		{3, "notes.md svc/pay/retry.go api/a1.go", both},
	} {
		fp, err := w.footprintUpTo(context.Background(), c.limit)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range fp.Files {
			got = append(got, f.Path)
		}
		if strings.Join(got, " ") != c.files || !slices.Equal(fp.Dirs, c.dirs) || fp.Truncated != (c.dirs != nil) {
			t.Errorf("up to %d: files %s, dirs %v, truncated %v\nwant files %s, dirs %v", c.limit, strings.Join(got, " "), fp.Dirs, fp.Truncated, c.files, c.dirs)
		}
	}
	fp, _ := w.footprintUpTo(context.Background(), 6)
	areas := map[string]string{"notes.md": "", "svc/pay/retry.go": "svc/pay", "web/app.ts": "web", "api/a1.go": "api", "api/a2.go": "api", "api/a3.go": "api"}
	for _, f := range fp.Files {
		if f.Area != areas[f.Path] {
			t.Errorf("%s in area %q, want %q", f.Path, f.Area, areas[f.Path])
		}
	}
}

// bulk writes n files under dir in a member's clone.
func bulk(t *testing.T, clone, dir string, n int) {
	t.Helper()
	for i := range n {
		writeFile(t, filepath.Join(clone, dir, fmt.Sprintf("f%04d.txt", i)), "x\n")
	}
}

// However a worktree comes to change more files than a footprint keeps, a
// teammate's edit of the file its agent works on is refused once.
func TestFootprintOverTheCapStillProtectsTheWork(t *testing.T) {
	edit := func(dir, file string) map[string]any {
		return map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(dir, file)}}
	}
	for _, c := range []struct {
		name string
		// before, run in alice's clone after she enrols, sets the scene.
		before func(t *testing.T, tm *team, a string)
		// hooks are alice's hook events after her session starts.
		hooks []string
		// target is the file bob edits; svc/pay/retry.go if empty.
		target string
	}{
		{"ignored files sort first", func(t *testing.T, tm *team, a string) {
			writeFile(t, filepath.Join(a, ".intagent.json"), `{"url": "`+tm.url+`", "ignore": ["api/gen/**"]}`)
			bulk(t, a, "api/gen", 2100)
			writeFile(t, filepath.Join(a, "svc/pay/retry.go"), "package pay // changed\n")
		}, nil, ""},
		{"an untracked .venv", func(t *testing.T, _ *team, a string) {
			bulk(t, a, ".venv/lib", 2500)
			writeFile(t, filepath.Join(a, "svc/pay/retry.go"), "package pay // changed\n")
		}, nil, ""},
		{"2050 committed files sort first", func(t *testing.T, _ *team, a string) {
			gitRun(t, a, "checkout", "-q", "-b", "codemod")
			bulk(t, a, "api", 2050)
			gitRun(t, a, "add", "api")
			gitRun(t, a, "commit", "-q", "-m", "codemod")
			writeFile(t, filepath.Join(a, "svc/pay/retry.go"), "package pay // changed\n")
		}, nil, ""},
		{"an edit, then a bulk", nil, []string{"edit", "venv", "PostToolUse:Bash", "Stop"}, ""},
		{"a bulk, then an edit", nil, []string{"codemod", "PostToolUse:Bash", "edit", "Stop"}, ""},
		{"a new package past the branch's commits", func(t *testing.T, _ *team, a string) {
			gitRun(t, a, "checkout", "-q", "-b", "codemod")
			bulk(t, a, "web", 1900)
			gitRun(t, a, "add", "web")
			gitRun(t, a, "commit", "-q", "-m", "codemod")
			bulk(t, a, "svc/billing", 200)
		}, []string{"Stop"}, "svc/billing/f0150.txt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tm := newTeam(t, "alice", "bob")
			a, b := tm.clone("alice"), tm.clone("bob")
			tm.enrol(map[string]string{"alice": a, "bob": b})
			if c.before != nil {
				c.before(t, tm, a)
			}
			hook := func(event string, extra map[string]any) {
				t.Helper()
				if _, errOut, code := tm.as("alice", a, claudeEvent("a1", a, event, extra), "hook", "claude-code"); code != 0 {
					t.Fatalf("%s: %d %s", event, code, errOut)
				}
			}
			hook("SessionStart", map[string]any{"source": "startup"})
			for _, h := range c.hooks {
				switch h {
				case "edit":
					hook("PreToolUse", edit(a, "svc/pay/retry.go"))
					writeFile(t, filepath.Join(a, "svc/pay/retry.go"), "package pay // changed\n")
					hook("PostToolUse", edit(a, "svc/pay/retry.go"))
				case "venv":
					bulk(t, a, ".venv/lib", 2500)
				case "codemod":
					bulk(t, a, "api", 2050)
					gitRun(t, a, "add", "api")
					gitRun(t, a, "commit", "-q", "-m", "codemod")
				case "PostToolUse:Bash":
					hook("PostToolUse", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make"}})
				default:
					hook(h, map[string]any{})
				}
			}
			target := c.target
			if target == "" {
				target = "svc/pay/retry.go"
			}
			out, _, _ := tm.as("bob", b, claudeEvent("b1", b, "PreToolUse", edit(b, target)), "hook", "claude-code")
			if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "alice") {
				var files int
				for _, r := range tm.srv.Board().Repos(time.Now()) {
					for _, cl := range tm.srv.Board().View(time.Now(), r.Repo).Claims {
						if cl.Member == "alice" {
							files = cl.FileCount
						}
					}
				}
				t.Fatalf("bob's edit of %s, which alice changed: %q (alice's claim lists %d files)", target, dec, files)
			}
		})
	}
}

// Past maxFootprintDirs directories with files left out, dirs names those
// that leave the most out, in path order.
func TestLeftOutNamesTheLargestGaps(t *testing.T) {
	var added []addedDir
	taken := []bool{}
	for i := range maxFootprintDirs + 2 {
		lo := len(taken)
		// d000 leaves 1 file out, d001 2 and so on; d201 is kept whole.
		for j := range i + 2 {
			taken = append(taken, j > i || i == maxFootprintDirs+1)
		}
		added = append(added, addedDir{fmt.Sprintf("d%03d", i), lo, len(taken)})
	}
	got := leftOut(added, taken)
	if len(got) != maxFootprintDirs || got[0] != "d001" || got[len(got)-1] != fmt.Sprintf("d%03d", maxFootprintDirs) {
		t.Fatalf("leftOut: %d dirs, %v ... %v", len(got), got[:2], got[len(got)-2:])
	}
	if !slices.IsSorted(got) {
		t.Error("dirs out of path order")
	}
}
