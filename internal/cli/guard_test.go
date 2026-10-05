package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// checkGate stands between the hooks and the team's server and answers
// /v1/check itself when answer says so.
type checkGate struct {
	next   http.Handler
	mu     sync.Mutex
	checks int
	// answer returns the status to give the nth check, or 0 to pass it on.
	answer func(n int) int
}

func (g *checkGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/check" {
		g.mu.Lock()
		g.checks++
		n := g.checks
		g.mu.Unlock()
		if status := g.answer(n); status != 0 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"not now"}`))
			return
		}
	}
	g.next.ServeHTTP(w, r)
}

func (g *checkGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checks
}

// gatedTeam is a team whose clients reach the server through a checkGate.
// Alice's active agent holds svc/pay/** exclusively, and bob has staged 250
// documents and svc/pay/retry.go, more than one check of 200 paths holds.
func gatedTeam(t *testing.T, answer func(n int) int) (*team, *checkGate, string) {
	t.Helper()
	tm := newTeam(t, "alice", "bob")
	g := &checkGate{next: tm.srv.Handler(), answer: answer}
	hs := httptest.NewServer(g)
	t.Cleanup(hs.Close)
	tm.url = hs.URL
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	tm.as("alice", a, claudeEvent("a1", a, "SessionStart", map[string]any{"source": "startup"}), "hook", "claude-code")
	if out, errOut, code := tm.as("alice", a, "", "declare", "-x", "-m", "Rework retries", "svc/pay/**"); code != 0 {
		t.Fatalf("declare: %s %s", out, errOut)
	}
	bulk(t, b, "docs", 250)
	writeFile(t, filepath.Join(b, "svc/pay/retry.go"), "package pay // hotfix\n")
	gitRun(t, b, "add", "-A")
	return tm, g, b
}

// A commit is checked on every staged file, in checks of 200 paths, which
// is all an older server checks of one.
func TestGuardChecksEveryStagedFile(t *testing.T) {
	tm, g, b := gatedTeam(t, func(int) int { return 0 })
	_, errOut, code := tm.as("bob", b, "", "guard")
	if code != 1 || !strings.Contains(errOut, "svc/pay/retry.go: alice") {
		t.Fatalf("guard: exit %d\n%s", code, errOut)
	}
	if n := g.count(); n != 2 {
		t.Errorf("%d checks for 259 staged files, want 2", n)
	}
}

// A server that is busy for a moment is asked again; one that does not
// answer a check leaves the rest of the commit unchecked, which guard says,
// and which INTAGENT_FAIL=closed refuses.
func TestGuardSaysWhatItDidNotCheck(t *testing.T) {
	tm, g, b := gatedTeam(t, func(n int) int {
		switch n {
		case 1:
			return http.StatusTooManyRequests
		case 2:
			return 0
		}
		return http.StatusServiceUnavailable
	})
	_, errOut, code := tm.as("bob", b, "", "guard")
	if code != 0 || !strings.Contains(errOut, "59 of the 259 files in this commit were not checked") {
		t.Fatalf("guard: exit %d\n%s", code, errOut)
	}
	if n := g.count(); n != 3 {
		t.Errorf("%d checks, want the busy one again and then the one that failed", n)
	}
	t.Setenv("INTAGENT_FAIL", "closed")
	g.mu.Lock()
	g.checks = 1
	g.mu.Unlock()
	if _, errOut, code := tm.as("bob", b, "", "guard"); code != 1 || !strings.Contains(errOut, "INTAGENT_FAIL=closed is set, so the commit is refused") {
		t.Fatalf("guard under fail-closed: exit %d\n%s", code, errOut)
	}
}

// 'intagent check' and the MCP check_paths tool take more paths than one
// check carries, sent in several, and tell what all of them found.
func TestCheckTakesMorePathsThanOneCheck(t *testing.T) {
	tm, g, b := gatedTeam(t, func(int) int { return 0 })
	var paths []string
	for i := range 250 {
		paths = append(paths, fmt.Sprintf("docs/f%04d.txt", i))
	}
	paths = append(paths, "svc/pay/retry.go")
	const found = "svc/pay/retry.go:\n  [block] alice's agent"
	out, errOut, code := tm.as("bob", b, "", append([]string{"check"}, paths...)...)
	if code != 0 || !strings.Contains(out, found) {
		t.Fatalf("check: exit %d\n%s%s", code, out, errOut)
	}
	if n := g.count(); n != 2 {
		t.Errorf("check: %d checks for 251 paths, want 2", n)
	}
	args, err := json.Marshal(map[string]any{"name": "check_paths", "arguments": map[string]any{"paths": paths}})
	if err != nil {
		t.Fatal(err)
	}
	calls := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(args) + "}\n"
	out, errOut, code = tm.as("bob", b, calls, "mcp")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var r struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if code != 0 || len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &r) != nil || r.Result.IsError ||
		len(r.Result.Content) != 1 || !strings.Contains(r.Result.Content[0].Text, found) {
		t.Fatalf("check_paths: exit %d\n%s%s", code, out, errOut)
	}
	if n := g.count(); n != 4 {
		t.Errorf("check_paths: %d checks in all, want 4", n)
	}
}

// One tool call that changes many files is one pre_edit, which the hook
// sends whole: a file a teammate holds exclusively is refused wherever the
// patch names it, also past the first 200 paths, which are all the server
// compares with the rest of teammates' work. The server judged only those
// 200, and let the patch into alice's reservation with a note.
func TestLargePatchIntoAReservationIsRefused(t *testing.T) {
	for _, n := range []int{150, 250, 1999} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			tm := newTeam(t, "alice", "bob")
			a, b := tm.clone("alice"), tm.clone("bob")
			tm.enrol(map[string]string{"alice": a, "bob": b})
			tm.as("alice", a, claudeEvent("a1", a, "SessionStart", map[string]any{"source": "startup"}), "hook")
			if out, errOut, code := tm.as("alice", a, "", "declare", "-x", "-m", "Rework payment retries", "svc/pay/**"); code != 0 {
				t.Fatalf("declare: %s %s", out, errOut)
			}
			var patch strings.Builder
			patch.WriteString("*** Begin Patch\n")
			for i := range n {
				fmt.Fprintf(&patch, "*** Add File: docs/p%04d.md\n+x\n", i)
			}
			patch.WriteString("*** Update File: svc/pay/retry.go\n@@\n-package pay\n+package pay // bob\n*** End Patch\n")
			ev, _ := json.Marshal(map[string]any{"session_id": "b1", "cwd": b, "hook_event_name": "PreToolUse", "tool_name": "apply_patch",
				"tool_use_id": "u1", "tool_input": map[string]any{"command": patch.String()}})
			tm.as("bob", b, claudeEvent("b1", b, "SessionStart", map[string]any{"source": "startup"}), "hook", "codex")
			out, _, _ := tm.as("bob", b, string(ev), "hook", "codex")
			if dec, reason, _ := decision(t, out); dec != "deny" || !strings.Contains(reason, "svc/pay/retry.go") {
				t.Fatalf("a patch of %d documents and svc/pay/retry.go, which alice holds exclusively: %.300q", n, out)
			}
		})
	}
}
