package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
)

// longDown marks the server at u as refusing this machine's hooks for a
// minute, so that a test of what a hook does without its server is not
// held up by the retries of one that is restarting.
func longDown(t *testing.T, u string) {
	t.Helper()
	stamp := downStamp(client.NormalizeURL(u))
	if stamp == "" {
		t.Fatal("no cache directory for the stamp")
	}
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(time.Now().Add(-time.Minute).UnixNano(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func refusedErr() error {
	return &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/hook", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}
}

// A refusal is a connection the server did not take. A timeout is not one,
// nor a reset: a reset is tried again only when the client found it before
// any of the answer came back (TestHookRetriesAResetConnection). A name that
// does not resolve and a network that cannot be reached are not tried again
// either: a restart does not cause them, and a laptop away from the team's
// network would wait out every hook's time.
func TestOnlyRefusedConnectionsAreRetried(t *testing.T) {
	timeout := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}
	reset := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "read", Net: "tcp",
		Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}}
	noHost := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "intagent.example", IsNotFound: true}}}
	unreachable := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENETUNREACH}}}
	windows := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &os.SyscallError{Syscall: "connectex", Err: wsaeconnrefused}}}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", refusedErr(), true},
		{"refused on Windows", windows, true},
		{"dial timeout", timeout, false},
		{"reset", reset, false},
		{"no such host", noHost, false},
		{"network unreachable", unreachable, false},
		{"answered", &client.APIError{Status: http.StatusServiceUnavailable}, false},
		{"none", nil, false},
	} {
		if got := refused(c.err); got != c.want {
			t.Errorf("%s: refused = %t", c.name, got)
		}
		if got := tryAgain(c.err); got != c.want {
			t.Errorf("%s: tryAgain = %t", c.name, got)
		}
	}
	if !tryAgain(&client.ResetError{Err: reset}) {
		t.Error("a reset before any of the answer is not tried again")
	}
	// A real refusal, from a port nobody listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, err = client.New("http://"+addr, "t", time.Second).Hook(context.Background(), board.HookEvent{})
	if !refused(err) {
		t.Fatalf("a closed port's error %v is not taken for a refusal", err)
	}
}

// fakePauses has sendRetrying wait on a clock of its own, which each pause
// moves on, and records the pauses.
func fakePauses(t *testing.T) (now func() time.Time, pauses *[]time.Duration) {
	clock := time.Now()
	var got []time.Duration
	old := pause
	pause = func(_ context.Context, d time.Duration) bool {
		got = append(got, d)
		clock = clock.Add(d)
		return true
	}
	t.Cleanup(func() { pause = old })
	return func() time.Time { return clock }, &got
}

// A hook refused by a server that restarts tries again after 100, 200 and
// then 400 ms, and gets its answer once the server is back; it stops while
// half a second of its time is left. A reset the client did not find before
// the answer began is not tried again.
func TestHookRetriesARefusedConnection(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stamp := downStamp("http://example.test")
	now, pauses := fakePauses(t)
	tries := 0
	err := sendRetrying(context.Background(), stamp, now, func() error {
		if tries++; tries < 5 {
			return refusedErr()
		}
		return nil
	})
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}
	if err != nil || tries != 5 || fmt.Sprint(*pauses) != fmt.Sprint(want) {
		t.Fatalf("%d tries after pauses %v (%v), want 5 after %v", tries, *pauses, err, want)
	}
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an answer left the refusals stamp: %v", err)
	}

	// With 1.2 s: tries at 0, 100, 300 and 700 ms; one more would leave less
	// than half a second for its answer.
	ctx, cancel := context.WithDeadline(context.Background(), now().Add(1200*time.Millisecond))
	defer cancel()
	tries = 0
	if err := sendRetrying(ctx, stamp, now, func() error { tries++; return refusedErr() }); !refused(err) || tries != 4 {
		t.Fatalf("within 1.2 s: %d tries, %v", tries, err)
	}

	tries = 0
	reset := errors.New("connection reset by peer")
	if err := sendRetrying(context.Background(), stamp, now, func() error { tries++; return reset }); !errors.Is(err, reset) || tries != 1 {
		t.Fatalf("a reset was tried %d times", tries)
	}
}

// A hook whose connection is reset before any of its answer comes back
// tries again as a refused one does, and gets its answer. A server that
// stops resets the connections its kernel had accepted and the server not
// yet: their hooks let their edits go ahead unchecked. Here the first two
// connections are reset once their request has arrived, and the third is
// answered. A reset is noted in the refusals stamp too, so that a server that
// resets every connection is soon tried only once.
func TestHookRetriesAResetConnection(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				if accepted.Add(1) <= 2 {
					_ = c.(*net.TCPConn).SetLinger(0) // the close resets it
					return
				}
				body := `{"decision":"allow","claim_id":"c_1"}`
				_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n"+
					"Connection: close\r\n\r\n%s", len(body), body)
			}()
		}
	}()
	url := "http://" + ln.Addr().String()
	stamp := downStamp(url)
	now, pauses := fakePauses(t)
	c := client.New(url, "t", 2*time.Second)
	tries := 0
	var res board.HookResult
	err = sendRetrying(context.Background(), stamp, now, func() error {
		tries++
		var err error
		if _, serr := os.Stat(stamp); tries == 2 && serr != nil {
			t.Errorf("the first reset is not in the refusals stamp: %v", serr)
		}
		res, err = c.Hook(context.Background(), board.HookEvent{Kind: board.KindPreEdit})
		return err
	})
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if err != nil || res.ClaimID != "c_1" || tries != 3 || fmt.Sprint(*pauses) != fmt.Sprint(want) {
		t.Fatalf("%d tries after pauses %v: %+v, %v; want 3 after %v", tries, *pauses, res, err, want)
	}
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the answer left the refusals stamp: %v", err)
	}
}

// A hook whose server is back answers as it would have: here a session
// start sent while the port was closed reaches a server that opens it
// 300 ms later.
func TestHookReachesAServerThatComesBack(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	served := make(chan *http.Server, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			served <- nil
			return
		}
		srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"decision":"allow","claim_id":"c_1"}`))
		})}
		served <- srv
		_ = srv.Serve(ln)
	}()
	defer func() {
		if srv := <-served; srv != nil {
			_ = srv.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c := client.New("http://"+addr, "t", 2*time.Second)
	var res board.HookResult
	err = sendRetrying(ctx, downStamp("http://"+addr), time.Now, func() error {
		var err error
		res, err = c.Hook(ctx, board.HookEvent{Kind: board.KindSessionStart})
		return err
	})
	if err != nil || res.ClaimID != "c_1" {
		t.Fatalf("a hook to a server back 300 ms later: %+v, %v", res, err)
	}
}

// Once this machine's hooks have been refused for 10 seconds, the server is
// taken for down: a hook tries once, and goes ahead at once, rather than
// spend seconds on every edit. It stays down until it answers, whatever it
// says, however long between hooks: before, a minute without a refusal
// forgot it, and each edit made a minute or more apart waited out its
// time again.
func TestServerDownStampTriesOnce(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stamp := downStamp("http://example.test")
	now := time.Now()
	if serverDown(stamp, now) || serverDown(stamp, now.Add(9*time.Second)) {
		t.Fatal("down before 10 seconds of refusals")
	}
	if !serverDown(stamp, now.Add(11*time.Second)) {
		t.Fatal("not down after 11 seconds of refusals")
	}
	if !serverDown(stamp, now.Add(2*time.Hour)) {
		t.Fatal("not down after two hours of refusals, the last two hours ago")
	}
	longDown(t, "http://example.test")
	clock, _ := fakePauses(t)
	tries := 0
	if err := sendRetrying(context.Background(), stamp, clock, func() error { tries++; return refusedErr() }); !refused(err) || tries != 1 {
		t.Fatalf("a server down: %d tries", tries)
	}
	for _, answer := range []error{&client.APIError{Status: http.StatusUnauthorized}, nil} {
		longDown(t, "http://example.test")
		if err := sendRetrying(context.Background(), stamp, clock, func() error { return answer }); !errors.Is(err, answer) {
			t.Fatal(err)
		}
		if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the answer %v left the server down: %v", answer, err)
		}
	}
}

// The refusals stamp is written whole: a hook that reads it while another
// writes it finds the old time or the new one. Written in place, it was
// empty for a moment, and a hook that read it then took the refusals for
// new and wrote its own time, putting off the down state for every hook.
// Here each call finds the stamp in its future, as after the clock went
// back, and writes it again.
func TestRefusalsStampIsNeverReadTorn(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stamp := downStamp("http://example.test")
	start := time.Now()
	serverDown(stamp, start)
	done := make(chan struct{})
	torn := make(chan string, 1)
	go func() {
		defer close(torn)
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(stamp)
			if err != nil {
				continue // between a rename's unlink and link, on some systems
			}
			if _, err := strconv.ParseInt(string(data), 10, 64); err != nil {
				torn <- string(data)
				return
			}
		}
	}()
	for i := 1; i <= 500; i++ {
		serverDown(stamp, start.Add(-time.Duration(i)*time.Millisecond))
	}
	close(done)
	if data, ok := <-torn; ok {
		t.Fatalf("a hook read the stamp as %q while another wrote it", data)
	}
}

// The hook command waits for a server that comes back within its time: an
// edit sent while the port was closed is refused by the server that opens
// it 300 ms later. Without the retry it went ahead unchecked.
func TestHookCommandWaitsForAServerThatComesBack(t *testing.T) {
	tm := newTeam(t, "alice")
	a := tm.clone("alice")
	tm.enrol(map[string]string{"alice": a})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	served := make(chan *http.Server, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			served <- nil
			return
		}
		srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"decision":"deny","reason":"[intagent] bob holds this file"}`))
		})}
		served <- srv
		_ = srv.Serve(ln)
	}()
	defer func() {
		if srv := <-served; srv != nil {
			_ = srv.Close()
		}
	}()
	t.Setenv("INTAGENT_URL", "http://"+addr)
	t.Setenv("INTAGENT_TOKEN", tm.tokens["alice"])
	edit := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(a, "README.md")}}
	out, errOut, code := tm.as("alice", a, claudeEvent("a1", a, "PreToolUse", edit), "hook", "claude-code")
	if dec, reason, _ := decision(t, out); code != 0 || dec != "deny" || reason != "[intagent] bob holds this file" {
		t.Fatalf("an edit while the server restarted: %q %q (exit %d, %s)", dec, reason, code, errOut)
	}
}
