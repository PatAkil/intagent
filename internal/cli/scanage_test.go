package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/patakil/intagent/internal/board"
)

// A footprint says how long before its event was sent its scan began, so
// the server keeps the changes hooks reported since, which git may not have
// seen: with git taking 400 ms, the age is at least that.
func TestFootprintSaysHowOldItsScanIs(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	writeFile(t, filepath.Join(a, "svc/pay/new.go"), "package pay\n")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	slow := t.TempDir()
	writeFile(t, filepath.Join(slow, "git"), "#!/bin/sh\ncase \" $* \" in *\" ls-files \"*) sleep 0.4;; esac\nexec "+real+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(slow, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", slow+string(os.PathListSeparator)+os.Getenv("PATH"))
	var mu sync.Mutex
	var ages []int64
	h := tm.srv.Handler()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hook" {
			body, _ := io.ReadAll(r.Body)
			var ev board.HookEvent
			if json.Unmarshal(body, &ev) == nil && ev.Footprint != nil {
				mu.Lock()
				ages = append(ages, ev.Footprint.AgeMS)
				mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	tm.url = front.URL
	tm.enrol(map[string]string{"alice": a})
	if _, errOut, code := tm.as("alice", a, claudeEvent("a1", a, "SessionStart", map[string]any{"source": "startup"}), "hook"); code != 0 {
		t.Fatalf("hook: %d %s", code, errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ages) != 1 || ages[0] < 400 {
		t.Fatalf("the footprints sent were %v ms old, want one at least 400", ages)
	}
}
