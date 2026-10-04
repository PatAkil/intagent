package board

import (
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
