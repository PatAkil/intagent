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
	ts.do(t, http.MethodPost, "/v1/intents", "alice", board.DeclareRequest{Where: where("alice"), Summary: "retry", Patterns: []string{"svc/**"}, Mode: board.ModeExclusive}, nil)
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
		{board.Activity{Kind: "conflict", Decision: board.DecisionRefuse}, true},
		{board.Activity{Kind: "conflict", Decision: board.DecisionAllow}, false},
		{board.Activity{Kind: "conflict", Decision: board.DecisionAllow, Severity: board.SeverityBlock}, false}, // a reservation that only warns
		{board.Activity{Kind: "conflict", Decision: board.DecisionAllow, Severity: board.SeverityBlock, Breach: true}, true},
		{board.Activity{Kind: "file.changed"}, false},
		{board.Activity{Kind: "session.gone"}, true},
		{board.Activity{Kind: "session.gone", Idle: true}, false}, // left open, not stuck
	} {
		if n.wants(c.a) != c.want {
			t.Errorf("wants(%+v) = %v", c.a, !c.want)
		}
	}
	custom := newNotifier(WebhookConfig{URL: "http://x", Events: []board.ActivityKind{board.ActivityNoteSent}}, nil)
	if !custom.wants(board.Activity{Kind: "note.sent"}) || custom.wants(board.Activity{Kind: "session.stalled"}) {
		t.Error("custom event list not honoured")
	}
	idle := newNotifier(WebhookConfig{URL: "http://x", Idle: true}, nil)
	if !idle.wants(board.Activity{Kind: "session.gone", Idle: true}) {
		t.Error("idle: true does not send sessions left open")
	}
}

// Every evening people leave their agents waiting; two hours later each
// session turns gone. Only the one that went quiet mid-turn is worth a message.
func TestWebhookLeavesSessionsLeftOpenToTheDashboard(t *testing.T) {
	got := make(chan webhookPayload, 10)
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		got <- p
	}))
	defer hook.Close()
	clock := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
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
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindStop, "alice", "a1"), nil) // left waiting
	ts.do(t, http.MethodPost, "/v1/hook", "bob", hookEv(board.KindPrompt, "bob", "b1"), nil)   // never heard of again
	ts.Board().Sweep(clock.Add(3 * time.Hour))

	select {
	case p := <-got:
		if p.Activity.Kind != "session.gone" || p.Activity.Member != "bob" || p.Activity.Idle {
			t.Fatalf("payload = %+v, want bob's session.gone alone", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook for the session that went quiet mid-turn")
	}
	feed := ts.Board().View(clock.Add(3*time.Hour), repo).Recent
	if n := len(feed); n == 0 || !feed[n-2].Idle || feed[n-2].Member != "alice" {
		t.Errorf("the feed should still show alice's session as gone, marked idle: %+v", feed)
	}
}

// Slack reads <...> as mentions and links; member text must not become either.
func TestWebhookEscapesSlackMarkup(t *testing.T) {
	a := board.Activity{Kind: "conflict", Member: "bob", Agent: board.AgentCodex, Repo: "github.com/acme/mono", Decision: board.DecisionRefuse,
		Text: `svc/a.go → alice (declared exclusive intent svc/**: "<!channel> & <https://evil.example|the runbook>")`}
	got := slackText.Replace(describeActivity(a))
	if strings.ContainsAny(got, "<>") || !strings.Contains(got, "&lt;!channel&gt; &amp; &lt;https://evil.example|the runbook&gt;") {
		t.Fatalf("text = %s", got)
	}
	for _, kind := range []board.ActivityKind{"session.stalled", "session.gone", "conflict", "other"} {
		a := board.Activity{Kind: kind, Member: "m", Agent: "a", Repo: "r", Text: "t"}
		if s := describeActivity(a); strings.ContainsAny(s, "<>&") {
			t.Errorf("%s: the template itself uses Slack markup: %s", kind, s)
		}
	}
}
