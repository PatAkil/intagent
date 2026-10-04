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

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/fsutil"
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

// previousPath keeps the snapshot before the last, should the last be
// damaged after it was written.
func (s *Server) previousPath() string { return s.snapshotPath() + ".prev" }

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

// load restores the board from the data directory, if a snapshot exists,
// decoding it as it is read. A damaged snapshot is set aside as
// board.json.corrupt-<unix time>, and the one saved before it restored, or
// none: every agent fails open while the server does not start. Any other
// error stops the server, so that it does not overwrite a snapshot it
// could not read: one of a newer format, say.
//
// The snapshot saved before takes the damaged one's place before it is
// restored: a server stopped before its next save, or one killed while it
// restores, restores it again at its next start rather than none, and
// one that cannot read it stops again rather than start empty.
func (s *Server) load() error {
	if s.dataDir == "" {
		return nil
	}
	s.removeTemps()
	path := s.snapshotPath()
	err := s.restoreFrom(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case errors.Is(err, board.ErrCorruptSnapshot):
	case err != nil:
		return fmt.Errorf("%s: %w", path, err)
	default:
		s.saves.version = s.board.Version()
		return nil
	}
	aside := fmt.Sprintf("%s.corrupt-%d", path, s.clock().Unix())
	if rerr := os.Rename(path, aside); rerr != nil {
		return fmt.Errorf("%s: %w, and it could not be set aside: %w", path, err, rerr)
	}
	s.log.Error("the board's snapshot is damaged, and was set aside", "file", aside, "err", err)
	if lerr := fsutil.LinkAside(s.previousPath(), path); lerr != nil {
		s.log.Warn("the snapshot saved before the damaged one could not take its place", "err", lerr)
	}
	switch perr := s.restoreFrom(s.previousPath()); {
	case perr == nil:
		s.log.Warn("restored the snapshot saved before the damaged one: what changed after it is lost", "file", s.previousPath())
		// Not the version saved, which may be gone: the next save writes
		// the board again.
		s.saves.version = s.board.Version() - 1
	case errors.Is(perr, fs.ErrNotExist) || errors.Is(perr, board.ErrCorruptSnapshot):
		// Damaged too: the next start finds no snapshot, as this one
		// restores none, rather than set it aside again.
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w, and it could not be removed: %w", path, perr, rerr)
		}
		s.log.Error("starting with an empty board: no earlier snapshot could be restored", "err", perr)
	default:
		return fmt.Errorf("%s: %w", s.previousPath(), perr)
	}
	return nil
}

// restoreFrom restores the board from the snapshot at path.
func (s *Server) restoreFrom(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return s.board.Restore(f, s.now())
}

// removeTemps removes the temporary files a save killed halfway left in the
// data directory, best effort.
func (s *Server) removeTemps() {
	for _, p := range []string{s.snapshotPath(), s.previousPath(), filepath.Join(s.dataDir, "ui.key")} {
		if n, err := fsutil.RemoveTemps(p); n > 0 || (err != nil && !errors.Is(err, fs.ErrNotExist)) {
			s.log.Info("removed the temporary files of saves that did not finish", "file", p, "removed", n, "err", err)
		}
	}
}

// save writes a snapshot if the board changed since the last one, and
// records how it went. The snapshot is streamed to a temporary file beside
// the last, which it then replaces (fsutil.WriteFileFunc); the one it
// replaces is kept as board.json.prev. Once stop is closed, the save is
// abandoned at its next write; a nil stop never is.
func (s *Server) save(stop <-chan struct{}) error {
	if s.dataDir == "" || s.board.Version() == s.saves.saved() {
		return nil
	}
	start := s.clock()
	slow := time.AfterFunc(s.slowSave, func() {
		s.log.Warn("a snapshot of the board has been saving for a long time: the disk may be slow or stuck", "for", s.slowSave)
	})
	version, err := s.writeSnapshot(stop)
	slow.Stop()
	if errors.Is(err, errAbandoned) {
		s.log.Debug("snapshot abandoned: the server is stopping, and saves once more")
		return err
	}
	s.saves.finished(start, s.clock(), version, err, s.log)
	return err
}

func (s *Server) writeSnapshot(stop <-chan struct{}) (uint64, error) {
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return 0, err
	}
	if err := fsutil.LinkAside(s.snapshotPath(), s.previousPath()); err != nil {
		s.log.Debug("the previous snapshot could not be kept", "err", err)
	}
	var version uint64
	err := s.saveFile(s.snapshotPath(), 0o600, func(w io.Writer) error {
		var err error
		version, err = s.board.WriteSnapshot(abandonable{stop: stop, w: w}, s.now())
		return err
	})
	return version, err
}

// errAbandoned ends a save the server gave up for its final one.
var errAbandoned = errors.New("snapshot abandoned: the server is stopping")

// abandonable fails every write once stop is closed.
type abandonable struct {
	stop <-chan struct{}
	w    io.Writer
}

func (a abandonable) Write(p []byte) (int, error) {
	select {
	case <-a.stop:
		return 0, errAbandoned
	default:
		return a.w.Write(p)
	}
}

// persist saves the board while the server runs, as saves are due. Once
// the server starts to stop, it abandons a save in flight and returns:
// Serve saves once more when the requests are done.
func (s *Server) persist(ctx context.Context) {
	if s.dataDir == "" {
		return
	}
	t := time.NewTimer(minSaveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closing:
			return
		case <-t.C:
			t.Reset(s.saveIfDue(s.closing))
		}
	}
}

// saveIfDue saves the board if it changed and a save is due, and returns
// how long until it should look again. The save is abandoned once stop is
// closed.
func (s *Server) saveIfDue(stop <-chan struct{}) time.Duration {
	if wait := s.saves.wait(s.clock()); wait > 0 {
		return wait
	}
	if s.board.Version() == s.saves.saved() {
		return minSaveEvery
	}
	_ = s.save(stop) // logged and recorded
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
