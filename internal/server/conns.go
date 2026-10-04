package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxConnections is how many connections a server keeps open at once
// unless told otherwise: far more than a team's hooks and dashboards hold,
// and far fewer than the file descriptors and memory that run out first.
const DefaultMaxConnections = 4096

// limitListener keeps at most cap(slots) connections open. Past that it
// closes each new connection as soon as it is accepted: its client fails at
// once, and a hook fails open, instead of waiting in the kernel's queue until
// it gives up.
type limitListener struct {
	net.Listener
	slots chan struct{}
	log   *slog.Logger

	mu        sync.Mutex
	turned    int // connections turned away since the last log line
	lastLog   time.Time
	logPeriod time.Duration
}

func newLimitListener(ln net.Listener, n int, log *slog.Logger) *limitListener {
	return &limitListener{Listener: ln, slots: make(chan struct{}, n), log: log, logPeriod: time.Minute}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			lc := &limitConn{Conn: c, l: l}
			lc.heard.Store(time.Now().UnixNano())
			return lc, nil
		default:
			_ = c.Close()
			l.turnedAway()
		}
	}
}

// turnedAway logs, at most once a period, that connections are being closed.
func (l *limitListener) turnedAway() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turned++
	if now := time.Now(); now.Sub(l.lastLog) >= l.logPeriod {
		l.log.Warn("too many connections: new ones are closed at once", "open", cap(l.slots), "closed", l.turned)
		l.turned, l.lastLog = 0, now
	}
}

// limitConn gives its slot back when it is closed, once. It notes when it
// last read something from its client, for freshConns.
type limitConn struct {
	net.Conn
	l    *limitListener
	once sync.Once
	// heard is when it was accepted or last read bytes, in Unix
	// nanoseconds.
	heard atomic.Int64
}

func (c *limitConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.heard.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.l.slots })
	return err
}

// CloseWrite lets the HTTP server half-close a plain TCP connection, as it
// does before closing one whose request body it did not read.
func (c *limitConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// handshakeTimeout bounds a TLS handshake. A hook's client gives up after
// two seconds, and a handshake finished after that is work for nobody: with
// an RSA certificate, about 1.5 ms of CPU each.
const handshakeTimeout = 2 * time.Second

// tlsListener serves TLS on the connections it accepts, and closes those
// whose handshake has not finished within timeout of being accepted. The
// HTTP server bounds handshakes only by its read timeouts, which must leave
// time for a whole request.
type tlsListener struct {
	net.Listener
	config  *tls.Config
	timeout time.Duration
}

func (l *tlsListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := tls.Server(c, l.config)
	// The HTTP server starts the handshake as well; it waits for this one
	// and takes its outcome.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
		defer cancel()
		_ = tc.HandshakeContext(ctx)
	}()
	return tc, nil
}

// unusedAfter is how long a connection that has not sent a whole request
// must have sent nothing before the server, as it stops, takes it for
// unused. One whose client is sending its request, or is in a TLS
// handshake, has sent something within a round trip.
const unusedAfter = 500 * time.Millisecond

// freshConns follows the connections that have not sent a whole request
// yet. As it stops, http.Server waits for them as for busy ones, up to
// Serve's five seconds, and then fails: one browser's preconnect, or a
// client's spare connection, held every restart up that long while hooks
// went unchecked. So once the server starts to stop, each is closed when it
// has sent nothing for quiet, unless its request has arrived by then. One
// whose request is under way, or whose TLS handshake is, is left to be
// answered: closed, its hook would go ahead unchecked.
type freshConns struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	quiet time.Duration
	// done is set once the server starts to stop: a connection that turns
	// up after that, as the listener closes, is closed once quiet too.
	done bool
}

// track is http.Server's ConnState.
func (f *freshConns) track(c net.Conn, st http.ConnState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st != http.StateNew {
		delete(f.conns, c)
		return
	}
	f.conns[c] = struct{}{}
	if f.done {
		f.closeWhenQuiet(c)
	}
}

// closeAll closes the connections that have not sent a whole request once
// each has been quiet for f.quiet, when the server starts to stop.
func (f *freshConns) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = true
	for c := range f.conns {
		f.closeWhenQuiet(c)
	}
}

// closeWhenQuiet closes c if it has sent nothing for f.quiet, or looks again
// once it would have, if it is still fresh then. f.mu is held.
func (f *freshConns) closeWhenQuiet(c net.Conn) {
	wait := f.quiet - quietFor(c)
	if wait <= 0 {
		_ = c.Close()
		delete(f.conns, c)
		return
	}
	time.AfterFunc(wait, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.conns[c]; ok {
			f.closeWhenQuiet(c)
		}
	})
}

// quietFor is how long the server has read nothing from c: since it was
// accepted, or since the last bytes of its TLS handshake or request. A
// connection that is not the server's own is taken for quiet.
func quietFor(c net.Conn) time.Duration {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	lc, ok := c.(*limitConn)
	if !ok {
		return math.MaxInt64
	}
	return time.Since(time.Unix(0, lc.heard.Load()))
}
