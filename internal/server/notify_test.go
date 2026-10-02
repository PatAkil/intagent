package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

func TestWebhookSendsStallsAndRefusals(t *testing.T) {
	got := make(chan webhookPayload, 10)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		got <- p
	}))
	defer hook.Close()

	clock := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(o *Options) {
		o.Webhook = WebhookConfig{URL: hook.URL}
		o.Now = func() time.Time { return clock }
		o.SweepEvery = time.Hour
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ts.Serve(ctx, ln) }()
	ts.url = "http://" + ln.Addr().String()

	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	ts.do(t, http.MethodPost, "/v1/intents", "alice", board.DeclareRequest{Where: where("alice"), Summary: "retry", Patterns: []string{"svc/**"}, Mode: board.Exclusive}, nil)
	ts.do(t, http.MethodPost, "/v1/hook", "bob", hookEv(board.KindPreEdit, "bob", "b1", "svc/x.go"), nil)

	select {
	case p := <-got:
		if p.Activity.Kind != "conflict" || !strings.Contains(p.Text, "bob's claude-code agent was refused an edit") {
			t.Fatalf("payload = %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook for the refused edit")
	}

	ts.Board().Sweep(clock.Add(11 * time.Minute)) // alice's working session goes quiet
	select {
	case p := <-got:
		if p.Activity.Kind != "session.stalled" || !strings.Contains(p.Text, "alice's claude-code agent") || !strings.Contains(p.Text, "looks stuck") {
			t.Fatalf("payload = %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook for the stalled session")
	}
}

func TestWebhookFilters(t *testing.T) {
	n := newNotifier(WebhookConfig{URL: "http://x"}, nil)
	for _, c := range []struct {
		a    board.Activity
		want bool
	}{
		{board.Activity{Kind: "session.stalled"}, true},
		{board.Activity{Kind: "conflict", Decision: board.Refuse}, true},
		{board.Activity{Kind: "conflict", Decision: board.Allow}, false},
		{board.Activity{Kind: "file.changed"}, false},
	} {
		if n.wants(c.a) != c.want {
			t.Errorf("wants(%+v) = %v", c.a, !c.want)
		}
	}
	custom := newNotifier(WebhookConfig{URL: "http://x", Events: []string{"note.sent"}}, nil)
	if !custom.wants(board.Activity{Kind: "note.sent"}) || custom.wants(board.Activity{Kind: "session.stalled"}) {
		t.Error("custom event list not honoured")
	}
}
