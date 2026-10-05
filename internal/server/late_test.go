package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

func TestBudgetOf(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"2000", 1700 * time.Millisecond},
		{"8000", 7700 * time.Millisecond},
		{"500", 375 * time.Millisecond},
		{"1", 750 * time.Microsecond},
		{"", 0},
		{"2s", 0},
		{"0", 0},
		{"-2000", 0},
		{"600001", 0},
		{"99999999999999999999", 0},
	} {
		if got := budgetOf(tc.header); got != tc.want {
			t.Errorf("budgetOf(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// A pre_edit that reaches the board after its client stopped waiting is
// answered unchecked and changes nothing: the bump it would have spent stops
// the agent's next edit. One whose client still waits is decided, whatever
// its connection says before lateGuard.
func TestLateHooksAreNotDecided(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string // the X-Intagent-Timeout header
		closed  bool   // the request's context has ended
		waited  time.Duration
		late    bool
	}{
		{"within the client's budget", "2000", false, 1600 * time.Millisecond, false},
		{"past the client's budget", "2000", false, 1800 * time.Millisecond, true},
		{"a short timeout", "300", false, 300 * time.Millisecond, true},
		{"closed early, behind a half-closing proxy", "2000", true, 200 * time.Millisecond, false},
		{"closed after the guard", "8000", true, 1500 * time.Millisecond, true},
		{"an old client, waiting long", "", false, 10 * time.Second, false},
		{"an old client, closed early", "", true, 500 * time.Millisecond, false},
		{"an old client, gone", "", true, 1500 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			bob := hookEv(board.KindPostEdit, "bob", "b1", "svc/pay/retry.go")
			if code := ts.do(t, "POST", "/v1/hook", "bob", bob, nil); code != http.StatusOK {
				t.Fatalf("bob's edit: %d", code)
			}
			ts.wait(tc.waited)
			body, _ := json.Marshal(hookEv(board.KindPreEdit, "alice", "a1", "svc/pay/retry.go"))
			ctx, cancel := context.WithCancel(context.Background())
			if tc.closed {
				cancel()
			} else {
				defer cancel()
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/hook", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+ts.tokens["alice"])
			if tc.timeout != "" {
				req.Header.Set(timeoutHeader, tc.timeout)
			}
			rec := httptest.NewRecorder()
			ts.Handler().ServeHTTP(rec, req)

			var res board.HookResult
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("answer %d %q: %v", rec.Code, rec.Body, err)
			}
			if res.Unchecked != tc.late || (res.Decision == board.DecisionRefuse) == tc.late {
				t.Fatalf("decision %s, unchecked %v; want late %v", res.Decision, res.Unchecked, tc.late)
			}
			st := ts.Board().View(ts.now(), repo).Stats
			if st.Unheard != map[bool]int{true: 1}[tc.late] || st.Checks != map[bool]int{false: 1}[tc.late] {
				t.Fatalf("stats: %+v", st)
			}
			// A late one went ahead unchecked, whether its client left or
			// still waits, and counts towards the server's being degraded.
			if load := ts.answers.status(ts.requestClock()); load.PreEdits60s != 1 || load.Unchecked60s != int64(map[bool]int{true: 1}[tc.late]) {
				t.Fatalf("load: %+v", load)
			}
			ts.wait(0)
			var next board.HookResult
			ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPreEdit, "alice", "a1", "svc/pay/retry.go"), &next)
			if (next.Decision == board.DecisionRefuse) != tc.late {
				t.Fatalf("next edit: %s; the late one spent the bump: %v", next.Decision, !tc.late)
			}
		})
	}
}

// Stopping the server ends nothing it is still answering: a hook in flight
// when the signal comes is decided, not taken for one whose agent left.
func TestHookInFlightAtShutdownIsDecided(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var gate atomic.Bool
	clock := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(o *Options) {
		o.Now = func() time.Time {
			if gate.Load() {
				close(entered)
				<-release
			}
			return clock
		}
	})
	ts.do(t, "POST", "/v1/hook", "bob", hookEv(board.KindPostEdit, "bob", "b1", "svc/pay/retry.go"), nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ts.Serve(ctx, ln) }()

	gate.Store(true)
	ts.wait(3 * time.Second) // well past the guard
	answered := make(chan board.HookResult, 1)
	go func() {
		body, _ := json.Marshal(hookEv(board.KindPreEdit, "alice", "a1", "svc/pay/retry.go"))
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/hook", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ts.tokens["alice"])
		var res board.HookResult
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&res)
			_ = resp.Body.Close()
		}
		answered <- res
	}()
	<-entered
	gate.Store(false)
	stop()
	close(release)
	if res := <-answered; res.Decision != board.DecisionRefuse || res.Unchecked {
		t.Fatalf("hook in flight at shutdown: %+v", res)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// A scan's age, as its client says, runs to when the client sent it: the
// server adds the time the request then waited in it, to be read and
// admitted, so that a change a hook reported meanwhile, which git may not
// have seen, is kept. Before, the board measured the age back from when it
// took the request, and dropped a change made 150 ms after a scan began
// that waited 300 ms.
func TestScanAgeCountsTheTimeTheRequestWaited(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "svc/a.go"), nil)
	ts.mu.Lock()
	ts.clock = ts.clock.Add(250 * time.Millisecond)
	ts.mu.Unlock()
	scan := hookEv(board.KindToolEnd, "alice", "a1")
	scan.Footprint = &board.Footprint{Files: []board.PathRef{{Path: "svc/b.go"}}, AgeMS: 100}
	ts.wait(300 * time.Millisecond)
	if code := ts.do(t, "POST", "/v1/hook", "alice", scan, nil); code != http.StatusOK {
		t.Fatalf("the scan: %d", code)
	}
	ts.wait(0)
	var res board.CheckResult
	ts.do(t, "POST", "/v1/check", "bob", board.CheckRequest{Where: where("bob"), Paths: []board.PathRef{{Path: "svc/a.go"}}}, &res)
	if len(res.Conflicts) != 1 || res.Conflicts[0].Member != "alice" {
		t.Fatalf("bob's check of the file alice changed as the scan ran: %+v", res.Conflicts)
	}
}
