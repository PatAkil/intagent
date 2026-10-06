package client

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// resetServer accepts connections on a local port and does to each what
// handle does, then resets it: it closes it at once with an RST, as a
// stopping server's kernel does to the connections still waiting to be
// accepted. It returns the server's URL.
func resetServer(t *testing.T, handle func(c *net.TCPConn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := c.(*net.TCPConn)
				handle(tc)
				_ = tc.SetLinger(0)
				_ = tc.Close()
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// readRequest reads one whole request from c.
func readRequest(c net.Conn) {
	if req, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
}

// A request whose connection is reset before any byte of an answer comes
// back says so (ResetBeforeAnswer), whether the reset met it in its TLS
// handshake, as it was still being sent, a large footprint with it, or once
// it had all been sent; one reset after its answer began does not. Such a
// reset was an error like any other, and the hook let its edit go ahead
// unchecked.
func TestResetBeforeAnswer(t *testing.T) {
	large := board.HookEvent{Kind: board.KindStop, Footprint: &board.Footprint{Files: make([]board.PathRef, 50_000)}}
	for i := range large.Footprint.Files {
		large.Footprint.Files[i].Path = "svc/pay/" + strings.Repeat("x", 40)
	}
	for _, c := range []struct {
		name   string
		ev     board.HookEvent
		handle func(c *net.TCPConn)
		want   bool
	}{
		{"reset as it is accepted", board.HookEvent{Kind: board.KindPreEdit}, func(*net.TCPConn) {}, true},
		{"reset in its TLS handshake", board.HookEvent{Kind: board.KindPreEdit}, func(c *net.TCPConn) {
			_, _ = c.Read(make([]byte, 1)) // the start of its ClientHello
		}, true},
		{"reset while a large request is sent", large, func(*net.TCPConn) {}, true},
		{"reset once the request has arrived", board.HookEvent{Kind: board.KindPreEdit}, func(c *net.TCPConn) { readRequest(c) }, true},
		{"reset as the answer is sent", board.HookEvent{Kind: board.KindPreEdit}, func(c *net.TCPConn) {
			readRequest(c)
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n")
			time.Sleep(100 * time.Millisecond) // the client reads the first line
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := resetServer(t, c.handle)
			if strings.Contains(c.name, "TLS") {
				url = "https" + strings.TrimPrefix(url, "http")
			}
			for range 5 {
				_, err := New(url, "t", 2*time.Second).Hook(context.Background(), c.ev)
				if err == nil {
					t.Fatal("a reset connection answered")
				}
				if got := ResetBeforeAnswer(err); got != c.want {
					t.Fatalf("ResetBeforeAnswer(%v) = %t", err, got)
				}
			}
		})
	}
}

// A request that timed out, or whose server closed the connection cleanly
// before answering, was not reset.
func TestTimeoutIsNotAReset(t *testing.T) {
	url := resetServer(t, func(c *net.TCPConn) {
		readRequest(c)
		time.Sleep(300 * time.Millisecond)
	})
	if _, err := New(url, "t", 100*time.Millisecond).Hook(context.Background(), board.HookEvent{}); err == nil || ResetBeforeAnswer(err) {
		t.Fatalf("a request that timed out: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, err := ln.Accept(); err == nil {
			readRequest(c)
			_ = c.Close()
		}
	}()
	_, err = New("http://"+ln.Addr().String(), "t", time.Second).Hook(context.Background(), board.HookEvent{})
	if err == nil || ResetBeforeAnswer(err) {
		t.Fatalf("a connection closed without an answer: %v", err)
	}
}
