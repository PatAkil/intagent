package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/fsutil"
)

// A server that restarts closes its port for a moment: while it saves its
// board for the last time, and until the new process has restored the board
// and opens the port again. A hook refused then went ahead unchecked,
// although its agent would wait seconds more. So a hook whose connection is
// refused tries again, as long as its time allows. So does one whose
// connection was reset before any of its answer came back
// (client.ResetBeforeAnswer): as the server stops, its kernel resets the
// connections it had accepted and the server not yet, and the server closes
// those whose request was still arriving a second later, so none of them
// reached a handler.
//
// A retry can only turn an edit that would go ahead unchecked into one that
// is checked. Should the server have read and decided the request before
// the reset, which a server that stops does not do, the retry asks the
// board about the same edit again, and it answers as it stands: a
// reservation refuses it again, and a bump or a heads-up the first request
// spent is not given twice, so the edit goes ahead without it, as it would
// have gone ahead unchecked. Nothing else is tried again: a request that
// timed out has spent its time, and one whose answer was cut off once it
// had begun was decided.

// retryWaits are the pauses between tries: then the last, again and again.
var retryWaits = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

const (
	// minTryLeft is the least time worth another try: the connection, and an
	// answer from a server that has just started.
	minTryLeft = 500 * time.Millisecond
	// downAfter is how long refusals last before hooks on this machine take
	// the server for down, and try once, until it answers.
	downAfter = 10 * time.Second
)

// wsaeconnrefused is a refused connection on Windows, which package syscall
// does not name: its ECONNREFUSED there is a number of Go's own.
const wsaeconnrefused = syscall.Errno(10061)

// refused reports whether err is a connection the server's host refused,
// so that no part of the request reached it: what a server that restarts
// does while its port is closed. A dial that timed out is not, as its time
// is spent; nor is a name that did not resolve or a network that cannot be
// reached, as on a laptop away from the team's network, which a restart
// does not cause.
func refused(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaeconnrefused)
}

// tryAgain reports whether sendRetrying tries err again: a refused
// connection, or one reset before any of the answer came back.
func tryAgain(err error) bool { return refused(err) || client.ResetBeforeAnswer(err) }

// answered reports whether the server answered, whatever it said.
func answered(err error) bool {
	var ae *client.APIError
	return err == nil || errors.As(err, &ae)
}

// sendRetrying calls send, and again while its connection is refused or
// reset before an answer (tryAgain), until less than minTryLeft of ctx's
// time is left. A server that has refused or reset this machine's hooks for
// downAfter is taken for down, and tried once, so that every hook does not
// wait out its time while it is, until it answers one. stamp names the file
// that keeps when refusals began; "" keeps none.
func sendRetrying(ctx context.Context, stamp string, now func() time.Time, send func() error) error {
	for i := 0; ; i++ {
		err := send()
		if !tryAgain(err) {
			if stamp != "" && answered(err) {
				_ = os.Remove(stamp)
			}
			return err
		}
		if serverDown(stamp, now()) {
			return err
		}
		wait := retryWaits[min(i, len(retryWaits)-1)]
		if d, ok := ctx.Deadline(); ok && d.Sub(now()) < wait+minTryLeft {
			return err
		}
		if !pause(ctx, wait) {
			return err
		}
	}
}

// pause waits d, or less if ctx ends first, and reports whether it waited
// all of it. A variable for tests.
var pause = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// serverDown notes a refusal at now in stamp, and reports whether
// refusals have gone on for downAfter. The stamp holds when they began,
// and lasts until the server answers (sendRetrying removes it): a server
// that stays down costs each hook one try, however long it is between
// them.
func serverDown(stamp string, now time.Time) bool {
	if stamp == "" {
		return false
	}
	began := now
	if data, err := os.ReadFile(stamp); err == nil {
		if ns, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && ns <= now.UnixNano() {
			began = time.Unix(0, ns)
		}
	}
	if began.Equal(now) {
		// Whole or not at all: other hooks on this machine read it at once.
		_ = fsutil.WriteFile(stamp, []byte(strconv.FormatInt(now.UnixNano(), 10)), 0o600)
	}
	return now.Sub(began) >= downAfter
}

// downStamp is this machine's stamp of refusals by the server at url, in
// the user's cache directory, or "" if there is none.
func downStamp(url string) string {
	dir := cacheDir()
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(dir, "refused-"+hex.EncodeToString(sum[:8]))
}
