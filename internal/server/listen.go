package server

import (
	"context"
	"net"
	"time"
)

// userTimeout is how long, on Linux, a connection may hold data its peer has
// not acknowledged before the kernel gives up on it. Without it a dashboard
// that vanished (a laptop that slept, a NAT that forgot it) keeps its
// connection, and up to 4 MB of unsent stream in the kernel, until TCP's
// retries run out after about 15 minutes. Live peers acknowledge within
// seconds even when they read nothing.
const userTimeout = 60 * time.Second

// Listen opens the server's TCP listener on addr.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return timeoutListener{ln}, nil
}

// timeoutListener sets userTimeout on each connection it accepts. Not every
// Linux lets a listening socket pass it on to them.
type timeoutListener struct{ net.Listener }

func (l timeoutListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		setUserTimeout(tc)
	}
	return c, err
}
