package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/server"
)

// stall holds the server's next board call until it is released, as a server
// queueing behind its board lock does.
type stall struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (s *stall) now() time.Time {
	s.mu.Lock()
	armed, entered, release := s.armed, s.entered, s.release
	s.armed = false
	s.mu.Unlock()
	if armed {
		close(entered)
		<-release
	}
	return time.Now()
}

// during runs fn while the server holds its next board call, and lets the
// call go on once fn has returned.
func (s *stall) during(t *testing.T, fn func()) {
	t.Helper()
	s.mu.Lock()
	s.armed, s.entered, s.release = true, make(chan struct{}), make(chan struct{})
	entered, release := s.entered, s.release
	s.mu.Unlock()
	fn()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the board")
	}
	close(release)
}

// serveWith replaces the team's server, before anyone logs in, with one built
// from the same members and changed by mutate, run by Serve.
func (tm *team) serveWith(mutate func(*server.Options)) {
	tm.t.Helper()
	var ms []server.Member
	for name, tok := range tm.tokens {
		ms = append(ms, server.Member{Name: name, TokenSHA256: server.HashToken(tok)})
	}
	o := server.Options{Members: ms, Board: board.DefaultConfig(), SweepEvery: time.Hour}
	mutate(&o)
	srv, err := server.New(o)
	if err != nil {
		tm.t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tm.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	tm.t.Cleanup(func() { cancel(); <-done })
	tm.url, tm.srv = "http://"+ln.Addr().String(), srv
}

// webhookSink collects the text of the webhook messages a server posts.
type webhookSink struct {
	mu    sync.Mutex
	texts []string
}

func (w *webhookSink) start(t *testing.T) string {
	hs := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p)
		w.mu.Lock()
		w.texts = append(w.texts, p.Text)
		w.mu.Unlock()
	}))
	t.Cleanup(hs.Close)
	return hs.URL
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An overloaded server answers a hook after its client gave up and the agent
// went ahead. Nothing it decides then is for anyone: the bump is kept for the
// agent's next edit, no refusal is announced, the note waits for an answer
// the agent reads, and the edit made without a check is told afterwards.
func TestOverloadedServerDecidesNothingForAgentsThatLeft(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	var sink webhookSink
	var held stall
	hook := sink.start(t)
	tm.serveWith(func(o *server.Options) {
		o.Now = held.now
		o.Webhook = server.WebhookConfig{URL: hook, Events: []board.ActivityKind{board.ActivityConflict}}
	})
	a, b := tm.clone("alice"), tm.clone("bob")
	tm.enrol(map[string]string{"alice": a, "bob": b})
	run := func(member, dir, event, id string, extra map[string]any) string {
		t.Helper()
		if id != "" {
			extra["tool_use_id"] = id
		}
		out, errOut, code := tm.as(member, dir, claudeEvent(member[:1]+"1", dir, event, extra), "hook", "claude-code")
		if code != 0 {
			t.Fatalf("%s %s: exit %d %s", member, event, code, errOut)
		}
		return out
	}
	edit := func(dir, rel string) map[string]any {
		return map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(dir, rel)}}
	}
	view := func() board.View { return tm.srv.Board().View(time.Now(), tm.srv.Board().Repos(time.Now())[0].Repo) }
	stats := func() board.Stats { return view().Stats }
	lastSeen := func(member string) time.Time {
		for _, c := range view().Claims {
			if c.Member == member {
				return c.Sessions[0].LastSeen
			}
		}
		return time.Time{}
	}

	run("bob", b, "SessionStart", "", map[string]any{})
	run("bob", b, "UserPromptSubmit", "", map[string]any{"prompt": "Payments"})
	for i, f := range []string{"svc/pay/retry.go", "svc/pay/client.go"} {
		id := "b-" + string(rune('0'+i))
		run("bob", b, "PreToolUse", id, edit(b, f))
		run("bob", b, "PostToolUse", id, edit(b, f))
	}
	if out, errOut, code := tm.as("bob", b, "", "declare", "-x", "-m", "Rewrite the guide", "docs/**"); code != 0 {
		t.Fatalf("declare: %s %s", out, errOut)
	}
	run("alice", a, "SessionStart", "", map[string]any{})
	run("alice", a, "UserPromptSubmit", "", map[string]any{"prompt": "Tidy up"})
	if dec, _, _ := decision(t, run("alice", a, "PreToolUse", "a-0", edit(a, "svc/pay/client.go"))); dec != "deny" {
		t.Fatalf("an edit answered in time: %q, want the bump", dec)
	}
	if out, errOut, code := tm.as("bob", b, "", "note", "alice", "retry.go", "is", "mine", "today"); code != 0 {
		t.Fatalf("note: %s %s", out, errOut)
	}

	// The server stalls past the hooks' timeout. Each hook lets its agent go
	// ahead in silence; the server then gets to the event.
	t.Setenv("INTAGENT_TIMEOUT", "500ms")
	late := func(event, id string, extra map[string]any) {
		t.Helper()
		seen, unheard := lastSeen("alice"), stats().Unheard
		held.during(t, func() {
			if out := run("alice", a, event, id, extra); out != "" {
				t.Fatalf("late answer: %q, want the hook to fail open", out)
			}
		})
		waitFor(t, "the server to get to "+event, func() bool { return stats().Unheard > unheard || lastSeen("alice").After(seen) })
	}
	late("UserPromptSubmit", "", map[string]any{"prompt": "next"})
	late("PreToolUse", "a-1", edit(a, "svc/pay/retry.go"))
	late("PreToolUse", "a-2", edit(a, "docs/guide.md"))
	t.Setenv("INTAGENT_TIMEOUT", "")

	// Answered in time again: the edits' own reports. The server says what
	// the reported edit ran into, and the hook which other edits went ahead
	// unchecked: the reported one once, in the server's words.
	_, _, ctx := decision(t, run("alice", a, "PostToolUse", "a-1", edit(a, "svc/pay/retry.go")))
	for _, want := range []string{"An edit this session made at", "may be working on docs/guide.md. Check it",
		"Your edit of svc/pay/retry.go was not checked before it ran", "bob's agent", "retry.go is mine today"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("after the unchecked edit, missing %q in:\n%s", want, ctx)
		}
	}
	if strings.Count(ctx, "svc/pay/retry.go") != 1 {
		t.Errorf("the reported edit told more than once:\n%s", ctx)
	}
	_, _, ctx = decision(t, run("alice", a, "PostToolUse", "a-2", edit(a, "docs/guide.md")))
	if !strings.Contains(ctx, "docs/guide.md was not checked") || !strings.Contains(ctx, "Undo your change") || strings.Contains(ctx, "without a check") {
		t.Errorf("after the edit inside bob's reservation:\n%s", ctx)
	}
	if dec, reason, _ := decision(t, run("alice", a, "PreToolUse", "a-3", edit(a, "svc/pay/retry.go"))); dec != "deny" || !strings.Contains(reason, "bob") {
		t.Errorf("next edit of bob's file: %q, want the bump the late answer did not spend", dec)
	}

	st := stats()
	if st.Checks != 4 || st.Bumped != 2 || st.Refused != 0 || st.Unheard != 2 {
		t.Errorf("stats count edits nobody was told about: %+v", st)
	}
	waitFor(t, "the webhook", func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.texts) >= 3 })
	time.Sleep(100 * time.Millisecond) // anything else the notifier would send
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var refusals, breaches int
	for _, text := range sink.texts {
		switch {
		case strings.Contains(text, "was refused an edit"):
			refusals++
		case strings.Contains(text, "changed a reserved file without a check") && strings.Contains(text, "docs/guide.md"):
			breaches++
		}
	}
	if len(sink.texts) != 3 || refusals != 2 || breaches != 1 {
		t.Errorf("webhook: %q; want the two bumps alice saw and her unchecked edit of bob's reserved file", sink.texts)
	}
}

// meterLargeBodies puts a front before the team's server, before anyone logs
// in, that answers every hook body over 16 KB with status, unread, as the
// server answers a footprint when its member's budget for large bodies is
// spent (429) or no slot frees for it (503). The tests that use it send no
// large pre_edit, which the server never refuses. It counts the bodies it
// refused.
func (tm *team) meterLargeBodies(status int) *atomic.Int32 {
	tm.t.Helper()
	var refused atomic.Int32
	h := tm.srv.Handler()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hook" && r.ContentLength > 16<<10 {
			refused.Add(1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"this member is sending more data than the server takes at once; try again in a second"}`)
			return
		}
		h.ServeHTTP(w, r)
	}))
	tm.t.Cleanup(front.Close)
	tm.url = front.URL
	return &refused
}

// A session's start, stop and end whose footprint the server will not take
// for load still reach the board: the hook sends each again without its
// footprint. Lost, a stop would leave the session working, to be announced
// as stalled, and an end would leave it live, its reservation refusing
// teammates' edits for hours. The footprint is not recorded as sent, so the
// next scan sends it.
func TestLifecycleEventsGoThroughWithoutTheFootprintRefusedForLoad(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			tm := newTeam(t, "alice", "bob")
			refused := tm.meterLargeBodies(status)
			a, b := tm.clone("alice"), tm.clone("bob")
			tm.enrol(map[string]string{"alice": a, "bob": b})
			// A codemod's output: a footprint of about 25 KB.
			for i := range 300 {
				writeFile(t, filepath.Join(b, fmt.Sprintf("gen/codemod/output/file_%04d.go", i)), "package output\n")
			}
			run := func(member, dir, event string, extra map[string]any) string {
				t.Helper()
				out, errOut, code := tm.as(member, dir, claudeEvent(member[:1]+"1", dir, event, extra), "hook", "claude-code")
				if code != 0 {
					t.Fatalf("%s %s: exit %d %s", member, event, code, errOut)
				}
				return out
			}
			board1 := tm.srv.Board()
			state := func() board.State {
				t.Helper()
				for _, r := range board1.Repos(time.Now()) {
					for _, c := range board1.View(time.Now(), r.Repo).Claims {
						if c.Member == "bob" && len(c.Sessions) == 1 {
							return c.Sessions[0].State
						}
					}
				}
				t.Fatal("bob's session is not on the board")
				return ""
			}

			if _, _, ctx := decision(t, run("bob", b, "SessionStart", map[string]any{"source": "startup"})); !strings.Contains(ctx, "bob's agent") ||
				refused.Load() != 1 {
				t.Fatalf("session start, %d bodies refused: %q", refused.Load(), ctx)
			}
			if out, errOut, code := tm.as("bob", b, "", "declare", "-x", "-m", "Move the retry policy", "svc/pay/**"); code != 0 {
				t.Fatalf("declare: %s %s", out, errOut)
			}
			run("bob", b, "UserPromptSubmit", map[string]any{"prompt": "Payments"})
			run("bob", b, "Stop", map[string]any{})
			if s := state(); s != board.StateWaiting || refused.Load() != 2 {
				t.Fatalf("after a stop, %d bodies refused: bob's session is %s", refused.Load(), s)
			}
			later := time.Now().Add(time.Hour)
			board1.Sweep(later)
			for _, r := range board1.Repos(later) {
				for _, act := range board1.View(later, r.Repo).Recent {
					if act.Kind == board.ActivitySessionStalled {
						t.Fatalf("a session that stopped is announced as stuck: %+v", act)
					}
				}
			}

			run("bob", b, "SessionEnd", map[string]any{"reason": "exit"})
			if s := state(); s != board.StateEnded || refused.Load() != 3 {
				t.Fatalf("after a session's end, %d bodies refused: bob's session is %s", refused.Load(), s)
			}
			if _, err := os.Stat(filepath.Join(cacheDir(), "sent-"+worktreeKey(b))); !os.IsNotExist(err) {
				t.Fatalf("a footprint the server refused is recorded as sent: %v", err)
			}
			// bob's reservation bumps alice once now that his agent has ended.
			edit := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "svc/pay/retry.go")}}
			run("alice", a, "UserPromptSubmit", map[string]any{"prompt": "Tidy up"})
			run("alice", a, "PreToolUse", edit)
			if dec, reason, _ := decision(t, run("alice", a, "PreToolUse", edit)); dec == "deny" {
				t.Fatalf("alice's second try at bob's file after his agent ended: %s %s", dec, reason)
			}
		})
	}
}
