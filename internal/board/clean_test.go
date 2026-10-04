package board

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// What a client sends is bounded before the board's lock is taken: a
// footprint by the number of entries sent, whatever they hold, and a prompt
// by the first 8 KB of its first line.
func TestCleanBoundsWhatClientsSend(t *testing.T) {
	refs := func(n int, p string) []PathRef {
		out := make([]PathRef, n)
		for i := range out {
			out[i] = PathRef{Path: p}
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		files     []PathRef
		want      []PathRef
		truncated bool
	}{
		{"distinct paths and areas", []PathRef{{Path: "a/x.go", Area: "a"}, {Path: `b\y.go`}}, []PathRef{{Path: "a/x.go", Area: "a"}, {Path: "b/y.go"}}, false},
		{"duplicates count toward the limit", append(refs(2500, "a/x.go"), PathRef{Path: "b/y.go"}), []PathRef{{Path: "a/x.go"}}, true},
		{"invalid paths count toward the limit", append(refs(2500, "/etc/passwd"), PathRef{Path: "b/y.go"}), []PathRef{}, true},
		{"a duplicate within the limit", []PathRef{{Path: "a/x.go"}, {Path: "a/x.go"}}, []PathRef{{Path: "a/x.go"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "s",
				Where: Where{Repo: "r", Host: "h", Worktree: "/w"}, Footprint: &Footprint{Files: tc.files}}
			got, err := ev.clean(2000)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Footprint.Files, tc.want) || got.Footprint.Truncated != tc.truncated {
				t.Fatalf("footprint = %d files %v, truncated %v", len(got.Footprint.Files), got.Footprint.Files[:min(3, len(got.Footprint.Files))], got.Footprint.Truncated)
			}
			if len(ev.Footprint.Files) != len(tc.files) {
				t.Fatal("clean changed the caller's footprint")
			}
		})
	}
	for prompt, want := range map[string]string{
		"  fix the retry\nand then the rest": "fix the retry",
		strings.Repeat("x", 1<<20):           strings.Repeat("x", maxPrompt),
	} {
		ev := HookEvent{Kind: KindPrompt, Member: "alice", Agent: AgentClaudeCode, SessionID: "s", Where: Where{Repo: "r", Host: "h", Worktree: "/w"}, Prompt: prompt}
		if got, _ := ev.clean(2000); got.Prompt != want {
			t.Errorf("prompt of %d bytes cleaned to %d bytes", len(prompt), len(got.Prompt))
		}
	}
}

// The directories a footprint names in place of files it leaves out are
// bounded and cleaned as its files are: by the number of entries sent,
// whatever they hold.
func TestCleanBoundsFootprintDirs(t *testing.T) {
	dirs := []PathRef{{Path: `gen\out`, Area: "gen"}, {Path: "/etc"}, {Path: "gen/out"}, {Path: "a/../../b"}}
	for i := range 300 {
		dirs = append(dirs, PathRef{Path: fmt.Sprintf("build/%03d", i)})
	}
	ev := HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "s",
		Where: Where{Repo: "r", Host: "h", Worktree: "/w"}, Footprint: &Footprint{Files: []PathRef{{Path: "a/x.go"}}, Dirs: dirs}}
	got, err := ev.clean(2000)
	if err != nil {
		t.Fatal(err)
	}
	fp := got.Footprint
	want := []PathRef{{Path: "gen/out", Area: "gen"}, {Path: "build/000"}}
	if len(fp.Dirs) != maxFootprintDirs-3 || !slices.Equal(fp.Dirs[:2], want) || fp.Dirs[len(fp.Dirs)-1].Path != "build/195" {
		t.Fatalf("dirs = %d entries, from %v to %v", len(fp.Dirs), fp.Dirs[:min(2, len(fp.Dirs))], fp.Dirs[len(fp.Dirs)-1])
	}
	if !fp.Truncated || len(fp.Files) != 1 {
		t.Fatalf("footprint = %+v files, truncated %v", fp.Files, fp.Truncated)
	}
	if len(ev.Footprint.Dirs) != len(dirs) {
		t.Fatal("clean changed the caller's footprint")
	}
	// A footprint without directories keeps none.
	ev.Footprint = &Footprint{Files: []PathRef{{Path: "a/x.go"}}}
	if got, _ := ev.clean(2000); got.Footprint.Dirs != nil || got.Footprint.Truncated {
		t.Fatalf("footprint without dirs cleaned to %+v", *got.Footprint)
	}
}

// clean bounds a footprint's files and keeps the rest of it as the client
// sent it, every field, including ones added after clean was written.
func TestCleanKeepsEveryFootprintField(t *testing.T) {
	var fp Footprint
	v := reflect.ValueOf(&fp).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch {
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Kind() == reflect.Int:
			f.SetInt(1)
		case f.Kind() == reflect.String:
			f.SetString("x")
		case f.Type() == reflect.TypeFor[[]PathRef]():
			f.Set(reflect.ValueOf([]PathRef{{Path: "a/x.go", Area: "a"}}))
		default:
			t.Fatalf("Footprint.%s is a %s, which this test cannot fill: teach it", v.Type().Field(i).Name, f.Type())
		}
	}
	ev := HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentClaudeCode, SessionID: "s",
		Where: Where{Repo: "r", Host: "h", Worktree: "/w"}, Footprint: &fp}
	got, err := ev.clean(2000)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got.Footprint, fp) {
		t.Fatalf("clean turned %+v into %+v", fp, *got.Footprint)
	}
}

// The board keeps what clean left of a footprint, and says it was cut.
func TestReconcileKeepsTheCleanedFootprint(t *testing.T) {
	b := New(Config{})
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	w := Where{Repo: "r", Host: "h", Worktree: "/w"}
	files := []PathRef{{Path: "a/x.go"}}
	for range 2500 {
		files = append(files, PathRef{Path: "a/x.go"})
	}
	files = append(files, PathRef{Path: "b/y.go"})
	if _, err := b.Hook(now, HookEvent{Kind: KindSessionStart, Member: "alice", Agent: AgentClaudeCode, SessionID: "s", Where: w,
		Footprint: &Footprint{Files: files}}); err != nil {
		t.Fatal(err)
	}
	v := b.View(now, "r")
	if len(v.Claims) != 1 || len(v.Claims[0].Files) != 1 || !v.Claims[0].Truncated {
		t.Fatalf("claims = %+v", v.Claims)
	}
}
