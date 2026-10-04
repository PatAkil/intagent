package server

import (
	"bytes"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// Large request bodies are metered before they are read. A session start's or
// a heartbeat's footprint can be hundreds of kilobytes, and decoding a
// megabyte costs up to 35 ms of CPU and 18 MB of garbage; the hooks that
// check edits send a few hundred bytes and are never metered, so that no
// edit is refused, under INTAGENT_FAIL=closed, for load.
const (
	largeBody       = 16 << 10
	memberByteRate  = 4 << 20  // bytes a second, per member
	memberByteBurst = 32 << 20 // a fleet's session starts at once, under one token
	largeBodyWait   = time.Second
)

// bucket is a token bucket that refills with the server's clock.
type bucket struct {
	tokens float64
	at     time.Time
}

// take refills b for the time since it was last used, up to burst, and takes
// n tokens if it holds them. A clock that went back refills nothing.
func (b *bucket) take(now time.Time, n, rate, burst float64) bool {
	if d := now.Sub(b.at); d > 0 {
		b.tokens = min(burst, b.tokens+d.Seconds()*rate)
		b.at = now
	}
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// admission decides whether the server takes on a request's work.
type admission struct {
	mu sync.Mutex
	// bytes holds each member's budget for large bodies: one per member in
	// the team file.
	bytes             map[string]*bucket
	byteRate, byteCap float64
	// large holds a slot for each large body being decoded and handled, so
	// that their memory and CPU are bounded whoever sends them.
	large     chan struct{}
	largeWait time.Duration
}

func newAdmission() *admission {
	return &admission{
		bytes: map[string]*bucket{}, byteRate: memberByteRate, byteCap: memberByteBurst,
		large: make(chan struct{}, runtime.GOMAXPROCS(0)), largeWait: largeBodyWait,
	}
}

func (a *admission) takeBytes(member string, n int64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.bytes[member]
	if !ok {
		b = &bucket{tokens: a.byteCap, at: now}
		a.bytes[member] = b
	}
	return b.take(now, float64(n), a.byteRate, a.byteCap)
}

// admitBody meters a large request body. It is charged to the member's byte
// budget from its Content-Length before any of it is read (a body of unknown
// length is charged the most it may be), then read whole, and then decoded
// and handled once one of the large-body slots is free: a body sent slowly
// holds a connection, not a slot. When it does not admit the request, it
// answers it, and the hook fails open.
func (s *Server) admitBody(w http.ResponseWriter, r *http.Request, member string) (release func(), ok bool) {
	n := r.ContentLength
	if n >= 0 && n <= largeBody {
		return func() {}, true
	}
	if n < 0 || n > maxBody {
		n = maxBody
	}
	if !s.admit.takeBytes(member, n, s.now()) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "this member is sending more data than the server takes at once; try again in a second")
		return nil, false
	}
	buf := bytes.NewBuffer(make([]byte, 0, n+bytes.MinRead))
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxBody)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	r.Body = io.NopCloser(buf)
	t := time.NewTimer(s.admit.largeWait)
	defer t.Stop()
	select {
	case s.admit.large <- struct{}{}:
		return func() { <-s.admit.large }, true
	case <-t.C:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "the server is busy with other large requests; try again in a second")
	case <-r.Context().Done():
	}
	return nil, false
}
