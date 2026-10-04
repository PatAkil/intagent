package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// The webhook scenarios run the notifier's sender in fake time against fake
// endpoints, so a storm of minutes takes milliseconds and every count is
// exact: the clock moves only when the sender waits or an endpoint answers.
// The sender waits on its own timer, so what the scenarios measure is the
// waiting that ships.

var stormStart = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

// fakeClock is the sender's clock in a scenario.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// after is the sender's timer: the wait passes at once, and the clock with it.
func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.t = c.t.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.t
	return ch
}

// fakeEndpoint answers posts in fake time. It accepts everything after a
// round trip, or only rate messages a second as Slack does (429 with
// Retry-After beyond it), or fails, or never answers before the client's
// timeout.
type fakeEndpoint struct {
	clock      *fakeClock
	rtt        time.Duration
	rate       float64 // messages a second it accepts; 0 accepts all
	burst      float64
	retryAfter string // the Retry-After of a 429; empty sends none
	hang       bool   // never answers within the client's timeout
	script     []int  // answers these statuses first, in order
	fail       int    // answers 503 to this many posts first
	refuse     int    // answers this status to every post
	during     func() // runs once while the next post is in flight

	tokens    float64
	last      time.Time
	blocked   time.Time // a 429's Retry-After runs until then
	early     int       // posts that came while a Retry-After ran
	requests  []time.Time
	answers   map[int]int
	delivered []delivery
}

// delivery is a post the endpoint accepted.
type delivery struct {
	at    time.Time // when its answer reached the sender
	key   string
	text  string
	count int
	acts  []board.Activity
	more  int
	batch bool
}

func (e *fakeEndpoint) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	start := e.clock.now()
	e.requests = append(e.requests, start)
	if e.answers == nil {
		e.answers = map[int]int{}
	}
	if e.hang {
		e.clock.t = start.Add(webhookTimeout)
		return nil, context.DeadlineExceeded
	}
	if f := e.during; f != nil {
		e.during = nil
		f()
	}
	e.clock.t = start.Add(e.rtt)
	status, header := e.decide(start), http.Header{}
	e.answers[status]++
	switch status {
	case http.StatusOK:
		var p struct {
			Text       string           `json:"text"`
			Activity   board.Activity   `json:"activity"`
			Count      *int             `json:"count"`
			Activities []board.Activity `json:"activities"`
			More       int              `json:"more"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		d := delivery{at: e.clock.t, key: req.Header.Get("Idempotency-Key"), text: p.Text, count: 1, acts: []board.Activity{p.Activity}}
		if p.Count != nil {
			d.batch, d.count, d.acts, d.more = true, *p.Count, p.Activities, p.More
		}
		e.delivered = append(e.delivered, d)
	case http.StatusTooManyRequests:
		if e.retryAfter != "" {
			header.Set("Retry-After", e.retryAfter)
		}
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: header, Request: req,
		Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func (e *fakeEndpoint) decide(now time.Time) int {
	if len(e.script) > 0 {
		status := e.script[0]
		e.script = e.script[1:]
		return status
	}
	if e.refuse != 0 {
		return e.refuse
	}
	if e.fail > 0 {
		e.fail--
		return http.StatusServiceUnavailable
	}
	if e.rate == 0 {
		return http.StatusOK
	}
	if now.Before(e.blocked) {
		e.early++
	}
	if e.last.IsZero() {
		e.tokens, e.last = e.burst, now
	}
	e.tokens = math.Min(e.burst, e.tokens+now.Sub(e.last).Seconds()*e.rate)
	e.last = now
	if e.tokens < 1 {
		secs, _ := strconv.Atoi(e.retryAfter)
		e.blocked = now.Add(time.Duration(max(secs, 1)) * time.Second)
		return http.StatusTooManyRequests
	}
	e.tokens--
	return http.StatusOK
}

// The endpoints every scenario runs against.
func endpoints() map[string]func() *fakeEndpoint {
	return map[string]func() *fakeEndpoint{
		"instant": func() *fakeEndpoint { return &fakeEndpoint{} },
		"slow 1s": func() *fakeEndpoint { return &fakeEndpoint{rtt: time.Second} },
		"slack 1/s burst 1": func() *fakeEndpoint {
			return &fakeEndpoint{rtt: 100 * time.Millisecond, rate: 1, burst: 1, retryAfter: "1"}
		},
	}
}

// storm drives a notifier in fake time.
type storm struct {
	t     *testing.T
	n     *notifier
	clock *fakeClock
	ep    *fakeEndpoint
	logs  *logRecorder
	seq   uint64
	sent  map[uint64]board.Activity // every activity the board handed over
}

func newStorm(t *testing.T, ep *fakeEndpoint, cfg WebhookConfig) *storm {
	t.Helper()
	clock := &fakeClock{t: stormStart}
	ep.clock = clock
	logs := &logRecorder{}
	cfg.URL = "https://hooks.example.com/services/x"
	n := newNotifier(cfg, slog.New(logs))
	n.client = &http.Client{Transport: ep, Timeout: webhookTimeout}
	n.now, n.after = clock.now, clock.after
	return &storm{t: t, n: n, clock: clock, ep: ep, logs: logs, sent: map[uint64]board.Activity{}}
}

// until runs the sender up to fake time t: whenever something is queued and
// its turn comes before t, the sender waits for it and posts, as run does.
func (s *storm) until(t time.Time) {
	s.t.Helper()
	for {
		at, ok := s.n.nextSend()
		if !ok || at.After(t) {
			break
		}
		if !s.n.waitForTurn(context.Background()) {
			s.t.Fatal("the sender stopped waiting")
		}
		if s.clock.t.Before(at) {
			s.t.Fatalf("the sender went at %s, before its turn at %s", s.clock.t.Sub(stormStart), at.Sub(stormStart))
		}
		s.n.flush(context.Background())
	}
	if t.After(s.clock.t) {
		s.clock.t = t
	}
}

// at hands the notifier activities at time t, as one board unlock would.
func (s *storm) at(t time.Time, acts ...board.Activity) {
	s.until(t)
	for i := range acts {
		s.seq++
		acts[i].Seq, acts[i].At = s.seq, t
		s.sent[s.seq] = acts[i]
	}
	s.n.enqueue(acts)
}

// coverage checks that every wanted activity was delivered exactly once:
// listed by seq where the message lists them all, and counted otherwise.
func (s *storm) coverage(wanted int) {
	s.t.Helper()
	seen := map[uint64]bool{}
	counted := 0
	for _, d := range s.ep.delivered {
		counted += d.count
		if d.more > 0 {
			continue
		}
		for _, a := range d.acts {
			if seen[a.Seq] {
				s.t.Errorf("activity %d delivered twice", a.Seq)
			}
			seen[a.Seq] = true
		}
	}
	if counted != wanted {
		s.t.Errorf("messages covered %d activities, want %d", counted, wanted)
	}
	if n := s.n.queued(); n != 0 {
		s.t.Errorf("%d activities still queued", n)
	}
	if n := s.n.held(); n != 0 {
		s.t.Errorf("%d sessions' alerts still held for pairing", n)
	}
}

func (n *notifier) held() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.alerts)
}

func (n *notifier) queued() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.pending) + n.counted.total()
}

// logRecorder keeps what the notifier logs.
type logRecorder struct {
	mu   sync.Mutex
	recs []string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecorder) WithGroup(string) slog.Handler            { return l }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	line := r.Message
	r.Attrs(func(a slog.Attr) bool { line += " " + a.String(); return true })
	l.mu.Lock()
	l.recs = append(l.recs, line)
	l.mu.Unlock()
	return nil
}

func (l *logRecorder) lines(prefix string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.recs {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func stall(member, repo, session string) board.Activity {
	return board.Activity{Kind: board.ActivitySessionStalled, Repo: repo, Member: member, Session: session, Agent: board.AgentClaudeCode,
		Text: "silent for 10m"}
}

func refusal(member, repo, session, path string) board.Activity {
	return board.Activity{Kind: board.ActivityConflict, Repo: repo, Member: member, Session: session, Agent: board.AgentClaudeCode,
		Paths: []string{path}, Severity: board.SeverityBlock, Decision: board.DecisionRefuse,
		Text: path + " → alice (declared exclusive intent svc/**: \"retry\")"}
}

// A network blip at the target: 750 working agents go quiet together and
// the sweeper announces them over four sweeps, while 20 edits are refused.
// Before, a Slack-like endpoint received 28 of these 770 and refused 742.
func TestWebhookBlipIsAFewMessagesAndLosesNothing(t *testing.T) {
	for name, endpoint := range endpoints() {
		t.Run(name, func(t *testing.T) {
			s := newStorm(t, endpoint(), WebhookConfig{})
			refusedAt := map[uint64]time.Time{}
			for sweep := range 4 {
				at := stormStart.Add(time.Duration(sweep) * 15 * time.Second)
				var acts []board.Activity
				for i := sweep * 188; i < min((sweep+1)*188, 750); i++ {
					acts = append(acts, stall(fmt.Sprintf("m%03d", i%200), fmt.Sprintf("github.com/acme/r%02d", i%30), fmt.Sprintf("s%d", i)))
				}
				s.at(at, acts...)
				for k := range 5 { // refused edits land between sweeps
					when := at.Add(time.Duration(k)*2900*time.Millisecond + 700*time.Millisecond)
					s.at(when, refusal("bob", "github.com/acme/r00", fmt.Sprintf("b%d", sweep*5+k), "svc/x.go"))
					refusedAt[s.seq] = when
				}
			}
			s.until(stormStart.Add(5 * time.Minute))

			s.coverage(770)
			if n := len(s.ep.delivered); n > 30 {
				t.Errorf("%d messages, want at most 30", n)
			}
			if s.ep.answers[http.StatusTooManyRequests] != 0 || s.ep.early != 0 {
				t.Errorf("rate-limited %d times, %d posts inside a Retry-After", s.ep.answers[http.StatusTooManyRequests], s.ep.early)
			}
			// A refused edit reaches a person within the pace and a round trip.
			for _, d := range s.ep.delivered {
				for _, a := range d.acts {
					if at, ok := refusedAt[a.Seq]; ok {
						if late := d.at.Sub(at); late > s.n.pace+s.ep.rtt {
							t.Errorf("refusal %d arrived %s after it happened", a.Seq, late)
						}
						delete(refusedAt, a.Seq)
					}
				}
			}
			if len(refusedAt) != 0 {
				t.Errorf("%d refusals not listed in any message", len(refusedAt))
			}
		})
	}
}

// An orchestrator starts 500 agents onto one reservation: 1,500 refused edits
// in 16 seconds. Before, that was 1,500 posts, of which Slack took 18.
func TestWebhookConflictStormIsAFewMessages(t *testing.T) {
	for name, endpoint := range endpoints() {
		t.Run(name, func(t *testing.T) {
			s := newStorm(t, endpoint(), WebhookConfig{})
			for i := range 1500 {
				at := stormStart.Add(time.Duration(i) * 16 * time.Second / 1500)
				s.at(at, refusal(fmt.Sprintf("fleet%02d", i%50), "github.com/acme/mono", fmt.Sprintf("f%d", i/3), fmt.Sprintf("svc/f%d.go", i%3)))
			}
			s.until(stormStart.Add(time.Minute))
			s.coverage(1500)
			if n := len(s.ep.delivered); n > 20 {
				t.Errorf("%d messages, want at most 20", n)
			}
			last := s.ep.delivered[len(s.ep.delivered)-1].text
			if !strings.Contains(last, "collisions in github.com/acme/mono") || !strings.Contains(last, "edits refused") {
				t.Errorf("a storm's message should sum it up: %s", last)
			}
		})
	}
}

// Posts that fail are retried after 1, 2, 4 ... seconds, and an activity is
// dropped, with one log line, after six posts.
func TestWebhookBacksOffAndGivesUp(t *testing.T) {
	ep := &fakeEndpoint{fail: 1 << 30}
	s := newStorm(t, ep, WebhookConfig{})
	s.at(stormStart, stall("alice", "github.com/acme/mono", "a1"))
	s.until(stormStart.Add(5 * time.Minute))
	var offsets []string
	for _, r := range ep.requests {
		offsets = append(offsets, r.Sub(stormStart).String())
	}
	if got := strings.Join(offsets, " "); got != "0s 1s 3s 7s 15s 31s" {
		t.Errorf("posts at %s, want 0s 1s 3s 7s 15s 31s", got)
	}
	if got := s.logs.lines("webhook gave up"); len(got) != 1 || !strings.Contains(got[0], "dropped=1") {
		t.Errorf("logged %q", got)
	}
	if s.n.queued() != 0 {
		t.Error("the dropped activity is still queued")
	}

	// An endpoint back after two failures gets what waited, merged with
	// what came since, in one message.
	ep = &fakeEndpoint{fail: 2}
	s = newStorm(t, ep, WebhookConfig{})
	s.at(stormStart, stall("alice", "github.com/acme/mono", "a1"))
	s.at(stormStart.Add(2*time.Second), stall("bob", "github.com/acme/mono", "b1"))
	s.until(stormStart.Add(time.Minute))
	if len(ep.delivered) != 1 || ep.delivered[0].count != 2 || ep.delivered[0].at != stormStart.Add(3*time.Second) {
		t.Errorf("delivered %+v, want both at 3s", ep.delivered)
	}
}

// A 429 holds the sender for its Retry-After, at most a minute, and the same
// activities go again; it does not count against them.
func TestWebhookWaitsOutRateLimits(t *testing.T) {
	for _, c := range []struct {
		header string
		wait   time.Duration
	}{
		{"", time.Second},
		{"3", 3 * time.Second},
		{"3600", maxRetryAfter},
		{stormStart.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
	} {
		ep := &fakeEndpoint{refuse: http.StatusTooManyRequests}
		s := newStorm(t, ep, WebhookConfig{})
		ep.retryAfter = c.header
		s.at(stormStart, stall("alice", "github.com/acme/mono", "a1"))
		s.until(stormStart.Add(c.wait - time.Millisecond))
		if len(ep.requests) != 1 {
			t.Errorf("Retry-After %q: %d posts before %s", c.header, len(ep.requests), c.wait)
		}
		ep.refuse = 0
		s.until(stormStart.Add(20 * time.Minute))
		if len(ep.requests) != 2 || ep.requests[1] != stormStart.Add(c.wait) || len(ep.delivered) != 1 {
			t.Errorf("Retry-After %q: posts at %v, want the second at %s", c.header, ep.requests, c.wait)
		}
	}
	// Endless 429s never drop anything: the activities wait, bounded.
	ep := &fakeEndpoint{refuse: http.StatusTooManyRequests, retryAfter: "1"}
	s := newStorm(t, ep, WebhookConfig{})
	s.at(stormStart, stall("alice", "github.com/acme/mono", "a1"))
	s.until(stormStart.Add(10 * time.Minute))
	if s.n.queued() != 1 || len(s.logs.lines("webhook gave up")) != 0 {
		t.Errorf("queued %d after 10 minutes of 429s", s.n.queued())
	}
}

// A delivery or a refusal ends a run of failures, for what was only counted
// as for what was listed: an overflow that meets one failure, after an
// earlier one met five, still arrives whole.
func TestWebhookFailuresStartAfreshAfterAnAnswer(t *testing.T) {
	for _, end := range []int{http.StatusOK, http.StatusNotFound} {
		ep := &fakeEndpoint{script: append(slices.Repeat([]int{http.StatusServiceUnavailable}, maxAttempts-1), end)}
		s := newStorm(t, ep, WebhookConfig{})
		overflow := func(round int) {
			acts := make([]board.Activity, maxPending+1)
			for i := range acts {
				acts[i] = stall("m", repo, fmt.Sprint(round, "-", i))
			}
			s.at(stormStart.Add(time.Duration(round)*time.Minute), acts...)
		}
		overflow(0)
		s.until(stormStart.Add(time.Minute))
		ep.script = []int{http.StatusServiceUnavailable}
		overflow(1)
		s.until(stormStart.Add(5 * time.Minute))
		last := 0
		if n := len(ep.delivered); n > 0 {
			last = ep.delivered[n-1].count
		}
		if last != maxPending+1 {
			t.Errorf("after a %d: the last message covers %d activities, want %d", end, last, maxPending+1)
		}
		if got := s.logs.lines("webhook gave up"); len(got) != 0 {
			t.Errorf("after a %d: logged %q", end, got)
		}
	}
}

// run, the sender that ships, keeps the pace between messages, waits out a
// failure's backoff and a 429's Retry-After, and starts the backoff afresh
// after a delivery. Its clock moves only while it waits or the endpoint
// answers, so the times are exact.
func TestWebhookSenderWaitsItsTurn(t *testing.T) {
	const fail, slow = http.StatusServiceUnavailable, http.StatusTooManyRequests
	for _, c := range []struct {
		name       string
		rtt        time.Duration
		answers    []int // the endpoint's first answers; then it accepts
		retryAfter string
		want       string // when each post started
	}{
		{"the pace runs from start to start", 300 * time.Millisecond, nil, "", "0s 1s 2s"},
		{"failures back off, afresh after a delivery", 0, []int{fail, fail, http.StatusOK, fail}, "", "0s 1s 3s 4s 5s 6s"},
		{"a 429 holds the sender for its Retry-After", 0, []int{slow}, "3", "0s 3s 4s 5s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ep := &fakeEndpoint{rtt: c.rtt, script: c.answers, retryAfter: c.retryAfter}
			s := newStorm(t, ep, WebhookConfig{})
			delivered := make(chan struct{}, 8)
			s.n.client.Transport = deliveries{ep, delivered}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); s.n.run(ctx) }()
			// Each activity comes once the one before it was delivered.
			for i, who := range []string{"alice", "bob", "carol"} {
				a := stall(who, repo, who)
				a.Seq = uint64(i + 1)
				s.n.enqueue([]board.Activity{a})
				select {
				case <-delivered:
				case <-time.After(10 * time.Second):
					t.Fatalf("%s's stall was not delivered", who)
				}
			}
			cancel()
			<-done
			var got []string
			for _, r := range ep.requests {
				got = append(got, r.Sub(stormStart).String())
			}
			if g := strings.Join(got, " "); g != c.want {
				t.Errorf("posts at %s, want %s", g, c.want)
			}
		})
	}
}

// deliveries tells a test each time its endpoint accepts a post.
type deliveries struct {
	ep   *fakeEndpoint
	sent chan<- struct{}
}

func (d deliveries) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.ep.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		d.sent <- struct{}{}
	}
	return resp, err
}

// Any other refusal will not get better by trying again: the message is
// dropped, with a log line, and the endpoint is not hammered.
func TestWebhookDropsWhatTheEndpointRefuses(t *testing.T) {
	ep := &fakeEndpoint{refuse: http.StatusNotFound}
	s := newStorm(t, ep, WebhookConfig{})
	s.at(stormStart, stall("alice", "github.com/acme/mono", "a1"), stall("bob", "github.com/acme/mono", "b1"))
	s.until(stormStart.Add(time.Minute))
	if len(ep.requests) != 1 || s.n.queued() != 0 {
		t.Errorf("%d posts, %d still queued", len(ep.requests), s.n.queued())
	}
	if got := s.logs.lines("webhook refused"); len(got) != 1 || !strings.Contains(got[0], "dropped=2") {
		t.Errorf("logged %q", got)
	}
}

// An endpoint that never answers holds the sender for the client's timeout on
// every post, and an activity is dropped after six. A storm of 100,000 stalls
// keeps at most maxPending of them queued and only counts the rest, at no
// cost to the board; once the endpoint answers again, one message carries
// what is left and says how many came.
func TestWebhookHungEndpointKeepsMemoryBounded(t *testing.T) {
	ep := &fakeEndpoint{hang: true}
	s := newStorm(t, ep, WebhookConfig{})
	for sweep := range 100 {
		acts := make([]board.Activity, 1000)
		for i := range acts {
			acts[i] = stall(fmt.Sprintf("m%03d", i%200), fmt.Sprintf("github.com/acme/r%02d", i%30), fmt.Sprintf("s%d-%d", sweep, i))
		}
		s.at(stormStart.Add(time.Duration(sweep)*15*time.Second), acts...)
		s.n.mu.Lock()
		pending, capacity, kinds, held := len(s.n.pending), cap(s.n.pending), len(s.n.counted.kinds), len(s.n.alerts)
		s.n.mu.Unlock()
		if pending > maxPending || capacity > 2*maxPending || kinds > 1 || held > maxPending {
			t.Fatalf("after sweep %d: %d queued (capacity %d), %d kinds counted, %d sessions held", sweep, pending, capacity, kinds, held)
		}
	}
	// Past the bound, handing over an activity costs the board a count.
	wanted := 100000
	more := []board.Activity{stall("m", "github.com/acme/r00", "late")}
	if allocs := testing.AllocsPerRun(1000, func() { s.n.enqueue(more); wanted++ }); allocs != 0 {
		t.Errorf("enqueue into a full queue allocates %v times", allocs)
	}
	// 25 minutes: the first posts back off from one second, then there is
	// one every 32 s plus the 5 s the endpoint hangs for.
	if posts := len(ep.requests); posts > 50 {
		t.Errorf("%d posts to a hung endpoint", posts)
	}
	dropped := 0
	for _, l := range s.logs.lines("webhook gave up") {
		var n int
		_, _ = fmt.Sscanf(l[strings.Index(l, "dropped=")+len("dropped="):], "%d", &n)
		dropped += n
	}
	ep.hang = false
	s.until(stormStart.Add(time.Hour))
	s.coverage(wanted - dropped)
	if len(ep.delivered) == 0 || !strings.Contains(ep.delivered[0].text, "came while the webhook was behind") {
		t.Errorf("the message after the outage does not say how many came: %+v", ep.delivered)
	}
	t.Logf("%d posts, %d dropped after %d attempts, then %d messages for the other %d", len(ep.requests)-len(ep.delivered), dropped,
		maxAttempts, len(ep.delivered), wanted-dropped)
}

// Every evening people stop prompting their agents and leave them open; two
// hours later each session turns gone. At the target that was 1,000 messages
// a night, none of them about a stuck agent. Only the ten agents that died
// mid-turn are worth telling anyone about.
func TestWebhookEveningWaveIsQuiet(t *testing.T) {
	s := newStorm(t, &fakeEndpoint{}, WebhookConfig{})
	evening := time.Date(2026, 10, 2, 17, 0, 0, 0, time.UTC)
	s.clock.t = evening
	idle := 0
	b := board.New(board.DefaultConfig(), board.WithNotify(func(acts []board.Activity) {
		for _, a := range acts {
			if a.Idle {
				idle++
			}
		}
		s.n.enqueue(acts)
	}))
	type event struct {
		at   time.Time
		kind board.Kind
		i    int
	}
	var events []event
	died := map[string]bool{}
	for i := range 1010 {
		at := evening.Add(time.Duration(i) * 2 * time.Hour / 1010)
		events = append(events, event{at, board.KindPrompt, i})
		if i%101 == 100 {
			died[fmt.Sprint("s", i)] = true
		} else {
			events = append(events, event{at.Add(time.Minute), board.KindStop, i})
		}
	}
	slices.SortStableFunc(events, func(x, y event) int { return x.at.Compare(y.at) })
	end := evening.Add(5 * time.Hour)
	// Sweeps come every 15 s in production; how often makes no difference to
	// what they announce, only to how long this takes.
	for at := evening; at.Before(end); at = at.Add(5 * time.Minute) {
		for len(events) > 0 && !events[0].at.After(at) {
			e := events[0]
			events = events[1:]
			member := fmt.Sprintf("m%03d", e.i%200)
			w := board.Where{Repo: fmt.Sprintf("github.com/acme/r%02d", e.i%30), Host: member + "-laptop", Worktree: fmt.Sprintf("/work/%d", e.i), Branch: "main"}
			if _, err := b.Hook(e.at, board.HookEvent{Kind: e.kind, Member: member, Agent: board.AgentClaudeCode, SessionID: fmt.Sprint("s", e.i), Where: w}); err != nil {
				t.Fatal(err)
			}
		}
		s.until(at)
		b.Sweep(at)
	}
	s.until(end.Add(time.Minute))
	if idle != 1000 {
		t.Fatalf("%d sessions left open turned gone, want 1000", idle)
	}
	s.coverage(20) // the ten that died: stalled, then gone
	for _, d := range s.ep.delivered {
		for _, a := range d.acts {
			if !died[a.Session] {
				t.Errorf("sent %+v", a)
			}
		}
	}
}

func recovery(member, repo, session string) board.Activity {
	return board.Activity{Kind: board.ActivitySessionRecovered, Repo: repo, Member: member, Session: session, Agent: board.AgentClaudeCode}
}

func texts(ds []delivery) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.text)
	}
	return strings.Join(out, "\n")
}

// A "looks stuck" still waiting when its agent reports again is dropped:
// telling a person about an agent that is already back is noise.
func TestWebhookDropsAStallWhoseAgentIsBack(t *testing.T) {
	s := newStorm(t, &fakeEndpoint{}, WebhookConfig{})
	s.at(stormStart, refusal("bob", repo, "b1", "svc/x.go")) // goes at once; the next message waits a second
	s.at(stormStart.Add(100*time.Millisecond), stall("alice", repo, "a1"), stall("carol", repo, "c1"))
	s.at(stormStart.Add(500*time.Millisecond), recovery("alice", repo, "a1"))
	s.until(stormStart.Add(time.Minute))
	want := "intagent: bob's claude-code agent was refused an edit in github.com/acme/mono: svc/x.go → alice (declared exclusive intent svc/**: \"retry\").\n" +
		"intagent: carol's claude-code agent in github.com/acme/mono looks stuck: silent for 10m."
	if got := texts(s.ep.delivered); got != want {
		t.Errorf("sent\n%s\nwant\n%s", got, want)
	}
	s.coverage(2)
}

// A blip larger than the queue while the endpoint fails: the 5,000 stalls
// listed are withdrawn as their agents report again, but the 300 only counted
// cannot be, so the one message left says they may be back.
func TestWebhookOverflowedStallsMayBeBack(t *testing.T) {
	ep := &fakeEndpoint{fail: 3}
	s := newStorm(t, ep, WebhookConfig{})
	var stalls, back []board.Activity
	for i := range maxPending + 300 {
		stalls = append(stalls, stall(fmt.Sprintf("m%04d", i), repo, "s"))
		back = append(back, recovery(fmt.Sprintf("m%04d", i), repo, "s"))
	}
	s.at(stormStart, stalls...)
	s.at(stormStart.Add(2*time.Second), back...)
	s.until(stormStart.Add(time.Minute))
	want := "intagent: 300 more came while the webhook was behind, too many to list: 300 stuck agents. " +
		"Some of these agents may have reported again since; the dashboard shows which."
	if len(ep.delivered) != 1 || ep.delivered[0].text != want || ep.delivered[0].key != "5001-5300-300" {
		t.Errorf("delivered %+v\nwant one message keyed 5001-5300-300: %s", ep.delivered, want)
	}
	s.coverage(300)
}

// With session.recovered subscribed, a person who was told an agent looked
// stuck is told when it is back, and only then: not for a stall that was never
// sent, nor for an agent nobody announced (as after a restart).
func TestWebhookPairsRecoveriesWithAnnouncedStalls(t *testing.T) {
	ep := &fakeEndpoint{}
	s := newStorm(t, ep, WebhookConfig{Events: []board.ActivityKind{board.ActivitySessionStalled, board.ActivitySessionGone,
		board.ActivityConflict, board.ActivitySessionRecovered}})
	s.at(stormStart, stall("alice", repo, "a1"))
	s.at(stormStart.Add(100*time.Millisecond), stall("carol", repo, "c1"))
	s.at(stormStart.Add(200*time.Millisecond), recovery("carol", repo, "c1"))
	s.at(stormStart.Add(300*time.Millisecond), recovery("dave", repo, "d1"))
	s.at(stormStart.Add(5*time.Second), recovery("alice", repo, "a1"))
	// erin is back while her stall is being posted: the recovery follows it.
	s.until(stormStart.Add(9 * time.Second))
	ep.during = func() {
		a := recovery("erin", repo, "e1")
		s.seq++
		a.Seq, a.At = s.seq, s.clock.t
		s.n.enqueue([]board.Activity{a})
	}
	s.at(stormStart.Add(10*time.Second), stall("erin", repo, "e1"))
	s.until(stormStart.Add(time.Minute))
	want := strings.Join([]string{ // each one alone, so each the line it always was
		"intagent: alice's claude-code agent in github.com/acme/mono looks stuck: silent for 10m.",
		"intagent: alice's claude-code agent in github.com/acme/mono: session.recovered ",
		"intagent: erin's claude-code agent in github.com/acme/mono looks stuck: silent for 10m.",
		"intagent: erin's claude-code agent in github.com/acme/mono: session.recovered ",
	}, "\n")
	if got := texts(s.ep.delivered); got != want {
		t.Errorf("sent\n%s\nwant\n%s", got, want)
	}
	s.coverage(4)

	// After a blip, the agents announced as stuck come back together; so do
	// others, never announced, whose recoveries say nothing to anyone.
	ep = &fakeEndpoint{}
	s = newStorm(t, ep, WebhookConfig{Events: []board.ActivityKind{board.ActivitySessionStalled, board.ActivitySessionRecovered}})
	var stalls []board.Activity
	for i := range 300 {
		stalls = append(stalls, stall(fmt.Sprintf("m%03d", i), repo, "s"))
	}
	s.at(stormStart, stalls...)
	for i := range 500 {
		s.at(stormStart.Add(time.Minute+time.Duration(i)*20*time.Millisecond), recovery(fmt.Sprintf("m%03d", i), repo, "s"))
	}
	s.until(stormStart.Add(5 * time.Minute))
	s.coverage(600)
	if n := len(ep.delivered); n > 12 || !strings.Contains(texts(ep.delivered), "that were announced as quiet are reporting again") {
		t.Errorf("%d messages:\n%s", n, texts(ep.delivered))
	}
}

// The sessions remembered as announced are bounded; the oldest are forgotten
// first, and their recoveries go unsaid.
func TestWebhookForgetsTheOldestAnnouncedSessions(t *testing.T) {
	ep := &fakeEndpoint{}
	s := newStorm(t, ep, WebhookConfig{Events: []board.ActivityKind{board.ActivitySessionStalled, board.ActivitySessionRecovered}})
	for round := range 3 {
		var stalls []board.Activity
		for i := range 3000 {
			stalls = append(stalls, stall("m", repo, fmt.Sprint(round*3000+i)))
		}
		s.at(stormStart.Add(time.Duration(round)*time.Minute), stalls...)
	}
	s.until(stormStart.Add(5 * time.Minute))
	if len(s.n.told.added) != maxTold || len(s.n.told.order) > 2*maxTold {
		t.Fatalf("told holds %d sessions in %d entries", len(s.n.told.added), len(s.n.told.order))
	}
	s.at(stormStart.Add(10*time.Minute), recovery("m", repo, "0"), recovery("m", repo, "8999"))
	s.until(stormStart.Add(15 * time.Minute))
	last := ep.delivered[len(ep.delivered)-1]
	if last.count != 1 || last.acts[0].Session != "8999" {
		t.Errorf("last message %+v, want only the newest session's recovery", last)
	}
}
