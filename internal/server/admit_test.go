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
