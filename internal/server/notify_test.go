package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	ts.notifier.pace = 10 * time.Millisecond
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

// A message about one activity is byte for byte what the notifier always
// sent, so existing consumers read it as before.
func TestWebhookOneActivityIsTheMessageItAlwaysWas(t *testing.T) {
	var got []byte
	var key string
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		key = r.Header.Get("Idempotency-Key")
	}))
	defer hook.Close()
	at := time.Date(2026, 10, 2, 9, 30, 5, 0, time.UTC)
	for _, c := range []struct {
		a    board.Activity
		want string
	}{
		{
			board.Activity{Seq: 42, At: at, Kind: board.ActivityConflict, Repo: "github.com/acme/mono", Member: "bob", ClaimID: "c-bob1", Session: "b1",
				Agent: board.AgentClaudeCode, Paths: []string{"svc/x.go"}, Severity: board.SeverityBlock, Decision: board.DecisionRefuse,
				Also: []string{"carol on svc/x.go: has unmerged changes to this file"},
				Text: `svc/x.go → alice (declared exclusive intent svc/**: "retry <b> & co")`},
			`{"text":"intagent: bob's claude-code agent was refused an edit in github.com/acme/mono: svc/x.go → alice (declared exclusive intent svc/**: \"retry \u0026lt;b\u0026gt; \u0026amp; co\"); also carol on svc/x.go: has unmerged changes to this file.","activity":{"seq":42,"at":"2026-10-02T09:30:05Z","kind":"conflict","repo":"github.com/acme/mono","member":"bob","claim_id":"c-bob1","session":"b1","agent":"claude-code","paths":["svc/x.go"],"text":"svc/x.go → alice (declared exclusive intent svc/**: \"retry \u003cb\u003e \u0026 co\")","severity":"block","decision":"deny","also":["carol on svc/x.go: has unmerged changes to this file"]}}`,
		},
		{
			board.Activity{Seq: 7, At: at, Kind: board.ActivitySessionStalled, Repo: "github.com/acme/mono", Member: "alice", ClaimID: "c-al1", Session: "a1",
				Agent: board.AgentCodex, Text: "silent for 11m"},
			`{"text":"intagent: alice's codex agent in github.com/acme/mono looks stuck: silent for 11m.","activity":{"seq":7,"at":"2026-10-02T09:30:05Z","kind":"session.stalled","repo":"github.com/acme/mono","member":"alice","claim_id":"c-al1","session":"a1","agent":"codex","text":"silent for 11m"}}`,
		},
	} {
		n := newNotifier(WebhookConfig{URL: hook.URL}, nil)
		n.enqueue([]board.Activity{c.a})
		n.flush(context.Background())
		if string(got) != c.want {
			t.Errorf("payload\n%s\nwant\n%s", got, c.want)
		}
		if key != strconv.FormatUint(c.a.Seq, 10) {
			t.Errorf("Idempotency-Key = %q, want the activity's seq", key)
		}
	}
}

// Several activities make one message: collisions first, a group of more than
// five summed up, smaller ones told line by line, and what could not be
// queued counted.
func TestWebhookBatchMessage(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 30, 5, 0, time.UTC)
	var items []*pendingItem
	add := func(a board.Activity) {
		a.Seq = uint64(len(items) + 1)
		a.At = at.Add(time.Duration(len(items)) * time.Second)
		items = append(items, &pendingItem{act: a})
	}
	add(stall("alice", "github.com/acme/web", "a1"))
	for i := range 6 {
		a := refusal([]string{"bob", "bob", "carol", "bob", "dave", "carol"}[i], "github.com/acme/mono", fmt.Sprint("s", i), "svc/x.go")
		if i == 4 {
			a.Decision, a.Breach = board.DecisionAllow, true
		}
		add(a)
	}
	add(board.Activity{Kind: board.ActivitySessionGone, Repo: "github.com/acme/web", Member: "erin", Agent: board.AgentCursor, Text: "silent for 2h"})
	add(stall("frank", "github.com/acme/web", "f1"))
	var counted counts
	for i, kind := range []board.ActivityKind{board.ActivitySessionStalled, board.ActivityConflict, board.ActivitySessionStalled, board.ActivitySessionStalled} {
		counted.add(board.Activity{Seq: uint64(10 + i), Kind: kind})
	}
	m := compose(items, counted)

	want := strings.Join([]string{
		"intagent: 6 collisions in github.com/acme/mono, between 09:30:06 and 09:30:11 UTC: 5 edits refused, 1 reserved file changed without a check. Members: bob (3), carol (2), dave. Files: svc/x.go (6).",
		"intagent: alice's claude-code agent in github.com/acme/web looks stuck: silent for 10m.",
		"intagent: frank's claude-code agent in github.com/acme/web looks stuck: silent for 10m.",
		"intagent: erin's cursor agent in github.com/acme/web stopped reporting (silent for 2h) without ending its session.",
		"intagent: 4 more came while the webhook was behind, too many to list: 1 collision, 3 stuck agents. " +
			"Some of these agents may have reported again since; the dashboard shows which.",
	}, "\n")
	if m.text != want {
		t.Errorf("text\n%s\nwant\n%s", m.text, want)
	}
	var p batchPayload
	body, _ := m.payload()
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	order := ""
	for _, a := range p.Activities {
		order += fmt.Sprint(a.Seq, " ")
	}
	if p.Count != 13 || p.More != 4 || order != "2 3 4 5 6 7 1 9 8 " || p.Activity == nil || p.Activity.Seq != 2 || m.key != "1-13-13" {
		t.Errorf("count %d, more %d, activities %s, first %+v, key %s", p.Count, p.More, order, p.Activity, m.key)
	}
}

// A message left with only counts, as when every alert it listed was withdrawn
// while the endpoint failed, still lists its activities, if none, and has a
// key of its own; it does not say that the agents it counts are still stuck.
func TestWebhookCountsOnlyMessage(t *testing.T) {
	count := func(kind board.ActivityKind, seqs ...uint64) counts {
		var c counts
		for _, seq := range seqs {
			c.add(board.Activity{Seq: seq, Kind: kind})
		}
		return c
	}
	keys := map[string]string{}
	for _, c := range []struct {
		counted counts
		want    string
	}{
		{count(board.ActivitySessionStalled, 5001, 5002, 5003),
			`{"text":"intagent: 3 more came while the webhook was behind, too many to list: 3 stuck agents. ` +
				`Some of these agents may have reported again since; the dashboard shows which.","count":3,"activities":[],"more":3}`},
		{count(board.ActivitySessionStalled, 9001, 9002, 9003), ""},
		{count(board.ActivityConflict, 9101, 9102, 9103),
			`{"text":"intagent: 3 more came while the webhook was behind, too many to list: 3 collisions.","count":3,"activities":[],"more":3}`},
	} {
		m := compose(nil, c.counted)
		body, err := m.payload()
		if err != nil {
			t.Fatal(err)
		}
		if c.want != "" && string(body) != c.want {
			t.Errorf("payload\n%s\nwant\n%s", body, c.want)
		}
		if other, ok := keys[m.key]; ok {
			t.Errorf("Idempotency-Key %q on two messages:\n%s\n%s", m.key, other, body)
		}
		keys[m.key] = string(body)
	}
}

// However many repositories and activities, a message stays readable: at most
// 40 lines and 12,000 characters as sent, Slack's markup escaped, and what
// does not fit is counted.
func TestWebhookMessageIsBounded(t *testing.T) {
	long := func(i int) board.Activity {
		a := refusal("bob", fmt.Sprintf("github.com/acme/r%d", i%5), fmt.Sprint("s", i), strings.Repeat("deep/", 150)+"x.go")
		if i >= 25 {
			a = stall(fmt.Sprint("m", i), fmt.Sprintf("github.com/acme/r%d", i%5), fmt.Sprint("s", i))
		}
		a.Text = strings.Repeat("y", 400)
		return a
	}
	// Five collisions in each of five repositories are told a line each, and
	// a summary full of markup grows more than fourfold once escaped.
	markup := func(i int) board.Activity {
		a := refusal("bob", fmt.Sprintf("github.com/acme/r%d", i%5), fmt.Sprint("s", i), "svc/x.go")
		quote := strings.Repeat("<&>", 66)
		a.Text = "svc/x.go → alice (declared exclusive intent svc/**: \"" + quote + "\")"
		for _, who := range []string{"carol", "dave", "erin"} {
			a.Also = append(a.Also, who+" on svc/x.go: declared shared intent svc/**: \""+quote+"\"")
		}
		return a
	}
	for _, c := range []struct {
		name string
		n    int
		act  func(int) board.Activity
	}{
		{"long lines", 2000, long},
		{"markup", 25, markup},
	} {
		var items []*pendingItem
		for i := range c.n {
			a := c.act(i)
			a.Seq = uint64(i + 1)
			items = append(items, &pendingItem{act: a})
		}
		m := compose(items, counts{})
		lines := strings.Split(m.text, "\n")
		if len(lines) > maxMessageLines || len(m.text) > maxMessageText || m.count != c.n || strings.ContainsAny(m.text, "<>") {
			t.Fatalf("%s: %d lines, %d bytes, count %d:\n%.300s", c.name, len(lines), len(m.text), m.count, m.text)
		}
		if last := lines[len(lines)-1]; !strings.HasPrefix(last, "intagent: and ") {
			t.Errorf("%s: last line %q", c.name, last)
		}
	}
}

// Draining each answer lets one connection carry every post; before, an
// endpoint answering "ok", as Slack does, cost a new connection per message.
func TestWebhookReusesItsConnection(t *testing.T) {
	var conns atomic.Int64
	hook := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	hook.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	hook.Start()
	defer hook.Close()
	n := newNotifier(WebhookConfig{URL: hook.URL}, nil)
	for i := range 20 {
		a := stall("alice", repo, "a1")
		a.Seq = uint64(i + 1)
		if ans := n.post(context.Background(), compose([]*pendingItem{{act: a}}, counts{})); ans.err != nil {
			t.Fatal(ans.err)
		}
	}
	if c := conns.Load(); c != 1 {
		t.Errorf("20 posts opened %d connections", c)
	}
}

// serveWithWebhook runs a server whose webhook posts to hook, and returns a
// stop that cancels Serve and waits for it to return.
func serveWithWebhook(t *testing.T, hook string, logs *logRecorder) (*testServer, func()) {
	t.Helper()
	clock := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(o *Options) {
		o.Webhook = WebhookConfig{URL: hook}
		o.Now = func() time.Time { return clock }
		o.SweepEvery = time.Hour
		if logs != nil {
			o.Logger = slog.New(logs)
		}
	})
	ts.notifier.pace = time.Hour // only the first message goes before the stop
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Serve(ctx, ln) }()
	ts.url = "http://" + ln.Addr().String()
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	ts.do(t, http.MethodPost, "/v1/intents", "alice", board.DeclareRequest{Where: where("alice"), Summary: "retry", Patterns: []string{"svc/**"}, Mode: board.ModeExclusive}, nil)
	return ts, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return")
		}
	}
}

// What waits for the pace when the server stops goes out in one last message
// before Serve returns.
func TestServeSendsTheWebhookWhatWaitsAsItStops(t *testing.T) {
	var mu sync.Mutex
	var got []string
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got = append(got, p.Text)
		mu.Unlock()
	}))
	defer hook.Close()
	ts, stop := serveWithWebhook(t, hook.URL, nil)
	ts.do(t, http.MethodPost, "/v1/hook", "bob", hookEv(board.KindPreEdit, "bob", "b1", "svc/x.go"), nil) // goes at once
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no webhook for the first refusal")
		}
	}
	ts.do(t, http.MethodPost, "/v1/hook", "bob", hookEv(board.KindPreEdit, "bob", "b1", "svc/y.go"), nil) // waits for the pace
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !strings.Contains(got[1], "svc/y.go") {
		t.Fatalf("sent %q, want the second refusal sent as the server stopped", got)
	}
}

// A post under way when the server stops is not cut short.
func TestServeLetsAWebhookPostFinish(t *testing.T) {
	started, handled := make(chan struct{}, 1), make(chan bool, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // so the server notices a client that hangs up
		started <- struct{}{}
		select {
		case <-time.After(300 * time.Millisecond):
			handled <- true
		case <-r.Context().Done():
			handled <- false
		}
	}))
	defer hook.Close()
	logs := &logRecorder{}
	ts, stop := serveWithWebhook(t, hook.URL, logs)
	ts.do(t, http.MethodPost, "/v1/hook", "bob", hookEv(board.KindPreEdit, "bob", "b1", "svc/x.go"), nil)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook post")
	}
	stop()
	select {
	case finished := <-handled:
		if !finished {
			t.Fatal("the post was cancelled as the server stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the endpoint never finished the post")
	}
	if failed := logs.lines("webhook failed"); len(failed) != 0 {
		t.Errorf("logged %q", failed)
	}
}
