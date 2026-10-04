package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// heartbeat is a hook event with a footprint of about size bytes.
func heartbeat(member string, size int) board.HookEvent {
	ev := hookEv(board.KindHeartbeat, member, member+"-s")
	ev.Footprint = &board.Footprint{}
	for i := 0; i*100 < size; i++ {
		ev.Footprint.Files = append(ev.Footprint.Files, board.PathRef{Path: fmt.Sprintf("src/%080d.go", i)})
	}
	return ev
}

// post sends ev as member and returns the status and the Retry-After header.
func (ts *testServer) post(t *testing.T, path, member string, ev any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(ev)
	req, _ := http.NewRequest(http.MethodPost, ts.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+ts.tokens[member])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// A member's large bodies draw on a budget of bytes, charged before they are
// read; small ones, which check edits, never do, and other members keep
// their own budgets.
func TestLargeBodiesAreMetered(t *testing.T) {
	ts := newTestServer(t)
	ts.admit.byteCap = 100 << 10
	big := heartbeat("alice", 40<<10)
	for i := range 2 {
		if code, _ := ts.post(t, "/v1/hook", "alice", big); code != http.StatusOK {
			t.Fatalf("large body %d within the budget: %d", i, code)
		}
	}
	if code, retry := ts.post(t, "/v1/hook", "alice", big); code != http.StatusTooManyRequests || retry != "1" {
		t.Fatalf("large body over the budget: %d, Retry-After %q", code, retry)
	}
	if code, _ := ts.post(t, "/v1/hook", "alice", hookEv(board.KindPreEdit, "alice", "alice-s", "a/b.go")); code != http.StatusOK {
		t.Fatalf("a small hook over the budget: %d", code)
	}
	if code, _ := ts.post(t, "/v1/hook", "bob", heartbeat("bob", 40<<10)); code != http.StatusOK {
		t.Fatalf("another member's large body: %d", code)
	}
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Second)
	ts.mu.Unlock()
	if code, _ := ts.post(t, "/v1/hook", "alice", big); code != http.StatusOK {
		t.Fatalf("large body after a second: %d", code)
	}
}

// Large bodies wait for one of a few slots to be decoded and handled, and
// give up with 503 after a while; small ones never wait.
func TestLargeBodiesWaitForASlot(t *testing.T) {
	ts := newTestServer(t)
	ts.admit.largeWait = 50 * time.Millisecond
	for range cap(ts.admit.large) {
		ts.admit.large <- struct{}{} // every slot busy with another large body
	}
	if code, retry := ts.post(t, "/v1/hook", "alice", heartbeat("alice", 40<<10)); code != http.StatusServiceUnavailable || retry != "1" {
		t.Fatalf("large body with no slot free: %d, Retry-After %q", code, retry)
	}
	if code, _ := ts.post(t, "/v1/hook", "alice", hookEv(board.KindPreEdit, "alice", "alice-s", "a/b.go")); code != http.StatusOK {
		t.Fatalf("small hook with no slot free: %d", code)
	}
	<-ts.admit.large
	if code, _ := ts.post(t, "/v1/hook", "alice", heartbeat("alice", 40<<10)); code != http.StatusOK {
		t.Fatalf("large body with a slot free: %d", code)
	}
	if n := len(ts.admit.large); n != cap(ts.admit.large)-1 {
		t.Fatalf("%d slots taken after the request, want %d", n, cap(ts.admit.large)-1)
	}
}

// A large body sent slowly holds its connection until the read timeout, but
// no slot: other large bodies go on being handled.
func TestSlowLargeBodyHoldsNoSlot(t *testing.T) {
	ts := newTestServer(t)
	ts.admit.largeWait = 50 * time.Millisecond
	addr := ts.serve(t)
	for range cap(ts.admit.large) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		fmt.Fprintf(c, "POST /v1/hook HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
			ts.tokens["alice"], 40<<10, strings.Repeat(" ", 1000))
	}
	// Wait until the server has charged them all, and so is reading them.
	charged := func() bool {
		ts.admit.mu.Lock()
		defer ts.admit.mu.Unlock()
		b := ts.admit.bytes["alice"]
		return b != nil && b.tokens <= ts.admit.byteCap-float64(cap(ts.admit.large)*(40<<10))
	}
	for deadline := time.Now().Add(5 * time.Second); !charged(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the slow bodies never reached the server")
		}
	}
	if code, _ := ts.post(t, "/v1/hook", "bob", heartbeat("bob", 40<<10)); code != http.StatusOK {
		t.Fatalf("large body while others are sent slowly: %d", code)
	}
}

func checkReq(member, worktree string, paths int) board.CheckRequest {
	w := where(member)
	w.Worktree = worktree
	req := board.CheckRequest{Where: w}
	for i := range paths {
		req.Paths = append(req.Paths, board.PathRef{Path: fmt.Sprintf("docs/%d.md", i)})
	}
	return req
}

// Checks and declarations from one worktree are paced: a burst, then a few a
// second. Other worktrees, other members and hooks are not held back.
func TestChecksArePacedPerWorktree(t *testing.T) {
	ts := newTestServer(t)
	for i := range callBurst {
		if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusOK {
			t.Fatalf("check %d: %d", i, code)
		}
	}
	if code, retry := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusTooManyRequests || retry != "1" {
		t.Fatalf("check past the burst: %d, Retry-After %q", code, retry)
	}
	declare := board.DeclareRequest{Where: checkReq("alice", "/w/a", 0).Where, Patterns: []string{"a/**"}}
	if code, _ := ts.post(t, "/v1/intents", "alice", declare); code != http.StatusTooManyRequests {
		t.Fatalf("declare past the burst: %d", code)
	}
	for _, c := range []struct{ member, worktree string }{{"alice", "/w/a2"}, {"bob", "/w/a"}} {
		if code, _ := ts.post(t, "/v1/check", c.member, checkReq(c.member, c.worktree, 1)); code != http.StatusOK {
			t.Fatalf("%s's check from %s: %d", c.member, c.worktree, code)
		}
	}
	pre := hookEv(board.KindPreEdit, "alice", "a1", "a/b.go")
	pre.Where.Worktree = "/w/a"
	if code, _ := ts.post(t, "/v1/hook", "alice", pre); code != http.StatusOK {
		t.Fatalf("a hook from the paced worktree: %d", code)
	}
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Second)
	ts.mu.Unlock()
	for i := range callRate {
		if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusOK {
			t.Fatalf("check %d a second later: %d", i, code)
		}
	}
	if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusTooManyRequests {
		t.Fatalf("check past the rate: %d", code)
	}
}

// One check or declaration at a time from a worktree.
func TestOneCheckAtATimePerWorktree(t *testing.T) {
	ts := newTestServer(t)
	req := checkReq("alice", "/w/a", 1)
	release, why := ts.admit.call("alice\x00"+req.Where.Host+"\x00/w/a", ts.now())
	if why != "" {
		t.Fatal(why)
	}
	var e ErrorResponse
	if code := ts.do(t, "POST", "/v1/check", "alice", req, &e); code != http.StatusTooManyRequests || !strings.Contains(e.Error, "still running") {
		t.Fatalf("check while another runs: %d %q", code, e.Error)
	}
	release()
	if code, _ := ts.post(t, "/v1/check", "alice", req); code != http.StatusOK {
		t.Fatalf("check after the other ended: %d", code)
	}
}

// Paced worktrees that have refilled are dropped once there are many: the
// buckets kept are at most about twice those in use.
func TestPacingForgetsIdleWorktrees(t *testing.T) {
	a := newAdmission()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	call := func(key string, at time.Time) {
		release, why := a.call(key, at)
		if why != "" {
			t.Fatal(why)
		}
		release()
	}
	for i := range 3 * minPrune {
		call(fmt.Sprint("early", i), now)
	}
	most := 0
	for i := range 3 * minPrune {
		call(fmt.Sprint("late", i), now.Add(time.Minute))
		most = max(most, len(a.calls))
	}
	if n := len(a.calls); n != 3*minPrune {
		t.Fatalf("%d buckets after the early ones refilled, want %d", n, 3*minPrune)
	}
	if most > 2*3*minPrune {
		t.Fatalf("up to %d buckets for %d in use", most, 3*minPrune)
	}
}

// A check names at most as many paths as a claim keeps, and is refused
// rather than cut short.
func TestOversizedCheckIsRefused(t *testing.T) {
	ts := newTestServer(t)
	if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 2000)); code != http.StatusOK {
		t.Fatalf("2000 paths: %d", code)
	}
	var e ErrorResponse
	if code := ts.do(t, "POST", "/v1/check", "alice", checkReq("alice", "/w/a", 2001), &e); code != http.StatusBadRequest || !strings.Contains(e.Error, "at most 2000") {
		t.Fatalf("2001 paths: %d %q", code, e.Error)
	}
}
