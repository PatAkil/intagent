package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

var t0 = time.Unix(1_790_000_000, 0)

func TestPerSecondCountsTheLastMinute(t *testing.T) {
	var c perSecond
	for i := range 5 {
		c.add(t0.Add(time.Duration(i) * time.Second))
		c.add(t0.Add(time.Duration(i)*time.Second + 999*time.Millisecond))
	}
	at := t0.Add(4 * time.Second)
	if got := c.sum(at, statusWindow); got != 10 {
		t.Fatalf("last minute: %d, want 10", got)
	}
	if got := c.sum(at, 2); got != 4 {
		t.Fatalf("last 2 s: %d, want 4", got)
	}
	// A minute on, the same slots count the new seconds only.
	later := at.Add(time.Minute)
	c.add(later)
	if got := c.sum(later, statusWindow); got != 1 {
		t.Fatalf("a minute later: %d, want 1", got)
	}
	if n := testing.AllocsPerRun(100, func() { c.add(later); c.sum(later, statusWindow) }); n != 0 {
		t.Fatalf("%v allocations per count", n)
	}
}

func TestPerSecondIsSafeWithoutALock(t *testing.T) {
	var c perSecond
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				c.add(t0.Add(time.Duration((g+i)%3) * time.Second))
			}
		}()
	}
	wg.Wait()
	if got := c.sum(t0.Add(2*time.Second), 3); got != 8000 {
		t.Fatalf("counted %d of 8000", got)
	}
}

// The server is degraded after a run of unchecked edits and recovers once
// they have stopped for a while, whatever the traffic.
func TestDegradedComesAndGoesWithHysteresis(t *testing.T) {
	for _, tc := range []struct {
		name string
		// each second's pre_edits and unchecked ones, from t0
		load       func(sec int) (n, unchecked int)
		changes    []int // seconds at which the state flips
		finalState bool
	}{
		{"healthy", func(int) (int, int) { return 50, 0 }, nil, false},
		{"one slow answer in quiet traffic", func(sec int) (int, int) {
			if sec == 5 {
				return 2, 1
			}
			return 1, 0
		}, nil, false},
		{"a few slow answers in heavy traffic", func(sec int) (int, int) { return 100, sec % 2 }, nil, false},
		{"five seconds without answers", func(sec int) (int, int) {
			if sec >= 10 && sec < 15 {
				return 10, 10
			}
			return 10, 0
		}, []int{10, 44}, false},
		{"answers stop for good", func(sec int) (int, int) {
			if sec >= 10 {
				return 10, 10
			}
			return 10, 0
		}, []int{10}, true},
		{"traffic stops while degraded", func(sec int) (int, int) {
			if sec >= 10 && sec < 15 {
				return 10, 10
			}
			return 0, 0
		}, []int{10, 44}, false},
		// Agents waiting out their timeouts send fewer edits: the last 30
		// seconds' share counts the traffic from before the trouble.
		{"traffic falls as answers stop", func(sec int) (int, int) {
			switch {
			case sec < 20:
				return 20, 0
			case sec >= 27 && sec < 30:
				return 1, 1
			}
			return 1, 0
		}, []int{29, 59}, false},
		// At 69 the burst at 39 has left the last 30 seconds, where the last
		// three unchecked edits are few, but they are most of the last 10's.
		{"a long stall ends as traffic falls", func(sec int) (int, int) {
			n := 100
			if sec >= 60 {
				n = 1
			}
			switch sec {
			case 10:
				return n, 100
			case 39:
				return n, 50
			case 66, 67, 68:
				return n, 1
			}
			return n, 0
		}, []int{10, 78}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAnswers()
			_, changed := a.watch(t0)
			var flips []int
			for sec := range 90 {
				at := t0.Add(time.Duration(sec) * time.Second)
				n, unchecked := tc.load(sec)
				for range n {
					a.preEdits.add(at)
				}
				for range unchecked {
					a.unchecked.add(at)
				}
				if _, _, ok := a.tick(at); ok {
					flips = append(flips, sec)
				}
			}
			select {
			case <-changed:
				if len(flips) == 0 {
					t.Error("watchers were told of a change that did not happen")
				}
			default:
				if len(flips) > 0 {
					t.Error("watchers were not told of the change")
				}
			}
			if st := a.status(t0.Add(89 * time.Second)); !equalInts(flips, tc.changes) || st.Degraded != tc.finalState {
				t.Fatalf("state flipped at %v, ending degraded %v; want %v and %v", flips, st.Degraded, tc.changes, tc.finalState)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// degrade makes the server see a minute of unchecked edits up to at.
func degrade(ts *testServer, at time.Time) {
	for range 5 {
		ts.answers.preEdits.add(at)
		ts.answers.unchecked.add(at)
	}
	ts.checkLoad(at)
}

func TestHealthReportsUncheckedEdits(t *testing.T) {
	ts := newTestServer(t)
	health := func(query string) (int, map[string]any) {
		t.Helper()
		var h map[string]any
		code := ts.do(t, http.MethodGet, "/healthz"+query, "", nil, &h)
		return code, h
	}
	for _, q := range []string{"", "?strict=1"} {
		if code, h := health(q); code != http.StatusOK || h["ok"] != true || h["degraded"] != false || h["unchecked_60s"] != 0.0 || h["version"] != "dev" {
			t.Fatalf("healthy %q: %d %v", q, code, h)
		}
	}
	degrade(ts, ts.requestClock())
	if code, h := health(""); code != http.StatusOK || h["ok"] != true || h["degraded"] != true || h["unchecked_60s"] != 5.0 || h["pre_edits_60s"] != 5.0 {
		t.Fatalf("degraded: %d %v", code, h)
	}
	if code, h := health("?strict=1"); code != http.StatusServiceUnavailable || h["ok"] != false || h["degraded"] != true {
		t.Fatalf("degraded, strict: %d %v", code, h)
	}
}

// A dashboard learns that edits go ahead unchecked from its stream, the
// moment it happens or when it connects, and from the board.
func TestDashboardHearsTheServerIsDegraded(t *testing.T) {
	ts := newTestServer(t)
	open := func() (*bufio.Reader, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.url+"/v1/stream?repo="+repo, nil)
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, ": connected") {
				break
			}
		}
		return r, func() { cancel(); _ = resp.Body.Close() }
	}
	status := func(r *bufio.Reader) LoadStatus {
		t.Helper()
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v after %q", err, lines)
			}
			lines = append(lines, line)
			if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
				if len(lines) < 2 || lines[len(lines)-2] != "event: status\n" {
					t.Fatalf("not a status event: %q", lines)
				}
				var st LoadStatus
				if err := json.Unmarshal([]byte(data), &st); err != nil {
					t.Fatal(err)
				}
				return st
			}
		}
	}
	r, closeStream := open()
	defer closeStream()
	at := ts.requestClock()
	degrade(ts, at)
	if st := status(r); !st.Degraded || st.Unchecked60s != 5 || !st.Since.Equal(at) {
		t.Fatalf("status on the stream: %+v", st)
	}
	late, closeLate := open()
	defer closeLate()
	if st := status(late); !st.Degraded {
		t.Fatalf("a stream opened while degraded: %+v", st)
	}
	var v struct {
		Repo   string     `json:"repo"`
		Server LoadStatus `json:"server"`
	}
	if code := ts.do(t, http.MethodGet, "/v1/board?repo="+repo, "bob", nil, &v); code != http.StatusOK || v.Repo != repo || !v.Server.Degraded {
		t.Fatalf("board: %d %+v", code, v)
	}
	ts.checkLoad(at.Add(time.Minute))
	if st := status(r); st.Degraded {
		t.Fatalf("recovery on the stream: %+v", st)
	}
}

// The server counts a pre_edit as unchecked when its client gives up waiting,
// as it happens, and not when the client got its answer.
func TestGivingUpIsCounted(t *testing.T) {
	var gate sync.Mutex
	ts := newTestServer(t, func(o *Options) {
		o.Now = func() time.Time { gate.Lock(); defer gate.Unlock(); return time.Now() }
	})
	ts.wall.Store(true)
	send := func(timeout time.Duration) error {
		body, _ := json.Marshal(hookEv(board.KindPreEdit, "alice", "a1", "a.go"))
		req, _ := http.NewRequest(http.MethodPost, ts.url+"/v1/hook", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ts.tokens["alice"])
		req.Header.Set(timeoutHeader, "300")
		resp, err := (&http.Client{Timeout: timeout}).Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return err
	}
	if err := send(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	gate.Lock()
	if err := send(300 * time.Millisecond); err == nil {
		t.Fatal("the client got an answer from a held server")
	}
	deadline := time.Now().Add(5 * time.Second)
	for ts.answers.status(time.Now()).Unchecked60s == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	waiting := ts.answers.status(time.Now())
	gate.Unlock()
	if waiting.Unchecked60s != 1 {
		t.Fatalf("while the request still waited for the board: %+v, want the give-up counted", waiting)
	}
	// The held request now reaches the board, late: it is not counted twice.
	deadline = time.Now().Add(5 * time.Second)
	for ts.Board().View(time.Now(), repo).Stats.Unheard == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := ts.answers.status(time.Now()); st.PreEdits60s != 2 || st.Unchecked60s != 1 {
		t.Fatalf("counted %+v, want 2 pre_edits and 1 unchecked", st)
	}
}

func TestWebhookAnnouncesTheServerOnlyWhenAsked(t *testing.T) {
	if _, err := parseFileConfig("team.json", []byte(`{"members":[],"webhook":{"url":"https://hooks.example.com/x",`+
		`"events":["session.stalled","server.degraded","server.recovered"]}}`)); err != nil {
		t.Fatalf("server events in team.json: %v", err)
	}
	degraded := board.Activity{Kind: board.ActivityServerDegraded, Text: "40 of 60 edits in the last minute went ahead unchecked"}
	recovered := board.Activity{Kind: board.ActivityServerRecovered, Text: "after 2m30s"}
	if n := newNotifier(WebhookConfig{URL: "x"}, nil); n.wants(degraded) || n.wants(recovered) {
		t.Fatal("server events sent by default")
	}
	n := newNotifier(WebhookConfig{URL: "x", Events: []board.ActivityKind{board.ActivityServerDegraded, board.ActivityServerRecovered}}, nil)
	if !n.wants(degraded) || !n.wants(recovered) {
		t.Fatal("server events asked for but not sent")
	}
	for a, want := range map[*board.Activity]string{
		&degraded:  "intagent: the team server is not answering agents in time: 40 of 60 edits in the last minute went ahead unchecked. They go ahead without a check until it does.",
		&recovered: "intagent: the team server answers agents in time again, after 2m30s.",
	} {
		if got := describeActivity(*a); got != want {
			t.Errorf("describeActivity = %q\nwant %q", got, want)
		}
	}
}
