package server

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// Large request bodies are metered before they are read. A session start's or
// a heartbeat's footprint can be hundreds of kilobytes, and decoding a
// megabyte costs up to 35 ms of CPU and 18 MB of garbage. Each member has a
// budget of bytes for them, and only a few are decoded at once.
//
// A pre_edit is never answered 429 or 503: its agent should hear that its
// edit was not checked. Most are a few hundred bytes and not metered at all.
// One that names a few hundred files, as a codemod's patch does, is over
// largeBody and draws on a budget of its own, which the member's footprints
// do not spend. When that budget or the wait for a slot runs out, the server
// answers the edit as one it got to too late: allow, marked unchecked, and
// counted towards the server's being degraded. A hook under
// INTAGENT_FAIL=closed refuses it, as it refuses any edit the server did not
// check; any other lets it through, notes it, and tells its agent later. A
// hook older than the mark lets it through, even under INTAGENT_FAIL=closed,
// and its agent reads why in the answer's context.
const (
	largeBody       = 16 << 10
	memberByteRate  = 4 << 20  // bytes a second, per member, for each budget
	memberByteBurst = 32 << 20 // a fleet's session starts at once, under one token
	largeBodyWait   = time.Second
)

// Checks and declarations are paced per member and worktree: one at a time,
// at callRate a second with a burst of callBurst. Each holds the board's lock
// for as long as its paths and patterns take, so an agent that loops on them
// is answered 429 rather than keep the lock from everyone's hooks; an
// orchestrator's agents, each in its own worktree, are paced apart. Hooks are
// never paced, since a fleet's agents share one token.
const (
	callRate  = 5
	callBurst = 20
)

// bucket is a token bucket that refills with the server's request clock
// (Server.clock), as other pacing does, not with the board's time.
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
	// bytes and edits hold each member's budgets for large bodies, the
	// second for pre_edits': one per member in the team file.
	bytes, edits      map[string]*bucket
	byteRate, byteCap float64
	// large holds a slot for each large body being decoded and handled, so
	// that their memory and CPU are bounded whoever sends them.
	large     chan struct{}
	largeWait time.Duration
	// calls and busy pace checks and declarations, by member and worktree.
	calls   map[string]*bucket
	busy    map[string]bool
	pruneAt int
	// shed counts, for the log, the large requests answered for load.
	shed loadLog
}

// loadLog counts the large requests the server answers for load rather than
// handle (a large edit let through unchecked, another large body turned
// away), and logs them at most once a period: a line for each would flood
// the log when the server is busiest. A line counts those since the last:
// the first is written at once, and the next once the period has passed,
// by the next of them or by the load watcher's tick (flush), whichever
// comes first, so that the end of a burst is not left out of the log.
type loadLog struct {
	mu                 sync.Mutex
	unchecked, refused int
	last               time.Time
	period             time.Duration
}

func (l *loadLog) note(now time.Time, unchecked bool, log *slog.Logger) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if unchecked {
		l.unchecked++
	} else {
		l.refused++
	}
	l.logIfDue(now, log)
}

// flush logs those counted since the last line, if the period has passed.
func (l *loadLog) flush(now time.Time, log *slog.Logger) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unchecked+l.refused > 0 {
		l.logIfDue(now, log)
	}
}

func (l *loadLog) logIfDue(now time.Time, log *slog.Logger) {
	if !l.last.IsZero() && now.Sub(l.last) < l.period {
		return
	}
	log.Warn("too busy for large requests: large edits let through unchecked, and other large bodies turned away",
		"unchecked", l.unchecked, "turned_away", l.refused)
	l.unchecked, l.refused, l.last = 0, 0, now
}

func newAdmission() *admission {
	return &admission{
		bytes: map[string]*bucket{}, edits: map[string]*bucket{}, byteRate: memberByteRate, byteCap: memberByteBurst,
		large: make(chan struct{}, runtime.GOMAXPROCS(0)), largeWait: largeBodyWait,
		calls: map[string]*bucket{}, busy: map[string]bool{}, pruneAt: minPrune, shed: loadLog{period: time.Minute},
	}
}

// takeBytes charges n bytes to one of member's budgets: its pre_edits' if
// edit is set, else the one for everything else.
func (a *admission) takeBytes(member string, n int64, now time.Time, edit bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	budgets := a.bytes
	if edit {
		budgets = a.edits
	}
	b, ok := budgets[member]
	if !ok {
		b = &bucket{tokens: a.byteCap, at: now}
		budgets[member] = b
	}
	return b.take(now, float64(n), a.byteRate, a.byteCap)
}

// admitBody meters a large request body. It is charged to the member's byte
// budget from its Content-Length before any of it is read (a body of unknown
// length is charged the most it may be), then read whole, and then decoded
// and handled once one of the large-body slots is free: a body sent slowly
// holds a connection, not a slot. When it does not admit the request, it
// answers it: 429 or 503, and the hook sends its event again without the
// footprint, which is small, or for a pre_edit an unchecked allow. It reports
// whether the body was admitted as a pre_edit's, which the handler must then
// find it is.
func (s *Server) admitBody(w http.ResponseWriter, r *http.Request, member string) (release func(), edit, ok bool) {
	n := r.ContentLength
	if n >= 0 && n <= largeBody {
		return func() {}, false, true
	}
	if n < 0 || n > maxBody {
		n = maxBody
	}
	edit = r.Pattern == hookRoute && startsPreEdit(r)
	if !s.admit.takeBytes(member, n, s.clock(), edit) {
		if edit {
			s.uncheckedEdit(w, r, member, "its member's budget for large edits is spent")
			return nil, edit, false
		}
		s.admit.shed.note(s.clock(), false, s.log)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "this member is sending more data than the server takes at once; try again in a second")
		return nil, edit, false
	}
	// The buffer grows as the body arrives: a client that says a megabyte
	// is coming and sends nothing must not hold a megabyte.
	buf := bytes.NewBuffer(make([]byte, 0, min(n, 64<<10)+bytes.MinRead))
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxBody)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, edit, false
	}
	r.Body = io.NopCloser(buf)
	t := time.NewTimer(s.admit.largeWait)
	defer t.Stop()
	select {
	case s.admit.large <- struct{}{}:
		return func() { <-s.admit.large }, edit, true
	case <-t.C:
		if edit {
			s.uncheckedEdit(w, r, member, "every large-body slot stayed busy")
			return nil, edit, false
		}
		s.admit.shed.note(s.clock(), false, s.log)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "the server is busy with other large requests; try again in a second")
	case <-r.Context().Done():
		if edit {
			// Its client went ahead, or refused the edit, without an answer.
			c := s.hookWaiter(r)
			s.answers.preEdits.add(c.arrived)
			s.gaveUp(c)
		}
	}
	return nil, edit, false
}

// preEditStart starts the body of every pre_edit intagent's clients send:
// encoding/json writes a struct's fields in order, and a HookEvent's first
// is its kind. A body that starts otherwise is metered as any other.
var preEditStart = []byte(`{"kind":"pre_edit"`)

// startsPreEdit reports whether r's body starts as a pre_edit's. It reads
// only that far, and leaves r.Body whole.
func startsPreEdit(r *http.Request) bool {
	br := bufio.NewReaderSize(r.Body, 64)
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	head, _ := br.Peek(len(preEditStart))
	return bytes.Equal(head, preEditStart)
}

// uncheckedEdit answers a pre_edit the server is too busy to check as it
// answers one it got to too late (board.HookResult.Unchecked), and counts it
// the same towards the server's being degraded. Its context says why, for
// clients older than the mark, which let the edit go ahead with the note.
func (s *Server) uncheckedEdit(w http.ResponseWriter, r *http.Request, member, why string) {
	c := s.hookWaiter(r)
	s.answers.preEdits.add(c.arrived)
	s.uncheckedCall(c)
	s.log.Debug("pre_edit let through unchecked", "member", member, "why", why)
	s.admit.shed.note(s.clock(), true, s.log)
	writeJSON(w, http.StatusOK, board.HookResult{Decision: board.DecisionAllow, Unchecked: true, Context: uncheckedEditNote})
}

const uncheckedEditNote = "[intagent] This edit goes ahead without a check: the team's intagent server is too busy " +
	"with large requests to compare it with teammates' work. A teammate may be working on these files. Check them " +
	"with the intagent check_paths tool, or tell your user."

// minPrune is how many paced worktrees are kept before refilled ones are
// dropped.
const minPrune = 1024

// call admits a check or a declaration with key, its member and worktree, or
// says why not.
func (a *admission) call(key string, now time.Time) (release func(), why string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy[key] {
		return nil, "another check or declaration from this worktree is still running; try again when it ends"
	}
	b, ok := a.calls[key]
	if !ok {
		a.prune(now)
		b = &bucket{tokens: callBurst, at: now}
		a.calls[key] = b
	}
	if !b.take(now, 1, callRate, callBurst) {
		return nil, "too many checks and declarations from this worktree; try again in a second"
	}
	a.busy[key] = true
	return func() {
		a.mu.Lock()
		delete(a.busy, key)
		a.mu.Unlock()
	}, ""
}

// prune drops the buckets that have refilled, which are as good as none, once
// there are many of them: their keys come from what clients send.
func (a *admission) prune(now time.Time) {
	if len(a.calls) < a.pruneAt {
		return
	}
	refill := time.Duration(float64(callBurst) / callRate * float64(time.Second))
	for k, b := range a.calls {
		if now.Sub(b.at) >= refill {
			delete(a.calls, k)
		}
	}
	a.pruneAt = max(minPrune, 2*len(a.calls))
}

// admitCall paces a check or a declaration from where, and answers it with
// 429 when it must wait.
func (s *Server) admitCall(w http.ResponseWriter, member string, where board.Where) (release func(), ok bool) {
	// Cleaned as the board cleans them, so the key names the claim's worktree.
	key := member + "\x00" + board.Clean(where.Host, 100) + "\x00" + board.Clean(where.Worktree, 500)
	release, why := s.admit.call(key, s.clock())
	if why != "" {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, why)
		return nil, false
	}
	return release, true
}
