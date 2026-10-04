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
	"time"

	"github.com/patakil/intagent/internal/fsutil"
)

// A server that restarts closes its port for a moment: while it saves its
// board for the last time, and until the new process has restored the board
// and opens the port again. A hook refused then went ahead unchecked,
// although its agent would wait seconds more. So a hook whose connection is
// refused tries again, as long as its time allows. Only a refused
// connection is tried again: a request that was sent may have been
// decided, and an answer it spent (a bump) must not be asked for twice.

// retryWaits are the pauses between tries: then the last, again and again.
var retryWaits = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

const (
	// minTryLeft is the least time worth another try: the connection, and an
	// answer from a server that has just started.
	minTryLeft = 500 * time.Millisecond
	// downAfter is how long refusals last before hooks on this machine take
	// the server for down, and try once; downForgotten is how long after the
	// last refusal that is forgotten.
	downAfter     = 10 * time.Second
	downForgotten = time.Minute
)

// refused reports whether err is a connection the server did not take, so
// that no part of the request reached it. A dial that timed out is not:
// its time is spent.
func refused(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	var t interface{ Timeout() bool }
	return !errors.As(err, &t) || !t.Timeout()
}

// sendRetrying calls send, and again while its connection is refused, until
// less than minTryLeft of ctx's time is left. A server that has refused this
// machine's hooks for downAfter is taken for down, and tried once, so that
// every hook does not wait out its time while it is. stamp names the file
// that keeps when refusals began; "" keeps none.
func sendRetrying(ctx context.Context, stamp string, now func() time.Time, send func() error) error {
	for i := 0; ; i++ {
		err := send()
		if !refused(err) {
			if stamp != "" && err == nil {
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
// refusals have gone on for downAfter. The stamp holds when they began, and
// its time is the last refusal's.
func serverDown(stamp string, now time.Time) bool {
	if stamp == "" {
		return false
	}
	began := now
	if fi, err := os.Stat(stamp); err == nil && now.Sub(fi.ModTime()) < downForgotten {
		if data, err := os.ReadFile(stamp); err == nil {
			if ns, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && ns <= now.UnixNano() {
				began = time.Unix(0, ns)
			}
		}
	}
	if began.Equal(now) {
		// Whole or not at all: other hooks on this machine read it at once.
		_ = fsutil.WriteFile(stamp, []byte(strconv.FormatInt(now.UnixNano(), 10)), 0o600)
	}
	_ = os.Chtimes(stamp, now, now)
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
