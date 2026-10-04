package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

	// Answered in time again: the edits' own reports.
	_, _, ctx := decision(t, run("alice", a, "PostToolUse", "a-1", edit(a, "svc/pay/retry.go")))
	for _, want := range []string{"Your edit of svc/pay/retry.go was not checked before it ran", "bob's agent", "retry.go is mine today"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("after the unchecked edit, missing %q in:\n%s", want, ctx)
		}
	}
	_, _, ctx = decision(t, run("alice", a, "PostToolUse", "a-2", edit(a, "docs/guide.md")))
	if !strings.Contains(ctx, "docs/guide.md was not checked") || !strings.Contains(ctx, "Undo your change") {
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
