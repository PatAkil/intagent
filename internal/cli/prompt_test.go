package cli

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A prompt is shared as its first line, which is all the board reads: a log
// pasted under the request stays on the computer, and the hook's request
// stays small enough that the server never meters it.
func TestPromptSendsOnlyItsFirstLine(t *testing.T) {
	tm := newTeam(t, "alice")
	target, err := url.Parse(tm.url)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var largest int64
	forward := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hook" {
			mu.Lock()
			largest = max(largest, r.ContentLength)
			mu.Unlock()
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	tm.url = proxy.URL
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})

	for _, prompt := range []string{
		"Fix the flaky retry test\n" + strings.Repeat("panic: test timed out after 10m0s in TestRetry\n", 2000),
		"Fix the flaky retry test: " + strings.Repeat("goroutine 7 [running] ", 3000),
	} {
		tm.as("alice", a, claudeEvent("a1", a, "UserPromptSubmit", map[string]any{"prompt": prompt}), "hook", "claude-code")
		mu.Lock()
		got := largest
		mu.Unlock()
		if got > 16<<10 {
			t.Fatalf("a prompt of %d bytes was sent in a request of %d bytes", len(prompt), got)
		}
	}
	if out, _, _ := tm.as("alice", a, "", "board"); !strings.Contains(out, "Fix the flaky retry test") || strings.Contains(out, "panic") {
		t.Fatalf("the board after a long prompt:\n%s", out)
	}
}
