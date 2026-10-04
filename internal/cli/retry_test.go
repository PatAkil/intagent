package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

// Only a connection the server did not take is tried again: a reset or a
// timeout may come after the server decided the request, and a bump it
// spent must not be asked for twice.
func TestOnlyRefusedConnectionsAreRetried(t *testing.T) {
	timeout := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}
	reset := &url.Error{Op: "Post", URL: "x", Err: &net.OpError{Op: "read", Net: "tcp",
		Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", refusedErr(), true},
		{"dial timeout", timeout, false},
		{"reset", reset, false},
		{"answered", &client.APIError{Status: http.StatusServiceUnavailable}, false},
		{"none", nil, false},
	} {
		if got := refused(c.err); got != c.want {
			t.Errorf("%s: refused = %t", c.name, got)
		}
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
// half a second of its time is left. One that was reset is not tried again.
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
// spend seconds on every edit. A minute without a refusal forgets it.
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
	if serverDown(stamp, now.Add(2*time.Minute)) {
		t.Fatal("still down after two minutes without a refusal")
	}
	longDown(t, "http://example.test")
	clock, _ := fakePauses(t)
	tries := 0
	if err := sendRetrying(context.Background(), stamp, clock, func() error { tries++; return refusedErr() }); !refused(err) || tries != 1 {
		t.Fatalf("a server down: %d tries", tries)
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
