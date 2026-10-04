package cli

import (
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
