package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// A server too busy to answer in time lets every edit through unchecked while
// it looks well from outside: /healthz answers and the stream flows. So the
// server counts, per second over the last minute, the pre_edits that arrive
// and those whose agent went ahead without a checked answer, and says when
// too many do: in /healthz, on the dashboard and, if asked, by webhook.

const (
	// The server is degraded once more than 1 in 20 of the last
	// degradedWindow seconds' pre_edits went unchecked, and at least
	// minUnchecked of them. It recovers once it has been degraded for
	// recoveredWindow seconds and fewer than 1 in 100 went unchecked, both of
	// the last recoveredWindow seconds' pre_edits and of the last
	// degradedWindow seconds'. The first keeps traffic from before the
	// trouble out of the share; the second keeps a recovery from meeting the
	// test to degrade at once, as when traffic falls and the last few seconds'
	// unchecked edits, a small share of the last 30 seconds', are a large one
	// of the last 10.
	degradedWindow  = 10
	recoveredWindow = 30
	minUnchecked    = 3
	statusWindow    = 60
)

// perSecond counts events in each of the last 60 seconds, without a lock.
// A slot holds the second it counts, in its upper bits, and the count in its
// lower 32: the first event of a later second starts the slot over.
type perSecond [statusWindow]atomic.Int64

const countBits = 1<<32 - 1

// stamp is the part of a slot that names its second.
func stamp(sec int64) int64 { return (sec & (1<<31 - 1)) << 32 }

func (c *perSecond) add(t time.Time) {
	sec := t.Unix()
	if sec < 0 {
		return
	}
	slot, st := &c[sec%statusWindow], stamp(sec)
	for {
		old := slot.Load()
		next := st | 1
		if old&^countBits == st {
			next = old + 1
		}
		if slot.CompareAndSwap(old, next) {
			return
		}
	}
}

// sum counts the events in the n seconds up to and including t's.
func (c *perSecond) sum(t time.Time, n int) int64 {
	var total int64
	for sec := max(t.Unix()-int64(n)+1, 0); sec <= t.Unix(); sec++ {
		if v := c[sec%statusWindow].Load(); v&^countBits == stamp(sec) {
			total += v & countBits
		}
	}
	return total
}

// LoadStatus says whether agents get the server's answers in time.
type LoadStatus struct {
	// Degraded says that too many agents recently went ahead with edits the
	// server could not check in time.
	Degraded bool `json:"degraded"`
	// Since is when the server last became degraded or recovered.
	Since time.Time `json:"since,omitzero"`
	// PreEdits60s counts the edits agents asked about in the last minute,
	// and Unchecked60s those whose agent went ahead without an answer.
	PreEdits60s  int64 `json:"pre_edits_60s"`
	Unchecked60s int64 `json:"unchecked_60s"`
}

// answers follows whether pre_edits are answered in time.
type answers struct {
	preEdits, unchecked perSecond

	mu       sync.Mutex
	degraded bool
	since    time.Time
}

func newAnswers() *answers { return &answers{} }

// status reports the state and the last minute's counts at now.
func (l *answers) status(now time.Time) LoadStatus {
	l.mu.Lock()
	st := LoadStatus{Degraded: l.degraded, Since: l.since}
	l.mu.Unlock()
	st.PreEdits60s, st.Unchecked60s = l.preEdits.sum(now, statusWindow), l.unchecked.sum(now, statusWindow)
	return st
}

// tick moves the state on, once a second. It reports the new status, and
// how long the state it left had lasted, when the state changed.
func (l *answers) tick(now time.Time) (st LoadStatus, lasted time.Duration, changed bool) {
	l.mu.Lock()
	if l.degraded {
		changed = now.Sub(l.since) >= recoveredWindow*time.Second && l.few(now, recoveredWindow) && l.few(now, degradedWindow)
	} else {
		n, gone := l.preEdits.sum(now, degradedWindow), l.unchecked.sum(now, degradedWindow)
		changed = gone >= minUnchecked && gone*20 > n
	}
	if changed {
		if !l.since.IsZero() {
			lasted = now.Sub(l.since)
		}
		l.degraded, l.since = !l.degraded, now
	}
	l.mu.Unlock()
	if changed {
		st = l.status(now)
	}
	return st, lasted, changed
}

// few reports whether fewer than 1 in 100 of the last n seconds' pre_edits
// went unchecked, or none did.
func (l *answers) few(now time.Time, n int) bool {
	gone := l.unchecked.sum(now, n)
	return gone == 0 || gone*100 < l.preEdits.sum(now, n)
}

// watchCall counts a pre_edit, and counts it unchecked if its client gives
// up before the answer is written; the returned func ends the watch.
func (s *Server) watchCall(c *waiter) (stop func() bool) {
	s.answers.preEdits.add(c.arrived)
	return context.AfterFunc(c.ctx, func() { s.gaveUp(c) })
}

// gaveUp counts a pre_edit unchecked once its request's context has ended.
// Contexts that end sooner than a client gives up are not believed, as in
// waiter.late.
func (s *Server) gaveUp(c *waiter) {
	guard := lateGuard
	if c.budget > 0 {
		guard = min(guard, c.budget)
	}
	if c.waited() >= guard {
		s.uncheckedCall(c)
	}
}

// uncheckedCall counts, once, a pre_edit that went ahead unchecked.
func (s *Server) uncheckedCall(c *waiter) {
	if c.unchecked.CompareAndSwap(false, true) {
		s.answers.unchecked.add(s.clock())
	}
}

// watchLoad moves the load state on every second until ctx ends.
func (s *Server) watchLoad(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkLoad(s.clock())
		}
	}
}

// checkLoad logs and announces a change of state: to the dashboards, whose
// streams each take one encoding of it whatever their repository, and to the
// webhook.
func (s *Server) checkLoad(now time.Time) {
	st, lasted, changed := s.answers.tick(now)
	if !changed {
		return
	}
	if ev := statusEvent(st); ev != nil {
		s.hub.send([]frame{{all: true, data: ev}})
	}
	a := board.Activity{At: now, Kind: board.ActivityServerRecovered, Text: fmt.Sprintf("after %s", lasted.Round(time.Second))}
	if st.Degraded {
		s.log.Warn("agents' edits are going ahead unchecked: the server does not answer them in time",
			"unchecked_60s", st.Unchecked60s, "pre_edits_60s", st.PreEdits60s)
		a.Kind, a.Text = board.ActivityServerDegraded, fmt.Sprintf("%d of %d edits in the last minute went ahead unchecked", st.Unchecked60s, st.PreEdits60s)
	} else {
		s.log.Warn("agents' edits are answered in time again", "degraded_for", lasted.Round(time.Second))
	}
	if s.notifier != nil {
		s.notifier.enqueue([]board.Activity{a})
	}
}

// statusEvent is the load status as a server-sent "status" event, or nil if
// it cannot be encoded. It has no id: it is not an activity, and a
// reconnecting stream asks for none.
func statusEvent(st LoadStatus) []byte {
	data, err := json.Marshal(st)
	if err != nil {
		return nil
	}
	return appendEvent(nil, "status", 0, data)
}
