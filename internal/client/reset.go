package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"syscall"
)

// ResetError is the error of a request whose connection was reset before
// any byte of an answer came back: what a server that stops does to the
// connections its kernel had accepted and the server had not yet, which
// never reached it. The caller may send the request again
// (internal/cli/retry.go): the hook does, as it does a refused one.
type ResetError struct {
	Err error
}

func (e *ResetError) Error() string { return e.Err.Error() }

func (e *ResetError) Unwrap() error { return e.Err }

// ResetBeforeAnswer reports whether err is a ResetError: the request's
// connection was reset before any of the answer came back. A request that
// timed out, or whose connection was closed or reset once its answer had
// begun, is not one.
func ResetBeforeAnswer(err error) bool {
	var re *ResetError
	return errors.As(err, &re)
}

// Windows' resets, which package syscall does not name: its ECONNRESET
// there is a number of Go's own.
const wsaeconnreset = syscall.Errno(10054)

// isReset reports whether err is a connection reset by its peer, or a write
// that met one (a broken pipe).
func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, wsaeconnreset)
}

// resetConn is a connection that notes whether it was reset. The HTTP
// transport writes a request while it reads for the answer, and when the
// reset meets the read first it closes the connection, so the write's
// error, which it returns, says only that the connection was closed.
type resetConn struct {
	net.Conn
	reset atomic.Bool
}

func (c *resetConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil && isReset(err) {
		c.reset.Store(true)
	}
	return n, err
}

func (c *resetConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil && isReset(err) {
		c.reset.Store(true)
	}
	return n, err
}

// transport is http.DefaultTransport's settings over connections that note
// a reset.
func transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &resetConn{Conn: c}, nil
	}
	return t
}

// resetWatch follows one request: the connection it went out on, and
// whether a byte of its answer has come back.
type resetWatch struct {
	conn     atomic.Pointer[resetConn]
	answered atomic.Bool
}

// trace has ctx's request report to w.
func (w *resetWatch) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			c := info.Conn
			if tc, ok := c.(*tls.Conn); ok {
				c = tc.NetConn()
			}
			if rc, ok := c.(*resetConn); ok {
				w.conn.Store(rc)
			}
		},
		GotFirstResponseByte: func() { w.answered.Store(true) },
	})
}

// mark makes err, the request's error, a ResetError if its connection was
// reset before any of the answer came back: in the dial or the TLS
// handshake, as the request was sent, or as its answer was awaited.
func (w *resetWatch) mark(err error) error {
	if w.answered.Load() {
		return err
	}
	if c := w.conn.Load(); isReset(err) || c != nil && c.reset.Load() {
		return &ResetError{Err: err}
	}
	return err
}
