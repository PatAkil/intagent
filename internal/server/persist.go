package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// The board lives in memory, and a snapshot of it is saved to the data
// directory while it changes, so a restart loses at most what changed
// since the last save. A save costs time and memory in proportion to the
// board, so saves are spaced by what the last one cost: on a large board
// they come every few seconds, and an unclean stop loses that much. A clean
// stop always saves once more.

const (
	// A changed board is saved saveCost times the last save's duration after
	// that save started, so saving takes at most a tenth of the time; and
	// no sooner than minSaveEvery, nor later than maxSaveEvery.
	saveCost     = 9
	minSaveEvery = time.Second
	maxSaveEvery = 30 * time.Second
	// failLogEvery is how often saves that keep failing are logged.
	failLogEvery = time.Minute
	// slowSave is how long a save may take before it is logged, once.
	slowSave = 30 * time.Second
)

func (s *Server) snapshotPath() string { return filepath.Join(s.dataDir, "board.json") }

// saves follows the server's saves of its board: when the next is due, and
// whether they fail.
type saves struct {
	mu sync.Mutex
	// version is the board's version the last save held.
	version uint64
	// lastStart and lastTook pace saves; savedAt is when one last succeeded.
	lastStart time.Time
	lastTook  time.Duration
	savedAt   time.Time
	// failing is when saves started to fail, attempts how many have failed
	// since, cause why the last did, and nextLog when to log it again.
	failing  time.Time
	attempts int
	cause    string
	nextLog  time.Time
}

// SnapshotStatus says whether the server saves its board. /healthz reports
// it while the server has a data directory.
type SnapshotStatus struct {
	// OK is false while saves fail: changes are kept in memory only, and
	// a restart will lose them.
	OK bool `json:"ok"`
	// SavedAt is when this process last saved the board.
	SavedAt time.Time `json:"saved_at,omitzero"`
	// FailingSince, Attempts and Error say since when saves fail, how many
	// have, and why the last did, without the paths involved.
	FailingSince time.Time `json:"failing_since,omitzero"`
	Attempts     int       `json:"attempts,omitempty"`
	Error        string    `json:"error,omitempty"`
}

func (v *saves) status() *SnapshotStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	return &SnapshotStatus{OK: v.attempts == 0, SavedAt: v.savedAt, FailingSince: v.failing, Attempts: v.attempts, Error: v.cause}
}

// saved is the board's version the last save held.
func (v *saves) saved() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.version
}

// wait is how long after now a save of a changed board is due: saveCost
// times the last one's duration after it started, within minSaveEvery and
// maxSaveEvery; while saves fail, 1, 2, 4, 8 and 16 seconds after the last
// started, and then every maxSaveEvery.
func (v *saves) wait(now time.Time) time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	every := min(maxSaveEvery, max(minSaveEvery, saveCost*v.lastTook))
	if v.attempts > 0 {
		every = max(every, min(maxSaveEvery, minSaveEvery<<min(v.attempts-1, 5)))
	}
	return v.lastStart.Add(every).Sub(now)
}

// finished records a save that started at start and ended at end, having
// saved version or failed with err, and logs the first failure, one a
// minute while they go on, and the recovery.
func (v *saves) finished(start, end time.Time, version uint64, err error, log *slog.Logger) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastStart, v.lastTook = start, end.Sub(start)
	if err == nil {
		if v.attempts > 0 {
			log.Warn("the board's snapshot is saved again", "failed_for", end.Sub(v.failing).Round(time.Second), "attempts", v.attempts)
		}
		v.version, v.savedAt, v.failing, v.attempts, v.cause = version, end, time.Time{}, 0, ""
		return
	}
	v.attempts++
	v.cause = causeOf(err)
	switch {
	case v.attempts == 1:
		v.failing, v.nextLog = end, end.Add(failLogEvery)
		log.Error("the board's snapshot could not be saved: changes are kept in memory only, and a restart will lose them", "err", err)
	case !end.Before(v.nextLog):
		v.nextLog = end.Add(failLogEvery)
		log.Error("the board's snapshot still cannot be saved", "since", v.failing, "attempts", v.attempts, "err", err)
	}
}

// causeOf says why a save failed, as the operation and the system's error,
// without the paths an error from the file system names: /healthz may be
// open to anyone who can reach the server.
func causeOf(err error) string {
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var errno syscall.Errno
	switch {
	case errors.As(err, &pe):
		return pe.Op + ": " + causeOf(pe.Err)
	case errors.As(err, &le):
		return le.Op + ": " + causeOf(le.Err)
	case errors.As(err, &se):
		return se.Syscall + ": " + causeOf(se.Err)
	case errors.As(err, &errno):
		return errno.Error()
	}
	return "the snapshot could not be written"
}

// load restores the board from the data directory, if a snapshot exists.
// The snapshot is decoded as it is read, a claim at a time.
func (s *Server) load() error {
	if s.dataDir == "" {
		return nil
	}
	f, err := os.Open(s.snapshotPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := s.board.Restore(f); err != nil {
		return fmt.Errorf("%s: %w", s.snapshotPath(), err)
	}
	s.saves.version = s.board.Version()
	return nil
}

// save writes a snapshot if the board changed since the last one, and
// records how it went. The snapshot is streamed to a temporary file beside
// the last, which it then replaces (fsutil.WriteFileFunc). Once ctx ends,
// the save is abandoned at its next write.
func (s *Server) save(ctx context.Context) error {
	if s.dataDir == "" || s.board.Version() == s.saves.saved() {
		return nil
	}
	start := s.clock()
	slow := time.AfterFunc(s.slowSave, func() {
		s.log.Warn("a snapshot of the board has been saving for a long time: the disk may be slow or stuck", "for", s.slowSave)
	})
	version, err := s.writeSnapshot(ctx)
	slow.Stop()
	if err != nil && ctx.Err() != nil {
		s.log.Debug("snapshot abandoned: the server is stopping, and saves once more", "err", err)
		return err
	}
	s.saves.finished(start, s.clock(), version, err, s.log)
	return err
}

func (s *Server) writeSnapshot(ctx context.Context) (uint64, error) {
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return 0, err
	}
	var version uint64
	err := s.saveFile(s.snapshotPath(), 0o600, func(w io.Writer) error {
		var err error
		version, err = s.board.WriteSnapshot(abandonable{ctx: ctx, w: w}, s.now())
		return err
	})
	return version, err
}

// abandonable fails every write once ctx ends.
type abandonable struct {
	ctx context.Context
	w   io.Writer
}

func (a abandonable) Write(p []byte) (int, error) {
	if err := a.ctx.Err(); err != nil {
		return 0, err
	}
	return a.w.Write(p)
}

// persist saves the board while the server runs, as saves are due. Once
// the server starts to stop, it abandons a save in flight and returns:
// Serve saves once more when the requests are done.
func (s *Server) persist(ctx context.Context) {
	if s.dataDir == "" {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	t := time.NewTimer(minSaveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(s.saveIfDue(ctx))
		}
	}
}

// saveIfDue saves the board if it changed and a save is due, and returns
// how long until it should look again.
func (s *Server) saveIfDue(ctx context.Context) time.Duration {
	if wait := s.saves.wait(s.clock()); wait > 0 {
		return wait
	}
	if s.board.Version() == s.saves.saved() {
		return minSaveEvery
	}
	_ = s.save(ctx) // logged and recorded
	return max(minSaveEvery, s.saves.wait(s.clock()))
}

// sweep looks for stalled and gone sessions until the server starts to
// stop. It runs apart from saves, so that a slow or stuck disk does not
// hold up the news of a stalled agent.
func (s *Server) sweep(ctx context.Context) {
	t := time.NewTicker(s.sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closing:
			return
		case <-t.C:
			s.board.Sweep(s.now())
		}
	}
}
