package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// serve runs ts's server on a real listener, the way 'intagent serve' does,
// until the test ends, and returns its address.
func (ts *testServer) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	ts.url = "http://" + ln.Addr().String()
	return ln.Addr().String()
}

// closedWithin reports whether the server closes c, after whatever it
// answers, within d.
func closedWithin(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, c)
	return !errors.Is(err, os.ErrDeadlineExceeded)
}

// A request body sent slowly, or not at all, holds its connection only as
// long as the server's read timeout, with a token or without one.
func TestSlowBodiesAreCut(t *testing.T) {
	ts := newTestServer(t)
	ts.readTimeout = 200 * time.Millisecond
	addr := ts.serve(t)
	var conns []net.Conn
	for i := range 20 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		auth := ""
		if i%2 == 0 {
			auth = "Authorization: Bearer " + ts.tokens["alice"] + "\r\n"
		}
		fmt.Fprintf(c, "POST /v1/hook HTTP/1.1\r\nHost: x\r\n%sContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", auth)
		conns = append(conns, c)
	}
	for i, c := range conns {
		if !closedWithin(c, 5*time.Second) {
			t.Fatalf("connection %d (token: %v) was still open after 5 s", i, i%2 == 0)
		}
	}
}

// Past the connection limit, a new connection is closed at once rather than
// left to wait; once one closes, the server takes new ones again.
func TestConnectionLimit(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.MaxConnections = 3 })
	addr := ts.serve(t)
	var held []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		held = append(held, c)
	}
	over, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = over.Close() }()
	if !closedWithin(over, 5*time.Second) {
		t.Fatal("a connection past the limit was kept open")
	}
	if closedWithin(held[0], 100*time.Millisecond) {
		t.Fatal("a connection within the limit was closed")
	}
	_ = held[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		_ = c.Close()
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		// The server may not have seen the first close yet.
		if time.Now().After(deadline) {
			t.Fatalf("no new connection was served after one closed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOversizedHeadersAreRefused(t *testing.T) {
	ts := newTestServer(t)
	ts.serve(t)
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/healthz", nil)
	req.Header.Set("X-Filler", strings.Repeat("x", 64<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("64 KB of headers: %d", resp.StatusCode)
	}
}

// A dashboard's stream outlives the read timeout: it sends its request once
// and then only listens.
func TestStreamOutlivesReadTimeout(t *testing.T) {
	ts := newTestServer(t)
	ts.readTimeout = 100 * time.Millisecond
	ts.serve(t)
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/stream?repo="+repo, nil)
	req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v %v", resp, err)
	}
	defer func() { _ = resp.Body.Close() }()
	time.Sleep(5 * ts.readTimeout)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	if evs := readEvents(t, bufio.NewReader(resp.Body), 3); evs[2].Kind != board.ActivityFileChanged {
		t.Fatalf("events: %+v", evs)
	}
}

// A connection that has sent nothing does not hold up a stop: Serve closes
// it and returns at once, with no error. It waited five seconds for it, and
// then returned one.
func TestShutdownClosesUnusedConnections(t *testing.T) {
	ts := newTestServer(t)
	stop := serveTest(t, ts)
	idle, err := net.Dial("tcp", strings.TrimPrefix(ts.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idle.Close() }()
	// A request on another connection: by its answer the server has
	// accepted the idle one too, as it accepts in order.
	if code := ts.do(t, http.MethodGet, "/healthz", "", nil, nil); code != http.StatusOK {
		t.Fatalf("healthz: %d", code)
	}
	http.DefaultClient.CloseIdleConnections()
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("stopping took %s with one unused connection", took)
	}
	_ = idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("the unused connection is still open")
	}
}

// A connection that arrives once the unused ones are closed is closed as it
// arrives.
func TestFreshConnsCloseLateArrivals(t *testing.T) {
	f := &freshConns{conns: map[net.Conn]struct{}{}}
	early, earlyPeer := net.Pipe()
	defer func() { _ = earlyPeer.Close() }()
	f.track(early, http.StateNew)
	f.closeAll()
	late, latePeer := net.Pipe()
	defer func() { _ = latePeer.Close() }()
	f.track(late, http.StateNew)
	for name, c := range map[string]net.Conn{"early": early, "late": late} {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second)) // an open pipe waits for its reader
		if _, err := c.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("the %s connection is still open: %v", name, err)
		}
	}
}
