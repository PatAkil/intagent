package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/hook"
)

func pathRefs(paths ...string) []board.PathRef {
	out := make([]board.PathRef, len(paths))
	for i, p := range paths {
		out[i] = board.PathRef{Path: p}
	}
	return out
}

func TestUncheckedLedger(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	noteUnchecked("/w", "s1", "", now.Add(-time.Minute), pathRefs("a.go"))
	noteUnchecked("/w", "s1", "", now, pathRefs("b.go", "c.go"))
	noteUnchecked("/w", "s2", "", now, pathRefs("d.go"))
	noteUnchecked("/other", "s1", "", now, pathRefs("e.go"))
	noteUnchecked("/w", "s1", "", now, nil) // nothing to note

	got := claimUnchecked("/w", "s1", now)
	if len(got) != 2 || got[0].Paths[0] != "a.go" || len(got[1].Paths) != 2 {
		t.Fatalf("claimed %+v", got)
	}
	if again := claimUnchecked("/w", "s1", now); again != nil {
		t.Fatalf("told twice: %+v", again)
	}
	if got := claimUnchecked("/w", "s2", now); len(got) != 1 || got[0].Paths[0] != "d.go" {
		t.Fatalf("another session's ledger: %+v", got)
	}

	// An edit from a day ago is no longer news; a session's end forgets its ledger.
	noteUnchecked("/w", "s3", "", now.Add(-25*time.Hour), pathRefs("old.go"))
	if got := claimUnchecked("/w", "s3", now); got != nil {
		t.Fatalf("a day-old edit told: %+v", got)
	}
	noteUnchecked("/w", "s4", "", now, pathRefs("x.go"))
	dropUnchecked("/w", "s4")
	if got := claimUnchecked("/w", "s4", now); got != nil {
		t.Fatalf("an ended session's ledger told: %+v", got)
	}

	// A ledger stops growing at its bound.
	long := strings.Repeat("p/", 400) + "x.go"
	for range maxLedger/len(long) + 10 {
		noteUnchecked("/w", "s5", "", now, pathRefs(long))
	}
	p, _ := ledgerFile("/w", "s5")
	if fi, err := os.Stat(p); err != nil || fi.Size() > maxLedger+int64(len(long))+64 {
		t.Fatalf("ledger size: %v %v", fi.Size(), err)
	}

	// Ledgers of sessions that never ended are removed when a new one starts.
	orphan, _ := ledgerFile("/w", "gone")
	noteUnchecked("/w", "gone", "", now, pathRefs("y.go"))
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	noteUnchecked("/w", "s6", "", now, pathRefs("z.go"))
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an orphaned ledger was kept: %v", err)
	}
}

// Parallel hooks of one session lose no edit and tell none twice.
func TestUncheckedLedgerUnderParallelHooks(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				noteUnchecked("/w", "s", "", now, pathRefs(fmt.Sprintf("g%d/%d.go", g, i)))
			}
		}()
	}
	wg.Wait()
	var mu sync.Mutex
	told := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := len(claimUnchecked("/w", "s", now))
			mu.Lock()
			told += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if told != 400 {
		t.Fatalf("told %d of 400 edits", told)
	}
	dir := filepath.Dir(must2(ledgerFile("/w", "s")))
	if left, _ := filepath.Glob(filepath.Join(dir, ledgerPrefix+"*")); len(left) != 0 {
		t.Fatalf("left behind: %q", left)
	}
}

func must2[T any](v T, ok bool) T {
	if !ok {
		panic("no cache directory")
	}
	return v
}

func TestRenderUnchecked(t *testing.T) {
	at := time.Date(2026, 10, 2, 14, 2, 0, 0, time.Local).Unix()
	for _, tc := range []struct {
		edits []uncheckedEdit
		want  string
	}{
		{[]uncheckedEdit{{At: at, Paths: []string{"a.go"}}},
			"[intagent] An edit this session made at 14:02 went ahead without a check: the team's intagent server at https://ia.example did not answer in time. " +
				"A teammate may be working on a.go. Check it with the intagent check_paths tool, or tell your user."},
		{[]uncheckedEdit{{At: at + 60, Paths: []string{"a.go"}}, {At: at, Paths: []string{"b.go", "a.go"}}},
			"[intagent] 2 of this session's edits since 14:02 went ahead without a check: the team's intagent server at https://ia.example did not answer in time. " +
				"A teammate may be working on a.go, b.go. Check them with the intagent check_paths tool, or tell your user."},
		{[]uncheckedEdit{{At: at, Paths: []string{"1", "2", "3", "4", "5", "6", "7"}}},
			"[intagent] An edit this session made at 14:02 went ahead without a check: the team's intagent server at https://ia.example did not answer in time. " +
				"A teammate may be working on 1, 2, 3, 4, 5 and 2 more. Check them with the intagent check_paths tool, or tell your user."},
	} {
		if got := renderUnchecked(tc.edits, "https://ia.example"); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}

func TestUnanswered(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("dial tcp 127.0.0.1:1: connection refused"), true},
		{errAnsweredLate, true},
		{&client.APIError{Status: http.StatusServiceUnavailable}, true},
		{&client.APIError{Status: http.StatusBadGateway}, true},
		{&client.APIError{Status: http.StatusBadRequest}, false},
		{&client.APIError{Status: http.StatusUnauthorized}, false},
		{nil, false},
	} {
		if got := unanswered(tc.err); got != tc.want {
			t.Errorf("unanswered(%v) = %v", tc.err, got)
		}
	}
}

// An edit the server did not answer in time goes ahead in silence, and the
// agent hears of it at its next answer it can read; once.
func TestHookTellsTheEditsItCouldNotCheck(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	run := func(event string, extra map[string]any) (string, string) {
		t.Helper()
		out, errOut, code := tm.as("alice", a, claudeEvent("a1", a, event, extra), "hook", "claude-code")
		if code != 0 {
			t.Fatalf("%s: exit %d %s", event, code, errOut)
		}
		_, reason, ctx := decision(t, out)
		return reason, ctx
	}
	edit := func(rel string) map[string]any {
		return map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, rel)}}
	}
	run("SessionStart", map[string]any{})

	// The server is out of reach: two edits go ahead.
	t.Setenv("INTAGENT_URL", "http://127.0.0.1:1")
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	t.Setenv("INTAGENT_TIMEOUT", "200ms")
	for _, f := range []string{"README.md", "svc/pay/retry.go"} {
		if reason, ctx := run("PreToolUse", edit(f)); reason != "" || ctx != "" {
			t.Fatalf("unreachable server: %q %q", reason, ctx)
		}
	}
	// A prompt that cannot reach the server either tells nothing, and keeps the ledger.
	if _, ctx := run("UserPromptSubmit", map[string]any{"prompt": "go on"}); ctx != "" {
		t.Fatalf("prompt while unreachable: %q", ctx)
	}
	t.Setenv("INTAGENT_URL", "")
	t.Setenv("INTAGENT_TOKEN", "")
	t.Setenv("INTAGENT_TIMEOUT", "")
	_, ctx := run("UserPromptSubmit", map[string]any{"prompt": "next"})
	for _, want := range []string{"2 of this session's edits since", "went ahead without a check", "at " + tm.url + " did not answer",
		"README.md, svc/pay/retry.go", "check_paths"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("missing %q in:\n%s", want, ctx)
		}
	}
	if _, ctx := run("UserPromptSubmit", map[string]any{"prompt": "again"}); strings.Contains(ctx, "without a check") {
		t.Fatalf("told twice: %q", ctx)
	}

	// Refused edits, under fail-closed, and a rejected token went ahead neither.
	t.Setenv("INTAGENT_URL", "http://127.0.0.1:1")
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	t.Setenv("INTAGENT_TIMEOUT", "200ms")
	t.Setenv("INTAGENT_FAIL", "closed")
	if reason, _ := run("PreToolUse", edit("README.md")); !strings.Contains(reason, "did not answer") {
		t.Fatalf("fail-closed: %q", reason)
	}
	t.Setenv("INTAGENT_FAIL", "")
	t.Setenv("INTAGENT_URL", tm.url)
	t.Setenv("INTAGENT_TOKEN", "ia_rotated")
	run("PreToolUse", edit("README.md"))
	t.Setenv("INTAGENT_URL", "")
	t.Setenv("INTAGENT_TOKEN", "")
	t.Setenv("INTAGENT_TIMEOUT", "")
	if _, ctx := run("UserPromptSubmit", map[string]any{"prompt": "and"}); ctx != "" {
		t.Fatalf("refused and rejected edits told as unchecked: %q", ctx)
	}

	// A session that ends takes its ledger with it.
	t.Setenv("INTAGENT_URL", "http://127.0.0.1:1")
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	t.Setenv("INTAGENT_TIMEOUT", "200ms")
	run("PreToolUse", edit("README.md"))
	t.Setenv("INTAGENT_URL", "")
	t.Setenv("INTAGENT_TOKEN", "")
	t.Setenv("INTAGENT_TIMEOUT", "")
	run("SessionEnd", map[string]any{})
	if _, ctx := run("SessionStart", map[string]any{}); strings.Contains(ctx, "without a check") {
		t.Fatalf("an ended session's edits told: %q", ctx)
	}
}

// An answer the agent never reads does not take the ledger: Cursor's legacy
// afterFileEdit is answered, but Cursor shows the answer to nobody. The edit
// is told at the next answer the agent reads.
func TestLedgerWaitsForAnAnswerTheAgentReads(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	cursor := func(event string, extra map[string]any) string {
		t.Helper()
		m := map[string]any{"conversation_id": "c1", "hook_event_name": event, "workspace_roots": []string{a}, "cursor_version": "1.7"}
		maps.Copy(m, extra)
		b, _ := json.Marshal(m)
		out, errOut, code := tm.as("alice", a, string(b), "hook", "cursor")
		if code != 0 {
			t.Fatalf("%s: exit %d %s", event, code, errOut)
		}
		return out
	}
	file := filepath.Join(a, "README.md")
	t.Setenv("INTAGENT_URL", "http://127.0.0.1:1")
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	t.Setenv("INTAGENT_TIMEOUT", "200ms")
	cursor("preToolUse", map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": file}})
	t.Setenv("INTAGENT_URL", "")
	t.Setenv("INTAGENT_TOKEN", "")
	t.Setenv("INTAGENT_TIMEOUT", "")
	if out := cursor("afterFileEdit", map[string]any{"file_path": file}); strings.Contains(out, "without a check") {
		t.Fatalf("afterFileEdit told the ledger: %s", out)
	}
	if out := cursor("beforeSubmitPrompt", map[string]any{"prompt": "next"}); !strings.Contains(out, "made at") || !strings.Contains(out, "README.md") {
		t.Fatalf("the next answer the agent reads: %s", out)
	}
}

// The edit a post_edit reports is left out of the ledger's telling when the
// server checked it as it was reported, and said what it found; the
// session's other unchecked edits are told.
func TestTellUncheckedLeavesOutTheEditTheServerChecked(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	reported := hook.Event{Kind: board.KindPostEdit, SessionID: "s1", ToolUseID: "t1"}
	for _, tc := range []struct {
		name    string
		checked bool
		want    string
	}{
		{"checked as it was reported", true, "A teammate may be working on b.go, c.go. Check them"},
		{"not checked by the server", false, "A teammate may be working on a.go, b.go, c.go. Check them"},
	} {
		noteUnchecked("/w", "s1", "t1", now, pathRefs("a.go"))
		noteUnchecked("/w", "s1", "t2", now, pathRefs("b.go"))
		noteUnchecked("/w", "s1", "", now, pathRefs("c.go"))
		res := board.HookResult{Context: "[intagent] from the server", CheckedAfter: tc.checked}
		tellUnchecked("/w", reported, &res, "https://ia.example")
		if !strings.Contains(res.Context, tc.want) || !strings.HasSuffix(res.Context, "\n\n[intagent] from the server") {
			t.Errorf("%s: %q", tc.name, res.Context)
		}
	}
	noteUnchecked("/w", "s1", "t1", now, pathRefs("a.go"))
	res := board.HookResult{CheckedAfter: true}
	if tellUnchecked("/w", reported, &res, "https://ia.example"); res.Context != "" {
		t.Errorf("the reported edit alone: %q", res.Context)
	}
}

// A server that gets to an edit too late to check it says so; the hook then
// does what it does when the server does not answer.
func TestHookTreatsAnUncheckedAnswerAsNoAnswer(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	late := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"allow","unchecked":true}`))
	}))
	defer late.Close()
	t.Setenv("INTAGENT_URL", late.URL)
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	ev := claudeEvent("a1", a, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}})
	if out, _, code := tm.as("alice", a, ev, "hook", "claude-code"); out != "" || code != 0 {
		t.Fatalf("unchecked answer: %q %d", out, code)
	}
	if edits := claimUnchecked(a, "a1", time.Now()); len(edits) != 1 || edits[0].Paths[0] != "README.md" {
		t.Fatalf("ledger: %+v", edits)
	}
	t.Setenv("INTAGENT_FAIL", "closed")
	out, _, _ := tm.as("alice", a, ev, "hook", "claude-code")
	if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "too late to check") {
		t.Fatalf("fail-closed, unchecked answer: %q", out)
	}
}

// The ledger is told only in answers the agent reads.
func TestCarriesContext(t *testing.T) {
	for _, tc := range []struct {
		agent, event string
		want         bool
	}{
		{"claude-code", "UserPromptSubmit", true},
		{"claude-code", "PostToolUse", true},
		{"copilot", "SessionStart", true},
		{"codex", "PostToolUse", true},
		{"cursor", "postToolUse", true},
		{"cursor", "afterFileEdit", false},
		{"gemini", "AfterTool", true},
		{"gemini", "BeforeTool", false},
	} {
		ad, err := hook.For(tc.agent)
		if err != nil {
			t.Fatal(err)
		}
		if got := carriesContext(ad, hook.Event{Name: tc.event}); got != tc.want {
			t.Errorf("%s %s: %v", tc.agent, tc.event, got)
		}
	}
}
