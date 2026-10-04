package server

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// A hook's client waits a short time for the answer and then lets its agent
// go ahead. The board must not decide an event for an agent that has gone
// (board.HookEvent.Late), so the server works out when that is.

// timeoutHeader carries how long a client waits for an answer, in
// milliseconds; internal/client sends it with every request.
const timeoutHeader = "X-Intagent-Timeout"

const (
	// lateMargin is kept back from a client's timeout, a quarter of it at
	// most, so that an answer still reaches the client in time.
	lateMargin = 300 * time.Millisecond
	// lateGuard: a request's context also ends when a proxy half-closes the
	// connection and still waits for the answer, so the server believes a
	// context that ended only once the request has waited this long.
	lateGuard = time.Second
	// maxTimeout bounds the timeout a client may give.
	maxTimeout = 10 * time.Minute
)

// waiter is a hook request's client, as far as the server can tell how long
// it waits for the answer.
type waiter struct {
	ctx     context.Context
	clock   func() time.Time
	arrived time.Time
	budget  time.Duration // 0: the client did not say
}

// waiterFor describes the client of a request that arrived at arrived.
func (s *Server) waiterFor(r *http.Request, arrived time.Time) *waiter {
	return &waiter{ctx: r.Context(), clock: s.clock, arrived: arrived, budget: budgetOf(r.Header.Get(timeoutHeader))}
}

// budgetOf is how long the server may take to answer a client that waits
// the header's milliseconds: 0 if the header is missing or invalid.
func budgetOf(header string) time.Duration {
	ms, err := strconv.ParseInt(header, 10, 64)
	if err != nil || ms <= 0 || ms > maxTimeout.Milliseconds() {
		return 0
	}
	t := time.Duration(ms) * time.Millisecond
	return t - min(lateMargin, t/4)
}

// waited is how long the request has been in the server.
func (w *waiter) waited() time.Duration { return w.clock().Sub(w.arrived) }

// late reports whether the client has stopped waiting, or will have by the
// time an answer reaches it. A client that gave no timeout is late only
// once its connection has closed, after lateGuard.
func (w *waiter) late() bool {
	waited := w.waited()
	if w.budget > 0 && waited > w.budget {
		return true
	}
	return waited >= lateGuard && w.ctx.Err() != nil
}
