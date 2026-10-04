package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Result(), string(body)
}

func TestDashboardIsServedLockedDown(t *testing.T) {
	resp, body := get(t, "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<html") {
		t.Fatalf("GET / = %d %.100s", resp.StatusCode, body)
	}
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	// Everything the page loads is embedded: the policy allows nothing else.
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"#]+)"`).FindAllStringSubmatch(body, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "data:") {
			continue // img-src allows data: for the icon
		}
		if strings.Contains(ref, "//") {
			t.Errorf("the page loads %s from elsewhere", ref)
			continue
		}
		if r, _ := get(t, "/"+strings.TrimPrefix(ref, "/")); r.StatusCode != http.StatusOK {
			t.Errorf("the page loads %s, which is not served: %d", ref, r.StatusCode)
		}
	}
	if r, _ := get(t, "/app.js"); !strings.Contains(r.Header.Get("Content-Type"), "javascript") {
		t.Errorf("app.js Content-Type = %q", r.Header.Get("Content-Type"))
	}
}

// The dashboard reloads the board and redraws the feed within their bounds,
// whatever its clocks do: testdata/pacing.js runs app.js on fake clocks and
// timers through a sleep in the middle of a request and wall clock steps both
// ways. It needs Node, which CI's runners have.
func TestDashboardPacingIsBounded(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	if out, err := exec.Command(node, "testdata/pacing.js", "static/app.js").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
