package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// WebhookConfig sends selected activities to an HTTP endpoint, such as a Slack
// incoming webhook, so people hear about stuck agents and refused collisions
// without watching the dashboard.
type WebhookConfig struct {
	URL string `json:"url"`
	// Events lists activity kinds to send. Empty means DefaultWebhookEvents.
	// "conflict" sends only refused or asked edits, never warnings.
	Events []board.ActivityKind `json:"events,omitempty"`
	// Idle also sends session.gone for agents that had finished their turn and
	// were waiting for their person: sessions left open, not stuck agents.
	Idle bool `json:"idle,omitempty"`
}

// DefaultWebhookEvents are the activities worth interrupting a person for.
var DefaultWebhookEvents = []board.ActivityKind{board.ActivitySessionStalled, board.ActivitySessionGone, board.ActivityConflict}

// webhookPayload is Slack-compatible ("text") and carries the activity for
// other consumers. A message about one activity is exactly this.
type webhookPayload struct {
	Text     string         `json:"text"`
	Activity board.Activity `json:"activity"`
}

// batchPayload is a message about several activities. Activity is the first
// one the text names, so a consumer that reads only it still gets the most
// urgent; Count is how many the message covers, Activities lists the first of
// them and More counts the rest.
type batchPayload struct {
	Text       string           `json:"text"`
	Activity   *board.Activity  `json:"activity,omitempty"`
	Count      int              `json:"count"`
	Activities []board.Activity `json:"activities"`
	More       int              `json:"more"`
}

const (
	// maxPending bounds the activities waiting to be sent. Past it they are
	// only counted, by kind, and the next message says how many.
	maxPending = 5000
	// webhookPace is the least time between the starts of two messages.
	// Webhooks reach people, and Slack takes about one message a second.
	webhookPace = time.Second
	// webhookTimeout bounds one post, the answer's body included.
	webhookTimeout = 5 * time.Second
	// maxGroupLines is how many activities of one kind in one repository a
	// message names line by line; a larger group is summed up in one line.
	maxGroupLines = 5
	// maxMessageLines and maxMessageText bound a message's text. What does
	// not fit is counted in its last lines, for which tailRoom is kept.
	maxMessageLines = 40
	maxMessageText  = 12000
	tailRoom        = 1000
	// maxPayloadActivities bounds the activities a message carries as JSON.
	maxPayloadActivities = 50
	// maxAttempts is how many posts an activity gets when the endpoint fails
	// or cannot be reached; the waits between them double up to maxBackoff.
	maxAttempts = 6
	maxBackoff  = 32 * time.Second
	// maxRetryAfter bounds how long a 429's Retry-After holds the sender.
	maxRetryAfter = 60 * time.Second
	// maxDrain is how much of an answer is read so its connection is reused.
	maxDrain = 64 << 10
	// maxTold bounds the sessions remembered as announced quiet, for pairing
	// their recoveries; the oldest are forgotten first.
	maxTold = 4096
	// finalFlushTime bounds the last message, sent as the server stops.
	finalFlushTime = 2 * time.Second
)

// notifier posts the activities a webhook wants. The board hands them over
// under its own lock, so enqueue only appends to a bounded queue; one sender
// takes everything queued at once and posts it as a single message, no more
// than one per pace, so a storm of activities costs a few messages.
type notifier struct {
	cfg       WebhookConfig
	recovered bool // the webhook wants session.recovered
	client    *http.Client
	log       *slog.Logger
	// now, after and pace time the sender, which tests drive themselves.
	// They are transport time: the board's own clock plays no part.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	pace  time.Duration
	wake  chan struct{} // one slot: something was queued

	mu      sync.Mutex
	pending []*pendingItem
	// counted holds what came while the queue was full.
	counted counts
	// alerts holds each session's stalled and gone items, queued or being
	// sent, so its recovery can cancel them or follow them.
	alerts map[string][]*pendingItem
	// told remembers the sessions whose stall or gone was delivered, when the
	// webhook wants session.recovered: only those recoveries are sent.
	told toldSet

	// Only the sender touches these, nextSend included.
	notBefore    time.Time // no message starts before this
	failures     int       // posts in a row that failed and are worth retrying
	countedTries int       // posts in a row that failed while carrying counted
}

// pendingItem is an activity waiting to be sent, and how often it was posted.
type pendingItem struct {
	act      board.Activity
	attempts int
	// recovery is the session.recovered that came after this stall or gone
	// was queued: unsent, the alert is dropped; sent, the recovery follows.
	recovery *board.Activity
}

func newNotifier(cfg WebhookConfig, log *slog.Logger) *notifier {
	if len(cfg.Events) == 0 {
		cfg.Events = DefaultWebhookEvents
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &notifier{cfg: cfg, client: &http.Client{Timeout: webhookTimeout}, log: log,
		now: time.Now, after: time.After, pace: webhookPace, wake: make(chan struct{}, 1), alerts: map[string][]*pendingItem{},
		recovered: slices.Contains(cfg.Events, board.ActivitySessionRecovered)}
}

func (n *notifier) wants(a board.Activity) bool {
	if !slices.Contains(n.cfg.Events, a.Kind) {
		return false
	}
	if a.Kind == board.ActivitySessionGone && a.Idle {
		return n.cfg.Idle
	}
	if a.Kind == board.ActivityConflict {
		// Refusals and questions, and changes made inside a teammate's
		// reservation without a check; never warnings.
		return a.Decision == board.DecisionRefuse || a.Decision == board.DecisionAsk || a.Breach
	}
	return true
}

// enqueue queues what the webhook wants. It never blocks the board: past
// maxPending it only counts.
func (n *notifier) enqueue(acts []board.Activity) {
	queued := false
	n.mu.Lock()
	for _, a := range acts {
		if a.Kind == board.ActivitySessionRecovered {
			queued = n.recover(a) || queued
			continue
		}
		if !n.wants(a) {
			continue
		}
		queued = true
		if len(n.pending) < maxPending {
			it := &pendingItem{act: a}
			n.pending = append(n.pending, it)
			if quiet(a.Kind) {
				k := sessionOf(a)
				n.alerts[k] = append(n.alerts[k], it)
			}
			continue
		}
		if n.counted.empty() {
			n.log.Warn("webhook queue full; counting activities without listing them", "queued", len(n.pending))
		}
		n.counted.add(a)
	}
	n.mu.Unlock()
	if queued {
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
}

func quiet(k board.ActivityKind) bool {
	return k == board.ActivitySessionStalled || k == board.ActivitySessionGone
}

func sessionOf(a board.Activity) string {
	return a.Member + "\x00" + string(a.Agent) + "\x00" + a.Session
}

// recover pairs a session.recovered with what was said about the session:
// an alert still queued or being sent takes it along, and otherwise it is
// queued only if the webhook wants it and was told the session went quiet.
// It reports whether it queued anything. Called with n.mu held.
func (n *notifier) recover(a board.Activity) bool {
	k := sessionOf(a)
	if its := n.alerts[k]; len(its) > 0 {
		for _, it := range its {
			it.recovery = &a
		}
		return false
	}
	if !n.recovered || !n.told.remove(k) {
		return false
	}
	if len(n.pending) < maxPending {
		n.pending = append(n.pending, &pendingItem{act: a})
	} else {
		n.counted.add(a)
	}
	return true
}

// settle ends what the notifier holds about alerts that were delivered, or
// dropped, and queues the recoveries that came for them while they waited
// and that a person now needs to hear. Called with n.mu held.
func (n *notifier) settle(items []*pendingItem, delivered bool) {
	var back []*pendingItem
	for _, it := range items {
		if !quiet(it.act.Kind) {
			continue
		}
		k := sessionOf(it.act)
		if n.alerts[k] = slices.DeleteFunc(n.alerts[k], func(x *pendingItem) bool { return x == it }); len(n.alerts[k]) == 0 {
			delete(n.alerts, k)
		}
		if delivered && n.recovered {
			n.told.add(k)
		}
		if it.recovery != nil {
			back = append(back, it)
		}
	}
	for _, it := range back {
		// Another alert of the session still waiting carries the same
		// recovery, and settles it in turn.
		if k := sessionOf(it.act); len(n.alerts[k]) == 0 {
			n.recover(*it.recovery)
		}
	}
}

// run sends what is queued until ctx ends, then sends what is left once. A
// post under way when ctx ends is not cut short; the client's timeout bounds
// it.
func (n *notifier) run(ctx context.Context) {
	send := context.WithoutCancel(ctx)
	for n.waitForTurn(ctx) {
		n.flush(send)
	}
	n.finalFlush()
}

// finalFlush posts what is still queued in one last message, within
// finalFlushTime and whatever the pace or a backoff would say. What it cannot
// deliver is lost with the process, and logged.
func (n *notifier) finalFlush() {
	items, counted := n.take()
	if len(items) == 0 && counted.empty() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), finalFlushTime)
	defer cancel()
	m := compose(items, counted)
	if ans := n.post(ctx, m); ans.err != nil {
		n.log.Warn("webhook failed as the server stopped; dropping", "dropped", m.count, "err", ans.err)
	}
}

// waitForTurn waits until something is queued and the pace allows a message.
// It returns false once ctx has ended.
func (n *notifier) waitForTurn(ctx context.Context) bool {
	for ctx.Err() == nil {
		at, ok := n.nextSend()
		if !ok {
			select {
			case <-ctx.Done():
			case <-n.wake:
			}
			continue
		}
		wait := at.Sub(n.now())
		if wait <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
		case <-n.after(wait):
			return true
		}
	}
	return false
}

// nextSend says when the next message may start, and whether anything waits.
func (n *notifier) nextSend() (time.Time, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notBefore, len(n.pending) > 0 || !n.counted.empty()
}

// take empties the queue into one message's worth, leaving out stalls and
// gones whose agent has been heard from since: "looks stuck" about an agent
// that is back is noise.
func (n *notifier) take() ([]*pendingItem, counts) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var back []*pendingItem
	items := slices.DeleteFunc(n.pending, func(it *pendingItem) bool {
		if it.recovery != nil {
			back = append(back, it)
			return true
		}
		return false
	})
	n.pending = nil
	n.settle(back, false)
	items = append(items, n.pending...)
	counted := n.counted
	n.pending, n.counted = nil, counts{}
	return items, counted
}

// giveBack queues a failed message's activities again, ahead of anything that
// came since. An activity posted maxAttempts times is dropped instead, and so
// are the counts once maxAttempts posts carrying them failed. It returns how
// many activities it dropped.
func (n *notifier) giveBack(items []*pendingItem, counted counts, attempted bool) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	var gone []*pendingItem
	dropped := 0
	if attempted {
		items = slices.DeleteFunc(items, func(it *pendingItem) bool {
			if it.attempts++; it.attempts >= maxAttempts {
				gone = append(gone, it)
				return true
			}
			return false
		})
		dropped = len(gone)
		if !counted.empty() {
			if n.countedTries++; n.countedTries >= maxAttempts {
				dropped += counted.total()
				counted, n.countedTries = counts{}, 0
			}
		}
	}
	items = append(items, n.pending...)
	over := items[min(len(items), maxPending):]
	for _, it := range over {
		counted.add(it.act)
	}
	n.pending = items[:min(len(items), maxPending)]
	counted.merge(n.counted)
	n.counted = counted
	n.settle(append(gone, over...), false)
	return dropped
}

// counts is what the queue could not hold: how many of each kind, never
// listed, and the lowest and highest of their seqs, by which the message
// that carries them is keyed.
type counts struct {
	kinds  map[board.ActivityKind]int
	lo, hi uint64
}

func (c *counts) add(a board.Activity) {
	if c.kinds == nil {
		c.kinds = map[board.ActivityKind]int{}
	}
	c.kinds[a.Kind]++
	c.cover(a.Seq, a.Seq)
}

func (c *counts) merge(o counts) {
	if o.empty() {
		return
	}
	if c.kinds == nil {
		c.kinds = map[board.ActivityKind]int{}
	}
	for k, v := range o.kinds {
		c.kinds[k] += v
	}
	c.cover(o.lo, o.hi)
}

// cover widens the range of seqs to take in lo to hi. Seqs start at 1, so a
// lo of 0 means no range yet.
func (c *counts) cover(lo, hi uint64) {
	if c.lo == 0 || lo < c.lo {
		c.lo = lo
	}
	c.hi = max(c.hi, hi)
}

func (c counts) empty() bool { return len(c.kinds) == 0 }

func (c counts) total() int {
	t := 0
	for _, v := range c.kinds {
		t += v
	}
	return t
}

// flush posts everything queued as one message, and handles the answer: a
// 429 waits as long as it asks, a server error or a failed connection backs
// off, and any other refusal drops the message.
func (n *notifier) flush(ctx context.Context) {
	items, counted := n.take()
	if len(items) == 0 && counted.empty() {
		return
	}
	n.notBefore = n.now().Add(n.pace)
	m := compose(items, counted)
	ans := n.post(ctx, m)
	switch {
	case ans.err == nil:
		n.failures, n.countedTries = 0, 0
		n.settled(items, true)
	case ans.status == http.StatusTooManyRequests:
		wait := time.Second
		if ans.hinted {
			wait = ans.retryAfter
		}
		n.notBefore = later(n.notBefore, n.now().Add(wait))
		n.giveBack(items, counted, false)
		n.log.Info("webhook rate-limited; waiting", "activities", m.count, "retry_in", wait)
	case ans.retry:
		n.failures++
		wait := backoff(n.failures)
		if ans.hinted {
			wait = max(wait, ans.retryAfter)
		}
		n.notBefore = later(n.notBefore, n.now().Add(wait))
		dropped := n.giveBack(items, counted, true)
		n.log.Warn("webhook failed", "activities", m.count, "err", ans.err, "retry_in", wait)
		if dropped > 0 {
			n.log.Warn("webhook gave up", "dropped", dropped, "attempts", maxAttempts)
		}
	default:
		n.failures, n.countedTries = 0, 0
		n.settled(items, false)
		n.log.Warn("webhook refused; dropping", "dropped", m.count, "err", ans.err)
	}
}

func (n *notifier) settled(items []*pendingItem, delivered bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.settle(items, delivered)
}

// toldSet remembers sessions, up to maxTold, forgetting the oldest first.
type toldSet struct {
	added map[string]uint64 // session → its place in order
	order []toldEntry
	next  uint64
}

type toldEntry struct {
	key   string
	place uint64
}

func (t *toldSet) add(k string) {
	if t.added == nil {
		t.added = map[string]uint64{}
	}
	t.next++
	t.added[k] = t.next
	t.order = append(t.order, toldEntry{k, t.next})
	for len(t.added) > maxTold {
		e := t.order[0]
		t.order = t.order[1:]
		if t.added[e.key] == e.place {
			delete(t.added, e.key)
		}
	}
	if len(t.order) > 2*maxTold { // entries of sessions removed or added again since
		t.order = slices.DeleteFunc(slices.Clone(t.order), func(e toldEntry) bool { return t.added[e.key] != e.place })
	}
}

// remove forgets a session and reports whether it was remembered.
func (t *toldSet) remove(k string) bool {
	if _, ok := t.added[k]; !ok {
		return false
	}
	delete(t.added, k)
	return true
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// backoff is the wait after the given number of failed posts in a row:
// 1, 2, 4 ... seconds, at most maxBackoff.
func backoff(failures int) time.Duration {
	if failures > 6 {
		return maxBackoff
	}
	return min(time.Second<<max(failures-1, 0), maxBackoff)
}

// answer is how an endpoint answered a post.
type answer struct {
	status     int  // 0 when nothing came back
	retry      bool // the endpoint failed or could not be reached
	retryAfter time.Duration
	hinted     bool // the answer said how long to wait, in Retry-After
	err        error
}

func (n *notifier) post(ctx context.Context, m message) answer {
	body, err := m.payload()
	if err != nil {
		return answer{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return answer{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	// The key lets the transport post again on a fresh connection when the
	// endpoint closed a reused one just as the post went out, and lets the
	// endpoint drop a message it already has.
	req.Header.Set("Idempotency-Key", m.key)
	resp, err := n.client.Do(req)
	if err != nil {
		return answer{retry: true, err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 300 {
		drain(resp.Body)
		return answer{status: resp.StatusCode}
	}
	why, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	drain(resp.Body)
	wait, hinted := retryAfter(resp.Header.Get("Retry-After"), n.now())
	return answer{
		status:     resp.StatusCode,
		retry:      resp.StatusCode >= 500,
		retryAfter: wait,
		hinted:     hinted,
		err:        fmt.Errorf("endpoint answered %s: %q", resp.Status, why),
	}
}

// drain reads what is left of an answer, up to maxDrain, so the connection
// can carry the next post instead of a new one being opened.
func drain(r io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(r, maxDrain)) }

// retryAfter reads a Retry-After header, in seconds or as a date, and says
// whether it held either. The wait is at most maxRetryAfter.
func retryAfter(h string, now time.Time) (time.Duration, bool) {
	var d time.Duration
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d = time.Duration(min(secs, int(maxRetryAfter/time.Second))) * time.Second
	} else if at, err := http.ParseTime(h); err == nil {
		d = at.Sub(now)
	} else {
		return 0, false
	}
	return min(max(d, 0), maxRetryAfter), true
}

// --- messages --------------------------------------------------------------

// message is one post: its text, the activities it names in the order it
// names them, and how many activities it covers, counted ones included.
type message struct {
	text  string
	acts  []board.Activity
	count int
	key   string
}

func (m message) payload() ([]byte, error) {
	if m.count == 1 && len(m.acts) == 1 {
		return json.Marshal(webhookPayload{Text: m.text, Activity: m.acts[0]})
	}
	p := batchPayload{Text: m.text, Count: m.count, Activities: m.acts[:min(len(m.acts), maxPayloadActivities)]}
	if len(m.acts) > 0 {
		p.Activity = &m.acts[0]
	} else {
		p.Activities = []board.Activity{} // a list, if an empty one, when only counts are left
	}
	p.More = m.count - len(p.Activities)
	return json.Marshal(p)
}

// compose writes one message about everything taken from the queue. One
// activity is told exactly as it always was. More are grouped by kind and
// repository, collisions first, then stalls, then the rest, each in the order
// it happened: a group of up to maxGroupLines is told line by line, a larger
// one is summed up in a line, and a kind spread over more repositories than
// that is summed up in one line for all of them.
func compose(items []*pendingItem, counted counts) message {
	if len(items) == 1 && counted.empty() {
		a := items[0].act
		return message{text: slackText.Replace(describeActivity(a)), acts: []board.Activity{a}, count: 1, key: activityKey(a)}
	}
	var m message
	var lines []string
	size := 0
	unlisted := map[board.ActivityKind]int{}
	tell := func(line string, about ...board.Activity) {
		line = slackText.Replace(line) // measured as sent: escaping can make it five times as long
		if len(lines) >= maxMessageLines-2 || size+len(line) > maxMessageText-tailRoom {
			unlisted[about[0].Kind] += len(about)
			return
		}
		lines, size = append(lines, line), size+len(line)+1
	}
	groups := groupActivities(items)
	repos := map[board.ActivityKind][]board.Activity{}
	spread := map[board.ActivityKind]int{}
	for _, g := range groups {
		repos[g.kind] = append(repos[g.kind], g.acts...)
		spread[g.kind]++
	}
	for _, g := range groups {
		m.acts = append(m.acts, g.acts...)
		switch all := repos[g.kind]; {
		case spread[g.kind] > maxGroupLines:
			if all != nil {
				tell(summarise(g.kind, all), all...)
				repos[g.kind] = nil // told once, where its first group stood
			}
		case len(g.acts) > maxGroupLines:
			tell(summarise(g.kind, g.acts), g.acts...)
		default:
			for _, a := range g.acts {
				tell(batchLine(a), a)
			}
		}
	}
	if n, what := tally(unlisted); n > 0 {
		lines = append(lines, fmt.Sprintf("intagent: and %d more, not listed here: %s.", n, what))
	}
	n, what := tally(counted.kinds)
	if n > 0 {
		line := fmt.Sprintf("intagent: %d more came while the webhook was behind, too many to list: %s.", n, what)
		if counted.kinds[board.ActivitySessionStalled] > 0 || counted.kinds[board.ActivitySessionGone] > 0 {
			// Only a listed alert is withdrawn when its agent reports again.
			line += " Some of these agents may have reported again since; the dashboard shows which."
		}
		lines = append(lines, line)
	}
	m.text = strings.Join(lines, "\n")
	m.count = len(items) + n
	m.key = batchKey(m.acts, counted, m.count)
	return m
}

// activityKey names a message about one activity: its seq or, for the
// server's own news (server.degraded, server.recovered), which the board does
// not number, its kind and time. They come at most once a second.
func activityKey(a board.Activity) string {
	if a.Seq == 0 {
		return fmt.Sprintf("%s-%d", a.Kind, a.At.UnixNano())
	}
	return strconv.FormatUint(a.Seq, 10)
}

// batchKey names a message by the activities it covers, listed or counted:
// the lowest and highest of their seqs, and how many, and the time of the
// latest of the server's own news it lists, which has no seq. A retry of the
// same message carries the same key; one that grew or shrank does not, nor
// does any other message.
func batchKey(acts []board.Activity, counted counts, count int) string {
	span := counts{lo: counted.lo, hi: counted.hi}
	var own time.Time
	ownNews := false
	for _, a := range acts {
		if a.Seq != 0 {
			span.cover(a.Seq, a.Seq)
		} else if !ownNews || a.At.After(own) {
			own, ownNews = a.At, true
		}
	}
	key := fmt.Sprintf("%d-%d-%d", span.lo, span.hi, count)
	if ownNews {
		key += fmt.Sprintf("-%d", own.UnixNano())
	}
	return key
}

// group is the activities of one kind in one repository, in the order they
// happened.
type group struct {
	kind board.ActivityKind
	acts []board.Activity
}

func groupActivities(items []*pendingItem) []*group {
	type key struct {
		kind board.ActivityKind
		repo string
	}
	byKey := map[key]*group{}
	var out []*group
	for _, it := range items {
		k := key{it.act.Kind, it.act.Repo}
		g := byKey[k]
		if g == nil {
			g = &group{kind: k.kind}
			byKey[k] = g
			out = append(out, g)
		}
		g.acts = append(g.acts, it.act)
	}
	// The server's own news has no seq: it goes by its time.
	order := func(x, y board.Activity) int { return cmp.Or(cmp.Compare(x.Seq, y.Seq), x.At.Compare(y.At)) }
	for _, g := range out {
		slices.SortFunc(g.acts, order)
	}
	slices.SortFunc(out, func(x, y *group) int {
		return cmp.Or(cmp.Compare(urgency(x.kind), urgency(y.kind)), order(x.acts[0], y.acts[0]))
	})
	return out
}

// urgency orders a message: what a person may need to act on now comes first.
func urgency(k board.ActivityKind) int {
	switch k {
	case board.ActivityConflict:
		return 0
	case board.ActivitySessionStalled:
		return 1
	case board.ActivitySessionGone:
		return 2
	}
	return 3
}

// correlated ends the summary of many agents gone quiet at once.
const correlated = " This many at once is usually the network or the server, not the agents."

// summarise sums up activities of one kind, too many to list: how many,
// where, when and whose.
func summarise(kind board.ActivityKind, acts []board.Activity) string {
	n, when := len(acts), span(acts)
	// The server's own news is about no repository or member.
	switch kind {
	case board.ActivityServerDegraded:
		return fmt.Sprintf("intagent: the team server fell behind %d times, %s, and agents went ahead without a check "+
			"while it was; the last time, %s.", n, when, latest(acts).Text)
	case board.ActivityServerRecovered:
		return fmt.Sprintf("intagent: the team server caught up %d times, %s, and answers agents in time again.", n, when)
	}
	where := "in " + acts[0].Repo
	who := " Members: " + most(acts, func(a board.Activity) string { return a.Member }) + "."
	if repos := distinct(acts, func(a board.Activity) string { return a.Repo }); repos > 1 {
		where = fmt.Sprintf("in %d repositories", repos)
		who = " Repositories: " + most(acts, func(a board.Activity) string { return a.Repo }) + "." + who
	}
	switch kind {
	case board.ActivitySessionStalled:
		return fmt.Sprintf("intagent: %d agents %s look stuck, %s.%s%s", n, where, when, who, correlated)
	case board.ActivitySessionGone:
		if distinct(acts, func(a board.Activity) string { return strconv.FormatBool(a.Idle) }) == 1 && acts[0].Idle {
			return fmt.Sprintf("intagent: %d agents %s that were left waiting stopped reporting, %s.%s", n, where, when, who)
		}
		return fmt.Sprintf("intagent: %d agents %s stopped reporting without ending their sessions, %s.%s%s", n, where, when, who, correlated)
	case board.ActivitySessionRecovered:
		return fmt.Sprintf("intagent: %d agents %s that were announced as quiet are reporting again, %s.%s", n, where, when, who)
	case board.ActivityConflict:
		line := fmt.Sprintf("intagent: %d collisions %s, %s: %s.%s", n, where, when, outcomes(acts), who)
		if files := most(acts, firstPath); files != "" {
			line += " Files: " + files + "."
		}
		return line
	}
	return fmt.Sprintf("intagent: %d %s %s, %s.%s", n, kind, where, when, who)
}

// latest is the activity that happened last.
func latest(acts []board.Activity) board.Activity {
	last := acts[0]
	for _, a := range acts[1:] {
		if a.At.After(last.At) {
			last = a
		}
	}
	return last
}

func firstPath(a board.Activity) string {
	if len(a.Paths) == 0 {
		return ""
	}
	return a.Paths[0]
}

// distinct counts the different values among activities.
func distinct(acts []board.Activity, value func(board.Activity) string) int {
	seen := map[string]bool{}
	for _, a := range acts {
		seen[value(a)] = true
	}
	return len(seen)
}

// span says when a group's activities happened, to the second, in UTC.
func span(acts []board.Activity) string {
	lo, hi := acts[0].At, acts[0].At
	for _, a := range acts {
		if a.At.Before(lo) {
			lo = a.At
		}
		if a.At.After(hi) {
			hi = a.At
		}
	}
	from, to := lo.UTC().Format(time.TimeOnly), hi.UTC().Format(time.TimeOnly)
	if from == to {
		return "at " + from + " UTC"
	}
	return "between " + from + " and " + to + " UTC"
}

// most names the five most frequent values, with their counts above one,
// and says how many others there are.
func most(acts []board.Activity, value func(board.Activity) string) string {
	counts := map[string]int{}
	for _, a := range acts {
		if v := value(a); v != "" {
			counts[v]++
		}
	}
	vals := make([]string, 0, len(counts))
	for v := range counts {
		vals = append(vals, v)
	}
	slices.SortFunc(vals, func(x, y string) int { return cmp.Or(cmp.Compare(counts[y], counts[x]), strings.Compare(x, y)) })
	parts := make([]string, 0, 5)
	for _, v := range vals[:min(len(vals), 5)] {
		if c := counts[v]; c > 1 {
			v = fmt.Sprintf("%s (%d)", v, c)
		}
		parts = append(parts, v)
	}
	s := strings.Join(parts, ", ")
	if rest := len(vals) - len(parts); rest > 0 {
		s += fmt.Sprintf(" and %d others", rest)
	}
	return s
}

// outcomes counts how a group of collisions ended.
func outcomes(acts []board.Activity) string {
	refused, asked, breached := 0, 0, 0
	for _, a := range acts {
		switch {
		case a.Breach:
			breached++
		case a.Decision == board.DecisionAsk:
			asked++
		default:
			refused++
		}
	}
	var parts []string
	if refused > 0 {
		parts = append(parts, plural(refused, "edit refused", "edits refused"))
	}
	if asked > 0 {
		parts = append(parts, plural(asked, "edit put to a person", "edits put to a person"))
	}
	if breached > 0 {
		parts = append(parts, plural(breached, "reserved file changed without a check", "reserved files changed without a check"))
	}
	return strings.Join(parts, ", ")
}

// tally totals counts by kind and names them, most urgent first.
func tally(counts map[board.ActivityKind]int) (int, string) {
	kinds := make([]board.ActivityKind, 0, len(counts))
	total := 0
	for k, c := range counts {
		if c > 0 {
			kinds = append(kinds, k)
			total += c
		}
	}
	slices.SortFunc(kinds, func(x, y board.ActivityKind) int {
		return cmp.Or(cmp.Compare(urgency(x), urgency(y)), strings.Compare(string(x), string(y)))
	})
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = kindCount(k, counts[k])
	}
	return total, strings.Join(parts, ", ")
}

func kindCount(k board.ActivityKind, c int) string {
	switch k {
	case board.ActivitySessionStalled:
		return plural(c, "stuck agent", "stuck agents")
	case board.ActivitySessionGone:
		return plural(c, "agent that stopped reporting", "agents that stopped reporting")
	case board.ActivityConflict:
		return plural(c, "collision", "collisions")
	case board.ActivitySessionRecovered:
		return plural(c, "agent reporting again", "agents reporting again")
	case board.ActivityServerDegraded:
		return plural(c, "time the server fell behind", "times the server fell behind")
	case board.ActivityServerRecovered:
		return plural(c, "time the server caught up", "times the server caught up")
	}
	return fmt.Sprintf("%d %s", c, k)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// slackText escapes the three characters Slack reads as markup, so a member's
// summary or tool name cannot mention @channel or disguise a link. The line's
// own wording uses none of them.
var slackText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// batchLine is an activity's line in a message about several, where a
// recovery reads as a sentence beside the stall it follows. Told alone, an
// activity keeps the line it always had.
func batchLine(a board.Activity) string {
	if a.Kind == board.ActivitySessionRecovered {
		return fmt.Sprintf("intagent: %s in %s is reporting again.", whose(a), a.Repo)
	}
	return describeActivity(a)
}

// whose names an activity's agent: "alice's codex agent".
func whose(a board.Activity) string {
	if a.Agent == "" {
		return a.Member + "'s agent"
	}
	return fmt.Sprintf("%s's %s agent", a.Member, a.Agent)
}

// describeActivity writes one line a person can act on.
func describeActivity(a board.Activity) string {
	who := whose(a)
	switch a.Kind {
	case board.ActivitySessionStalled:
		return fmt.Sprintf("intagent: %s in %s looks stuck: %s.", who, a.Repo, a.Text)
	case board.ActivitySessionGone:
		return fmt.Sprintf("intagent: %s in %s stopped reporting (%s) without ending its session.", who, a.Repo, a.Text)
	case board.ActivityConflict:
		verb := "was refused an edit"
		switch {
		case a.Breach:
			verb = "changed a reserved file without a check"
		case a.Decision == board.DecisionAsk:
			verb = "was asked to confirm an edit"
		case a.Decision == board.DecisionAllow:
			verb = "was warned before an edit"
		}
		text := a.Text
		if len(a.Also) > 0 {
			text += "; also " + strings.Join(a.Also, "; ")
		}
		return fmt.Sprintf("intagent: %s %s in %s: %s.", who, verb, a.Repo, text)
	case board.ActivityClaimForgotten:
		if a.ClaimID == "" { // claims forgotten to stay under the board's bound
			return fmt.Sprintf("intagent: in %s, %s.", a.Repo, a.Text)
		}
	case board.ActivityServerDegraded:
		return fmt.Sprintf("intagent: the team server is not answering agents in time: %s. They go ahead without a check until it does.", a.Text)
	case board.ActivityServerRecovered:
		return fmt.Sprintf("intagent: the team server answers agents in time again, %s.", a.Text)
	}
	return fmt.Sprintf("intagent: %s in %s: %s %s", who, a.Repo, a.Kind, a.Text)
}
