package server

import (
	"container/heap"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxConnections is how many connections a server keeps open at once
// unless told otherwise: far more than a team's hooks and dashboards hold,
// and far fewer than the file descriptors and memory that run out first.
const DefaultMaxConnections = 4096

// stuckWrite is how long a write must have waited for its client to read
// before its connection, though busy, may be closed to make room when none
// is waiting on its client: an answer to a hook fits in the socket's buffer
// at once, while one whose client stopped reading waits until its write
// deadline.
const stuckWrite = time.Second

// connTable keeps at most max connections open, bounding the file
// descriptors and memory clients can hold. Past that, a new connection takes
// the place of one that is waiting on its client, rather than being closed
// at once: a hook turned away fails open, so a cap anyone could fill with
// connections they hold, without a token, would let every edit through
// unchecked.
//
// A connection waits on its client from when it is accepted, or answered,
// until its next request has arrived whole, headers and body: it may be idle
// between requests, in its TLS handshake, or sending its request, however
// slowly. Closing it costs the server nothing. It is busy while the server
// works on a request of its that has arrived whole, and writes the answer;
// an HTTP/2 connection, which carries requests side by side, is busy while
// any such is being handled, whatever its other streams are sending.
//
// Room is made from the source (a client address, an IPv6 /64) with the
// most connections waiting, by closing the one that has waited longest: one
// client's idle or slow connections give way to each other, and to newer
// arrivals, before anyone else's do. A connection waits from when it began
// to, not from its last byte, so a client that trickles its request a byte
// at a time under the read timeouts gains nothing by it; but when the first
// bytes of a kept-alive connection's next request arrive, its wait starts
// again, so a client that reuses an idle connection is not cut off
// mid-request for having been idle. Behind a reverse proxy every connection
// is the proxy's, room is made from the longest waiting, and the proxy holds
// its own clients' idle and slow connections.
//
// When none is waiting, room is made by closing the connection whose write
// has waited longest, and at least stuckWrite, for its client to read; only
// when there is none of those either is the new connection closed at once.
// Busy connections are bounded by what the server does for them: streams by
// their caps, and answers by the write deadlines.
type connTable struct {
	max int
	log *slog.Logger
	// clock is a monotonic time in nanoseconds, never 0; tests replace it.
	clock func() int64

	mu      sync.Mutex
	open    []*limitConn // every open connection, by position (limitConn.pos)
	sources map[netip.Addr]*connSource
	// waiting holds the sources with connections waiting on their clients,
	// most waiting first, then the one whose longest has waited longest.
	waiting sourceHeap
	// What was done at the cap since the last log line, and when that was.
	madeRoom, turned int
	lastLog          time.Time
	logPeriod        time.Duration
}

func newConnTable(n int, log *slog.Logger) *connTable {
	start := time.Now()
	return &connTable{
		max: n, log: log, sources: map[netip.Addr]*connSource{}, logPeriod: time.Minute,
		clock: func() int64 { return int64(time.Since(start)) + 1 },
	}
}

// connSource is the connections of one client address.
type connSource struct {
	addr  netip.Addr
	conns int // open
	// waiting connections, the longest waiting first, linked through
	// limitConn.prev and next
	head, tail *limitConn
	waits      int
	at         int // in connTable.waiting; -1 when absent
}

// sourceOf is the source a connection from addr counts towards: its IP
// address, or for IPv6 its /64, which one client is commonly given whole.
func sourceOf(addr net.Addr) netip.Addr {
	ta, ok := addr.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}
	}
	a := ta.AddrPort().Addr().Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.Addr()
	}
	return a
}

// admit takes a new connection, making room for it if the table is full by
// closing the one returned as evicted. It returns nil when there is no room.
func (t *connTable) admit(c net.Conn) (lc, evicted *limitConn) {
	addr := sourceOf(c.RemoteAddr())
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.open) >= t.max {
		if evicted = t.victim(); evicted == nil {
			t.turned++
			t.logCap()
			return nil, nil
		}
		t.leave(evicted)
		t.madeRoom++
		t.logCap()
	}
	src := t.sources[addr]
	if src == nil {
		src = &connSource{addr: addr, at: -1}
		t.sources[addr] = src
	}
	src.conns++
	lc = &limitConn{Conn: c, t: t, src: src, pos: len(t.open)}
	lc.heard.Store(time.Now().UnixNano())
	t.open = append(t.open, lc)
	t.wait(lc)
	return lc, evicted
}

// victim is the connection to close to make room: of the source with the
// most waiting on their clients, the one that has waited longest; else the
// one whose write has waited longest, at least stuckWrite, on its client to
// read; else none. t.mu is held.
func (t *connTable) victim() *limitConn {
	if len(t.waiting) > 0 {
		return t.waiting[0].head
	}
	now := t.clock()
	var v *limitConn
	var oldest int64
	for _, c := range t.open {
		if w := c.writing.Load(); w != 0 && now-w >= int64(stuckWrite) && (v == nil || w < oldest) {
			v, oldest = c, w
		}
	}
	return v
}

// logCap logs, at most once a period, what the cap did. t.mu is held.
func (t *connTable) logCap() {
	if now := time.Now(); now.Sub(t.lastLog) >= t.logPeriod {
		t.log.Warn("too many connections: those waiting longest on their clients make room for new ones, "+
			"and new ones are closed at once when none is waiting",
			"open", t.max, "made_room", t.madeRoom, "closed", t.turned)
		t.madeRoom, t.turned, t.lastLog = 0, 0, now
	}
}

// wait marks c waiting on its client from now, last of its source's.
// t.mu is held.
func (t *connTable) wait(c *limitConn) {
	if c.pos < 0 {
		return
	}
	t.unwait(c)
	s := c.src
	c.since = t.clock()
	c.prev, c.next = s.tail, nil
	if s.tail != nil {
		s.tail.next = c
	} else {
		s.head = c
	}
	s.tail = c
	s.waits++
	if s.at < 0 {
		heap.Push(&t.waiting, s)
	} else {
		heap.Fix(&t.waiting, s.at)
	}
}

// unwait marks c no longer waiting on its client. t.mu is held.
func (t *connTable) unwait(c *limitConn) {
	if c.since == 0 {
		return
	}
	s := c.src
	if c.prev != nil {
		c.prev.next = c.next
	} else {
		s.head = c.next
	}
	if c.next != nil {
		c.next.prev = c.prev
	} else {
		s.tail = c.prev
	}
	c.prev, c.next, c.since = nil, nil, 0
	s.waits--
	if s.waits == 0 {
		heap.Remove(&t.waiting, s.at)
	} else {
		heap.Fix(&t.waiting, s.at)
	}
}

// leave takes c off the table: it was closed, or is about to be to make
// room. t.mu is held.
func (t *connTable) leave(c *limitConn) {
	t.unwait(c)
	last := t.open[len(t.open)-1]
	t.open[c.pos], last.pos = last, c.pos
	t.open[len(t.open)-1] = nil
	t.open = t.open[:len(t.open)-1]
	c.pos = -1
	if c.src.conns--; c.src.conns == 0 {
		delete(t.sources, c.src.addr)
	}
}

// request is one request on a connection, from when its handler starts.
type request struct {
	c *limitConn
	// Under t.mu: arrived is set once the request has arrived whole, and
	// over once its handler has returned.
	arrived, over bool
}

// arrive marks r arrived whole: its connection is busy until r is over,
// and the next bytes it reads are no new request's.
func (t *connTable) arrive(r *request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.arrived || r.over {
		return
	}
	r.arrived = true
	r.c.busy++
	r.c.idle.Store(false)
	t.unwait(r.c)
}

// finish marks r over: a connection with no request left that has arrived
// waits on its client again, from now.
func (t *connTable) finish(r *request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.over {
		return
	}
	r.over = true
	if r.arrived {
		// The answer goes out from here, and what the connection reads
		// after it is its next request.
		r.c.stage.CompareAndSwap(stageHandling, stageAnswered)
		if r.c.busy--; r.c.busy == 0 {
			t.wait(r.c)
		}
	}
}

// begun restarts the wait of c, which has been idle, as the first bytes of
// its next request arrive; a connection that is busy, as an HTTP/2 one may
// be while it reads other frames, is left busy.
func (t *connTable) begun(c *limitConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.busy == 0 && c.since != 0 {
		t.wait(c)
	}
}

// state is http.Server's ConnState: a connection that goes idle, answered,
// waits on its client again, from now.
func (t *connTable) state(c net.Conn, st http.ConnState) {
	if st != http.StateIdle {
		return
	}
	if lc := limited(c); lc != nil {
		lc.idle.Store(true)
		// Unless its next request has begun to arrive since it was
		// answered, as the server's read for a client that hangs up may
		// read it.
		if !lc.stage.CompareAndSwap(stageAnswered, stageIdle) {
			lc.stage.CompareAndSwap(stageHandling, stageIdle)
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if lc.busy == 0 {
			t.wait(lc)
		}
	}
}

type connKey struct{}

// context is http.Server's ConnContext: it gives each request its
// connection, for handler.
func (t *connTable) context(ctx context.Context, c net.Conn) context.Context {
	if lc := limited(c); lc != nil {
		return context.WithValue(ctx, connKey{}, lc)
	}
	return ctx
}

// handler marks a request's connection busy once the request has arrived
// whole, until its handler returns: at once for a request without a body,
// and for one with a body when the handler has read it, as many bytes as it
// said, or to its end. A request whose body never arrives, read by the
// handler or by the server after it, leaves its connection waiting on its
// client; so does an HTTP/2 connection whose only streams are such.
func (t *connTable) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lc, ok := r.Context().Value(connKey{}).(*limitConn)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// http.Server calls a handler only once it has taken the request,
		// past the check that drops one arriving as it stops: from here
		// Shutdown waits for the request, and it is answered.
		lc.stage.Store(stageHandling)
		req := &request{c: lc}
		defer t.finish(req)
		if r.ContentLength == 0 {
			t.arrive(req)
		} else {
			r.Body = &arrivingBody{ReadCloser: r.Body, t: t, r: req, left: r.ContentLength}
		}
		next.ServeHTTP(w, r)
	})
}

// arrivingBody is a request body that marks its request arrived once it has
// all arrived.
type arrivingBody struct {
	io.ReadCloser
	t    *connTable
	r    *request
	left int64 // bytes still to come, or -1 when the request did not say
	done bool
}

func (b *arrivingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.done {
		if b.left > 0 {
			b.left -= int64(n)
		}
		if b.left == 0 || err == io.EOF {
			b.done = true
			b.t.arrive(b.r)
		}
	}
	return n, err
}

// limited is the table's connection under c, which may be a TLS connection
// over it, or nil for a connection the table does not hold.
func limited(c net.Conn) *limitConn {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	lc, _ := c.(*limitConn)
	return lc
}

// sourceHeap orders sources by the connections they have waiting, most
// first, then by how long their longest has waited.
type sourceHeap []*connSource

func (h sourceHeap) Len() int { return len(h) }
func (h sourceHeap) Less(i, j int) bool {
	if h[i].waits != h[j].waits {
		return h[i].waits > h[j].waits
	}
	return h[i].head.since < h[j].head.since
}
func (h sourceHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].at, h[j].at = i, j
}
func (h *sourceHeap) Push(x any) {
	s := x.(*connSource)
	s.at = len(*h)
	*h = append(*h, s)
}
func (h *sourceHeap) Pop() any {
	old := *h
	s := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	s.at = -1
	return s
}

// limitListener accepts connections into a connTable, closing at once those
// it has no room for.
type limitListener struct {
	net.Listener
	t *connTable
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		lc, evicted := l.t.admit(c)
		if evicted != nil {
			_ = evicted.Close()
		}
		if lc != nil {
			return lc, nil
		}
		_ = c.Close()
	}
}

// A connection's stage, for a stop: how far its request has come.
const (
	// stageFresh: accepted, and nothing read from it yet.
	stageFresh int32 = iota
	// stageReceiving: the bytes of a request, or of a TLS handshake that
	// has not finished, have begun to arrive, and no handler has started
	// on it.
	stageReceiving
	// stageHandling: a handler has started on its request.
	stageHandling
	// stageAnswered: the handler of a request that arrived whole has
	// returned, and its answer goes out; what it reads next is the next
	// request.
	stageAnswered
	// stageIdle: answered, and nothing read of its next request.
	stageIdle
)

// limitConn is a connection the table holds. It leaves the table when it
// is closed, once. It notes when it last read something from its client,
// for freshConns, and when a write began that has not finished.
type limitConn struct {
	net.Conn
	t    *connTable
	src  *connSource
	once sync.Once
	// heard is when it was accepted or last read bytes, in Unix
	// nanoseconds.
	heard atomic.Int64
	// writing is when the write under way began, on t's clock, or 0.
	writing atomic.Int64
	// idle is set when it goes idle between requests; the first bytes of
	// its next request clear it, and start its wait again.
	idle atomic.Bool
	// stage is how far its request has come, for a stop (settle).
	stage atomic.Int32
	// multiplexed is set once its TLS handshake chose HTTP/2, which
	// carries requests side by side and stops on its own terms.
	multiplexed atomic.Bool

	// Under t.mu: since is when it began waiting on its client, on t's
	// clock, or 0 while it is not; prev and next link its source's
	// waiting connections; pos is its place in t.open, or -1 once it has
	// left the table; busy counts its requests that have arrived whole and
	// whose handlers have not returned.
	since      int64
	prev, next *limitConn
	pos        int
	busy       int
}

func (c *limitConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.heard.Store(time.Now().UnixNano())
		if c.idle.Load() && c.idle.CompareAndSwap(true, false) {
			c.t.begun(c)
		}
		if st := c.stage.Load(); st != stageReceiving && st != stageHandling {
			c.stage.CompareAndSwap(st, stageReceiving)
		}
	}
	return n, err
}

func (c *limitConn) Write(p []byte) (int, error) {
	c.writing.Store(c.t.clock())
	n, err := c.Conn.Write(p)
	c.writing.Store(0)
	return n, err
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.t.mu.Lock()
		defer c.t.mu.Unlock()
		if c.pos >= 0 {
			c.t.leave(c)
		}
	})
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
		lc := limited(tc)
		if tc.HandshakeContext(ctx) != nil || lc == nil {
			return
		}
		if tc.ConnectionState().NegotiatedProtocol == "h2" {
			lc.multiplexed.Store(true)
			return
		}
		// What it has read so far was its handshake, not a request: it is
		// fresh again, quiet since the handshake's last bytes, and a stop
		// takes it for unused once it has been quiet for unusedAfter, as it
		// would a plain connection. (Bytes of the request that came with
		// the handshake's last are counted with them: the rest of that
		// request comes within a round trip, unless its client stalls.)
		lc.stage.CompareAndSwap(stageReceiving, stageFresh)
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
// went unchecked. So once Shutdown begins, each is closed when it has sent
// nothing for quiet, unless its request has arrived by then. One whose
// request is under way, or whose TLS handshake is, is left to be answered:
// closed, its hook would go ahead unchecked. (Before Shutdown, the table's
// settle has given every such request time to arrive, and closed the
// connections of those that did not: from Go 1.25, Shutdown would drop
// them.)
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

// settleEvery is how often settle looks at the connections again.
const settleEvery = 5 * time.Millisecond

// settle waits, once the server has stopped accepting connections, until
// each HTTP/1 connection that may have a request under way has its handler
// started on it, or until within has passed or ctx is done, and then closes
// those that still have not. It returns how many it closed.
//
// From Go 1.25, http.Server drops a request that finishes arriving after
// Shutdown has begun, closing its connection without an answer: a hook
// mid-send at a restart got an EOF, and went ahead unchecked. So Serve
// settles its connections before it calls Shutdown, and Shutdown then waits
// for handlers alone. A connection may have a request under way when it has
// read something since it was accepted or last answered, or when it is fresh
// and has been quiet for less than quiet: a hook that has just connected
// sends its request at once, and one quiet for longer is taken for unused,
// as freshConns takes it. One idle between requests, with nothing read, is
// not: Shutdown closes it. Nor is an HTTP/2 connection, whose requests
// Shutdown lets finish, and whose new ones it turns away, so that their
// clients try again.
//
// Nothing new arrives meanwhile, so it ends as soon as the requests under
// way have arrived: within a round trip or two. A request still arriving at
// the deadline, from a client sending slowly or one that stopped halfway,
// would be dropped by Shutdown once it arrived, or hold the stop up until it
// did: its connection is closed instead.
func (t *connTable) settle(ctx context.Context, quiet, within time.Duration) int {
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	tick := time.NewTicker(settleEvery)
	defer tick.Stop()
	for len(t.underWay(quiet)) > 0 {
		select {
		case <-tick.C:
			continue
		case <-deadline.C:
		case <-ctx.Done():
		}
		left := t.underWay(quiet)
		for _, c := range left {
			_ = c.Close()
		}
		return len(left)
	}
	return 0
}

// underWay is the connections that may have a request under way whose
// handler has not started: see settle.
func (t *connTable) underWay(quiet time.Duration) []*limitConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	var under []*limitConn
	for _, c := range t.open {
		if c.multiplexed.Load() {
			continue
		}
		switch c.stage.Load() {
		case stageReceiving:
			under = append(under, c)
		case stageFresh:
			if quietFor(c) < quiet {
				under = append(under, c)
			}
		}
	}
	return under
}
