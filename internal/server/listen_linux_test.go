package server

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// The server's connections give up on a peer that acknowledges nothing for
// userTimeout, rather than after TCP's 15 minutes of retries.
func TestListenSetsTheUserTimeout(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	raw, err := accepted.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
	}); err != nil || gerr != nil {
		t.Fatal(err, gerr)
	}
	if want := int(userTimeout / time.Millisecond); got != want {
		t.Fatalf("TCP_USER_TIMEOUT of an accepted connection = %d ms, want %d", got, want)
	}
}
