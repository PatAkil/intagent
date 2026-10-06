package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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

// openStream opens a dashboard stream for member on a connection of its
// own, and returns the connection once the stream has begun.
func openStream(t *testing.T, ts *testServer, addr, member string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprintf(c, "GET /v1/stream?repo=%s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", repo, ts.tokens[member])
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v %v", resp, err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c
}

// idleConn opens a connection that asks for /healthz, reads the answer and
// stays open, idle, as a client's kept-alive connection does. It needs no
// token.
func idleConn(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = c.SetReadDeadline(time.Time{})
	return c
}

// sendHook sends alice's prompt hook on a new connection, as a hook process
// does, and returns the connection once it is answered, kept alive or
// closed by the server as asked.
func sendHook(ts *testServer, addr string, keepAlive bool) (net.Conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(hookEv(board.KindPrompt, "alice", "a1"))
	conn := "keep-alive"
	if !keepAlive {
		conn = "close"
	}
	fmt.Fprintf(c, "POST /v1/hook HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nConnection: %s\r\n\r\n%s", ts.tokens["alice"], len(body), conn, body)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, nil
}

// Connections the server is busy with hold the cap: past it, with every
// connection busy, a new connection is closed at once rather than left to
// wait; once one closes, the server takes new ones again.
func TestBusyConnectionsHoldTheCap(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.MaxConnections = 3 })
	addr := ts.serve(t)
	var streams []net.Conn
	for range 3 {
		streams = append(streams, openStream(t, ts, addr, "bob"))
	}
	over, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = over.Close() }()
	if !closedWithin(over, 5*time.Second) {
		t.Fatal("a connection past the limit was kept open")
	}
	for i, c := range streams {
		if closedWithin(c, 50*time.Millisecond) {
			t.Fatalf("stream %d, within the limit, was closed", i)
		}
	}
	_ = streams[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := sendHook(ts, addr, false)
		if err == nil {
			_ = c.Close()
			break
		}
		// The server may not have seen the first close yet.
		if time.Now().After(deadline) {
			t.Fatalf("no new connection was served after one closed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Idle connections cannot lock members out: a client without a token that
// holds the cap's worth of kept-alive connections loses the longest idle
// of them to each hook that needs room, and every hook is answered. Each
// was closed as it arrived, and every hook of the team failed open.
func TestIdleConnectionsMakeRoom(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.MaxConnections = 8 })
	addr := ts.serve(t)
	var held []net.Conn
	for range 8 {
		held = append(held, idleConn(t, addr))
	}
	for i := range 20 {
		c, err := sendHook(ts, addr, false)
		if err != nil {
			t.Fatalf("hook %d, with 8 idle connections held: %v", i, err)
		}
		_ = c.Close()
	}
	// The longest idle went first: those closed are the first held.
	closed := 0
	for i, c := range held {
		gone := closedWithin(c, 50*time.Millisecond)
		if gone && closed < i {
			t.Fatalf("idle connection %d was closed, and %d before it was not", i, closed)
		}
		if gone {
			closed++
		}
	}
	if closed == 0 {
		t.Fatal("no idle connection made room")
	}
}

// Nor can connections that send their requests slowly, or never: one that
// has sent nothing, one trickling its headers, one trickling a body without
// a token, and one trickling a member's body each wait on their client, and
// each gives way, the longest waiting first, to hooks that need room,
// however recently it sent a byte.
func TestSlowRequestsMakeRoom(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.MaxConnections = 8 })
	addr := ts.serve(t)
	starts := []string{
		"",
		"GET /healthz HTTP/1.1\r\nHost: x\r\n",
		"POST /v1/hook HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n{",
		"POST /v1/hook HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + ts.tokens["bob"] +
			"\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{",
	}
	trickles := []string{"", "X-Slow: 1\r\n", " ", " "}
	var held []net.Conn
	for i := range 8 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		held = append(held, c)
		kind := i % len(starts)
		if _, err := io.WriteString(c, starts[kind]); err != nil {
			t.Fatal(err)
		}
		if trickles[kind] != "" {
			go func() {
				for {
					time.Sleep(10 * time.Millisecond)
					if _, err := io.WriteString(c, trickles[kind]); err != nil {
						return
					}
				}
			}()
		}
	}
	// Each hook keeps its connection, idle, which then waits for less long
	// than any held: eight hooks take the place of all eight.
	for i := range 8 {
		c, err := sendHook(ts, addr, true)
		if err != nil {
			t.Fatalf("hook %d, with 8 slow connections held: %v", i, err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	for i, c := range held {
		if !closedWithin(c, 5*time.Second) {
			t.Fatalf("slow connection %d (%q) was kept open", i, starts[i%len(starts)])
		}
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
// it once it has been quiet for unusedAfter, and returns with no error. It
// waited five seconds for it, and then returned one.
func TestShutdownClosesUnusedConnections(t *testing.T) {
	ts := newTestServer(t)
	ts.unusedAfter = 50 * time.Millisecond
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

// A connection whose request is under way as the server starts to stop is
// answered, over TLS too: one whose request's first line has arrived, one
// that has connected and sent nothing yet, and a kept-alive one whose next
// request has begun to arrive; the rest of each comes after the stop began,
// when a new connection is refused, which a hook tries again. Every
// connection that had not sent a whole request was closed, and its hook went
// ahead unchecked; then from Go 1.25 Shutdown dropped a request that
// finished arriving after it began, and before that closed a kept-alive
// connection whose next request was arriving.
func TestShutdownAnswersARequestUnderWay(t *testing.T) {
	cert, pool := ecdsaCert(t)
	const request = "GET /healthz HTTP/1.1\r\nHost: intagent\r\n\r\n"
	for _, secure := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			before string // what is sent of the request before the stop
			kept   bool   // whether a request was answered on the connection first
		}{
			{"first line sent", "GET /healthz HTTP/1.1\r\n", false},
			{"nothing sent", "", false},
			{"kept alive", "GET /healthz HTTP/1.1\r\n", true},
		} {
			t.Run(fmt.Sprintf("TLS %t, %s", secure, tc.name), func(t *testing.T) {
				ts := newTestServer(t, func(o *Options) {
					if secure {
						o.TLS = TLSConfig([]tls.Certificate{cert})
					}
				})
				ts.unusedAfter = 2 * time.Second
				stop := serveTest(t, ts)
				addr := strings.TrimPrefix(ts.url, "http://")
				var c net.Conn
				var err error
				if secure {
					c, err = tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
				} else {
					c, err = net.Dial("tcp", addr)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				r := bufio.NewReader(c)
				answered := func(when string) {
					t.Helper()
					resp, err := http.ReadResponse(r, nil)
					if err != nil {
						t.Fatalf("%s: %v", when, err)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("%s: %d", when, resp.StatusCode)
					}
				}
				if tc.kept {
					if _, err := io.WriteString(c, request); err != nil {
						t.Fatal(err)
					}
					answered("the first request")
				}
				if _, err := io.WriteString(c, tc.before); err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Millisecond) // the server reads it
				stopped := make(chan error, 1)
				go func() { stopped <- stop() }()
				time.Sleep(100 * time.Millisecond) // the stop has begun
				if late, err := net.Dial("tcp", addr); err == nil {
					_ = late.Close()
					t.Error("a connection made after the stop began was accepted")
				}
				if _, err := io.WriteString(c, strings.TrimPrefix(request, tc.before)); err != nil {
					t.Fatalf("the rest of the request: %v", err)
				}
				answered("a request under way as the server stopped")
				if err := <-stopped; err != nil {
					t.Fatalf("Serve: %v", err)
				}
			})
		}
	}
}

// A request still arriving when the server has waited arriveWithin for it
// does not hold the stop up: its connection is closed unanswered, and the
// stop goes on. The server waited for the client to stop sending for
// unusedAfter, or for Serve's five seconds, and from Go 1.25 would have
// dropped the request had it arrived.
func TestShutdownClosesARequestStillArriving(t *testing.T) {
	logs := &logRecorder{}
	ts := newTestServer(t, func(o *Options) { o.Logger = slog.New(logs) })
	ts.unusedAfter, ts.arriveWithin = 2*time.Second, 200*time.Millisecond
	stop := serveTest(t, ts)
	c, err := net.Dial("tcp", strings.TrimPrefix(ts.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nX-Slow: "); err != nil {
		t.Fatal(err)
	}
	go func() { // a byte of the header every 50 ms, until the server closes it
		for {
			time.Sleep(50 * time.Millisecond)
			if _, err := io.WriteString(c, "x"); err != nil {
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("stopping took %s with a request still arriving", took)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if errors.Is(err, os.ErrDeadlineExceeded) || len(got) != 0 {
		t.Fatalf("the connection of the request still arriving: %q, %v", got, err)
	}
	if closed := logs.lines("closed connections whose requests had not arrived"); len(closed) != 1 {
		t.Fatalf("logged %q", logs.lines(""))
	}
}

// An HTTP/2 connection is left to Shutdown, which lets its requests finish
// and turns new ones away for their clients to try again: one that has sent
// its preface and settings, and no request, does not hold the stop up for
// arriveWithin.
func TestShutdownLeavesHTTP2ToShutdown(t *testing.T) {
	cert, pool := ecdsaCert(t)
	ts := newTestServer(t, func(o *Options) { o.TLS = TLSConfig([]tls.Certificate{cert}) })
	ts.arriveWithin = 3 * time.Second
	stop := serveTest(t, ts)
	c, err := tls.Dial("tcp", strings.TrimPrefix(ts.url, "http://"),
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if p := c.ConnectionState().NegotiatedProtocol; p != "h2" {
		t.Fatalf("negotiated %q", p)
	}
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// The server reads the preface, and the connection goes idle; then an
	// empty SETTINGS frame arrives on it.
	time.Sleep(100 * time.Millisecond)
	if _, err := c.Write([]byte{0, 0, 0, 0x4, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	go func() { // the client hangs up at the server's GOAWAY, as clients do
		defer func() { _ = c.Close() }()
		head := make([]byte, 9)
		for {
			if _, err := io.ReadFull(c, head); err != nil {
				return
			}
			if head[3] == 0x7 {
				return
			}
			if _, err := io.CopyN(io.Discard, c, int64(head[0])<<16|int64(head[1])<<8|int64(head[2])); err != nil {
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("stopping took %s with an idle HTTP/2 connection", took)
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

// Over TLS the table finds its connection under the TLS one, for HTTP/1.1
// and HTTP/2 alike: an idle connection makes room for a hook, and a
// dashboard's stream, older but busy, does not.
func TestEncryptedConnectionsMakeRoom(t *testing.T) {
	cert, pool := ecdsaCert(t)
	for _, proto := range []string{"HTTP/1.1", "HTTP/2.0"} {
		t.Run(proto, func(t *testing.T) {
			ts := newTestServer(t, func(o *Options) {
				o.MaxConnections = 2
				o.TLS = TLSConfig([]tls.Certificate{cert})
			})
			base := "https://" + ts.serve(t)
			client := func() *http.Client {
				tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}}
				if proto == "HTTP/2.0" {
					tr.ForceAttemptHTTP2 = true
				} else {
					tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
				}
				t.Cleanup(tr.CloseIdleConnections)
				return &http.Client{Transport: tr}
			}
			req, _ := http.NewRequest(http.MethodGet, base+"/v1/stream?repo="+repo, nil)
			req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
			resp, err := client().Do(req)
			if err != nil || resp.StatusCode != http.StatusOK || resp.Proto != proto {
				t.Fatalf("stream: %v %v", resp, err)
			}
			defer func() { _ = resp.Body.Close() }()
			idle, err := client().Get(base + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, idle.Body)
			_ = idle.Body.Close()
			body, _ := json.Marshal(hookEv(board.KindPrompt, "alice", "a1"))
			req, _ = http.NewRequest(http.MethodPost, base+"/v1/hook", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+ts.tokens["alice"])
			hook, err := client().Do(req)
			if err != nil || hook.StatusCode != http.StatusOK {
				t.Fatalf("a hook with the cap held by a stream and an idle connection: %v %v", hook, err)
			}
			_ = hook.Body.Close()
			// The stream hears of the hook's session: it was not closed.
			if evs := readEvents(t, bufio.NewReader(resp.Body), 1); evs[0].Member != "alice" {
				t.Fatalf("stream: %+v", evs)
			}
		})
	}
}

// fromConn is one end of a pipe, which says it comes from addr.
type fromConn struct {
	net.Conn
	addr net.Addr
}

func (c fromConn) RemoteAddr() net.Addr { return c.addr }

// tableTest is a connTable on a clock that moves only when told, and the
// connections it admits, by name.
type tableTest struct {
	t     *testing.T
	table *connTable
	now   atomic.Int64
	conns map[string]*limitConn
	peers map[string]net.Conn
}

func newTableTest(t *testing.T, max int) *tableTest {
	tt := &tableTest{t: t, table: newConnTable(max, slog.New(slog.NewTextHandler(io.Discard, nil))),
		conns: map[string]*limitConn{}, peers: map[string]net.Conn{}}
	tt.now.Store(1)
	tt.table.clock = func() int64 { return tt.now.Add(1) }
	return tt
}

// admit offers the table a connection named name from ip, and returns the
// name of the one it closed to make room ("" for none), or "refused".
func (tt *tableTest) admit(name, ip string) string {
	tt.t.Helper()
	c, peer := net.Pipe()
	tt.t.Cleanup(func() { _ = c.Close(); _ = peer.Close() })
	lc, evicted := tt.table.admit(fromConn{Conn: c, addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}})
	if lc == nil {
		return "refused"
	}
	tt.conns[name], tt.peers[name] = lc, peer
	if evicted == nil {
		return ""
	}
	_ = evicted.Close()
	for n, o := range tt.conns {
		if o == evicted {
			delete(tt.conns, n)
			return n
		}
	}
	tt.t.Fatal("closed a connection the test does not know")
	return ""
}

// Room is made from the client with the most connections waiting on it, and
// of those from the one that has waited longest: one client's connections
// give way to each other before anyone else's do, however long a teammate's
// has waited. An IPv6 client counts as its /64.
func TestConnSourcesShareTheCap(t *testing.T) {
	tt := newTableTest(t, 4)
	for _, c := range []struct{ name, ip string }{{"b1", "10.0.0.2"}, {"a1", "10.0.0.1"}, {"a2", "10.0.0.1"}, {"a3", "10.0.0.1"}} {
		if got := tt.admit(c.name, c.ip); got != "" {
			t.Fatalf("%s made room for itself under the cap: %s", c.name, got)
		}
	}
	// a has three waiting and b one, though b's has waited longest.
	if got := tt.admit("b2", "10.0.0.2"); got != "a1" {
		t.Fatalf("b2 closed %q, want a1, the longest waiting of the client with the most", got)
	}
	if got := tt.admit("c1", "10.0.0.3"); got != "b1" {
		t.Fatalf("c1 closed %q, want b1: two clients wait on two each, and b1 has waited longest", got)
	}
	if got := tt.admit("a4", "10.0.0.1"); got != "a2" {
		t.Fatalf("a4 closed %q, want a2", got)
	}
	// IPv6 clients count by /64.
	tt = newTableTest(t, 3)
	tt.admit("x1", "2001:db8::1")
	tt.admit("y1", "2001:db8:0:1::1")
	tt.admit("x2", "2001:db8::2")
	if got := tt.admit("y2", "2001:db8:0:1::2"); got != "x1" {
		t.Fatalf("y2 closed %q, want x1: 2001:db8::1 and ::2 are one client", got)
	}
}

// A connection whose request has arrived is busy, and never closed to make
// room; one whose answer waits on its client to read, for a second or more,
// is, when nothing waits on its request. Past that, a new connection is
// refused.
func TestStuckWritesMakeRoom(t *testing.T) {
	tt := newTableTest(t, 2)
	tt.admit("reader", "10.0.0.1")
	tt.admit("busy", "10.0.0.2")
	for _, name := range []string{"reader", "busy"} {
		tt.table.arrive(&request{c: tt.conns[name]})
	}
	if got := tt.admit("new", "10.0.0.3"); got != "refused" {
		t.Fatalf("with every connection busy, a new one closed %q", got)
	}
	// reader's client stops reading its answer: the pipe's write waits.
	reader := tt.conns["reader"]
	go func() { _, _ = reader.Write([]byte("an answer")) }()
	for reader.writing.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := tt.admit("new", "10.0.0.3"); got != "refused" {
		t.Fatalf("a write that has just begun made room: %q", got)
	}
	tt.now.Add(int64(stuckWrite))
	if got := tt.admit("new", "10.0.0.3"); got != "reader" {
		t.Fatalf("with a write stuck for %s, a new connection closed %q, want reader", stuckWrite, got)
	}
}

// A kept-alive connection waits on its client from when it went idle, and
// again from when its next request begins to arrive: one that has been idle
// long and then sends a request is not cut off mid-request for its idleness,
// while one that trickles bytes gains nothing by them.
func TestKeptAliveConnectionsWaitAgain(t *testing.T) {
	tt := newTableTest(t, 3)
	tt.admit("old", "10.0.0.1")
	tt.admit("trickling", "10.0.0.1")
	tt.admit("young", "10.0.0.1")
	for _, name := range []string{"old", "trickling", "young"} {
		r := &request{c: tt.conns[name]}
		tt.table.arrive(r)
		tt.table.finish(r)
		tt.table.state(tt.conns[name], http.StateIdle)
	}
	// trickling's next request begins, then old's, and trickling sends
	// another byte.
	for _, name := range []string{"trickling", "old", "trickling"} {
		peer := tt.peers[name]
		go func() { _, _ = peer.Write([]byte("G")) }()
		if _, err := tt.conns[name].Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tt.admit("new", "10.0.0.1"); got != "young" {
		t.Fatalf("closed %q, want young, idle the longest since the others' next requests began", got)
	}
	if got := tt.admit("newer", "10.0.0.1"); got != "trickling" {
		t.Fatalf("closed %q, want trickling, whose second byte did not start its wait again", got)
	}
}

// An HTTP/2 connection carries requests side by side: it is busy while any
// that has arrived whole is being handled, and the frames it reads then are
// no new request's; once only requests still arriving are left, it waits on
// its client. One whose only quick request was answered, while another's
// body trickled, held its place; one whose client sent a window update after
// it went idle was taken for idle again while busy.
func TestConnectionsWaitPerRequest(t *testing.T) {
	tt := newTableTest(t, 1)
	tt.admit("h2", "10.0.0.1")
	c, peer := tt.conns["h2"], tt.peers["h2"]
	tt.table.state(c, http.StateIdle) // as the HTTP/2 server starts serving it
	quick, slow := &request{c: c}, &request{c: c}
	tt.table.arrive(quick)
	go func() { _, _ = peer.Write([]byte("a window update")) }()
	if _, err := c.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if got := tt.admit("new", "10.0.0.2"); got != "refused" {
		t.Fatalf("a connection busy with a request made room: %q", got)
	}
	tt.table.finish(quick)
	if got := tt.admit("new", "10.0.0.2"); got != "h2" {
		t.Fatalf("closed %q, want h2, whose only request left is still arriving", got)
	}
	tt.table.finish(slow) // its handler returns as the connection closes
}
