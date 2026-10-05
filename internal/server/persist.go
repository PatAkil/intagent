package server

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/fsutil"
)

// The board lives in memory, and is saved to the data directory in two
// parts. Most of what it holds its agents send it again: a worktree's changed
// files at its next scan, a session at its next hook, and what a session was
// told or a claim alerted of only keeps it from being told twice. What they
// cannot send again (the claims' intents, the notes in their inboxes and in
// members' mailboxes, the feed and the stats: board.Durable) is a small part
// of it, saved soon after it changes, in board.json.durable. The whole board
// is saved in board.json at most every bulkSaveEvery while it changes, and at
// a clean stop. So a crash loses up to bulkSaveEvery of files, liveness and
// what agents were told, which they send again or which at worst tells them
// something again, and about a second of intents and notes. Both files are
// compressed with gzip, and a restart restores the board from board.json and
// then the durable part, if it was saved after it.

const (
	// The whole board is saved at most every bulkSaveEvery while it changes.
	bulkSaveEvery = 5 * time.Minute
	// The durable part is saved within durableSoon of a change to an intent,
	// a note or the mail, and within durableEvery of any other change to it:
	// the feed, the stats and the seq, which every hook moves on.
	durableSoon  = time.Second
	durableEvery = 30 * time.Second
	// Either is saved no sooner than saveCost times its last save's duration
	// after that save started, so saving takes at most a tenth of the time;
	// the durable part no later than durableEvery all the same.
	saveCost = 9
	// minSaveEvery is how often the persister looks.
	minSaveEvery = time.Second
	// maxRetryEvery bounds the pause between saves that fail.
	maxRetryEvery = 30 * time.Second
	// failLogEvery is how often saves that keep failing are logged.
	failLogEvery = time.Minute
	// slowSave is how long a save may take before it is logged, once.
	slowSave = 30 * time.Second
	// aliveEvery is how often the server notes that it runs (alivePath).
	aliveEvery = 30 * time.Second
)

func (s *Server) snapshotPath() string { return filepath.Join(s.dataDir, "board.json") }

// previousPath keeps the snapshot before the last, should the last be
// damaged after it was written.
func (s *Server) previousPath() string { return s.snapshotPath() + ".prev" }

// durablePath keeps the durable part of the board, saved since the
// snapshot.
func (s *Server) durablePath() string { return s.snapshotPath() + ".durable" }

// alivePath keeps when the server last ran, on its board's clock: it notes
// so every aliveEvery and when it stops. A board that does not change is
// not saved again, so its snapshot says only when it last changed, while
// the server may have run on for hours since, hearing nothing from a
// stuck agent. The next start counts only the time since as downtime
// (board.RestoreAfter): all of it after a clean stop, and up to aliveEvery
// more after a crash.
func (s *Server) alivePath() string { return s.snapshotPath() + ".alive" }

// saves follows the server's saves of its board, or of its durable part:
// when the next is due, and whether they fail.
type saves struct {
	mu sync.Mutex
	// what names what is saved, in the log.
	what string
	// version is the board's version the last save held, and kept the
	// digest of the durable part it held (board.Durable.Kept).
	version uint64
	kept    uint64
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
	// noted is when the persister last noted that the server runs
	// (aliveIfDue); only it reads and writes it.
	noted time.Time
}

// SnapshotStatus says whether the server saves its board. /healthz reports
// it while the server has a data directory.
type SnapshotStatus struct {
	// OK is false while saves of the board, or of its durable part, fail:
	// changes are kept in memory only, and a restart will lose them.
	OK bool `json:"ok"`
	// SavedAt is when this process last saved the board, and
	// DurableSavedAt its durable part.
	SavedAt        time.Time `json:"saved_at,omitzero"`
	DurableSavedAt time.Time `json:"durable_saved_at,omitzero"`
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

// snapshotStatus is how the saves of the board and of its durable part go.
func (s *Server) snapshotStatus() *SnapshotStatus {
	st, d := s.saves.status(), s.durable.status()
	st.DurableSavedAt = d.SavedAt
	if st.OK && !d.OK {
		st.OK, st.FailingSince, st.Attempts, st.Error = false, d.FailingSince, d.Attempts, d.Error
	}
	return st
}

// saved is the board's version the last save held.
func (v *saves) saved() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.version
}

// wait is how long after now the next save is due, at least every after
// the last started: saveCost times the last one's duration after it
// started, up to most, if that is later. While saves fail, 1, 2, 4, 8 and
// 16 seconds after the last started, and then every maxRetryEvery.
func (v *saves) wait(now time.Time, every, most time.Duration) time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	every = max(every, min(most, saveCost*v.lastTook))
	if v.attempts > 0 {
		every = min(maxRetryEvery, max(minSaveEvery<<min(v.attempts-1, 5), min(most, saveCost*v.lastTook)))
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
			log.Warn(v.what+" is saved again", "failed_for", end.Sub(v.failing).Round(time.Second), "attempts", v.attempts)
		}
		v.version, v.savedAt, v.failing, v.attempts, v.cause = version, end, time.Time{}, 0, ""
		return
	}
	v.attempts++
	v.cause = causeOf(err)
	switch {
	case v.attempts == 1:
		v.failing, v.nextLog = end, end.Add(failLogEvery)
		log.Error(v.what+" could not be saved: changes are kept in memory only, and a restart will lose them", "err", err)
	case !end.Before(v.nextLog):
		v.nextLog = end.Add(failLogEvery)
		log.Error(v.what+" still cannot be saved", "since", v.failing, "attempts", v.attempts, "err", err)
	}
}

// keep notes the digest of the durable part the last save held.
func (v *saves) keep(kept uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.kept = kept
}

// held is the digest of the durable part the last save held, and when that
// save started.
func (v *saves) held() (uint64, time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.kept, v.lastStart
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
// decoding it as it is read, and then its durable part saved since
// (loadDurable). A damaged snapshot is set aside as
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
	if err := s.loadBoard(s.lastAlive()); err != nil {
		return err
	}
	return s.loadDurable()
}

// loadBoard restores the board from its snapshot, or the one saved before
// it, if either exists.
func (s *Server) loadBoard(stopped time.Time) error {
	path := s.snapshotPath()
	err := s.restoreFrom(path, stopped)
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
	switch perr := s.restoreFrom(s.previousPath(), stopped); {
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

// loadDurable restores the durable part of the board saved after its
// snapshot, if there is one. A damaged one is set aside as
// board.json.durable.corrupt-<unix time>: what it held of the changes to
// intents and notes since the snapshot is lost, which is logged. One that
// cannot be read, or of a newer format, stops the server.
func (s *Server) loadDurable() error {
	path := s.durablePath()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	applied, err := s.board.RestoreDurable(f)
	_ = f.Close()
	switch {
	case err == nil:
		if applied {
			s.log.Info("restored the intents and notes saved after the board's snapshot", "file", path)
		}
		s.durable.version = s.board.Version()
		return nil
	case errors.Is(err, board.ErrCorruptSnapshot):
		aside := fmt.Sprintf("%s.corrupt-%d", path, s.clock().Unix())
		if rerr := os.Rename(path, aside); rerr != nil {
			return fmt.Errorf("%s: %w, and it could not be set aside: %w", path, err, rerr)
		}
		s.log.Error("the snapshot of the board's intents and notes is damaged, and was set aside: what they changed after "+
			"the board's snapshot is lost", "file", aside, "err", err)
		return nil
	}
	return fmt.Errorf("%s: %w", path, err)
}

// restoreFrom restores the board from the snapshot at path, saved by a
// server that ran until stopped, if that is known.
func (s *Server) restoreFrom(path string, stopped time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return s.board.RestoreAfter(f, s.now(), stopped)
}

// lastAlive is when the server that last used the data directory last
// noted that it ran, or zero if it is not known.
func (s *Server) lastAlive() time.Time {
	data, err := os.ReadFile(s.alivePath())
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// markAlive notes in the data directory that the server runs, at its
// board's time now.
func (s *Server) markAlive() {
	stamp := s.now().UTC().Format(time.RFC3339Nano) + "\n"
	if err := fsutil.WriteFile(s.alivePath(), []byte(stamp), 0o600); err != nil {
		s.log.Debug("could not note that the server runs", "err", err)
	}
}

// aliveIfDue notes that the server runs if it has not for aliveEvery, and
// returns how long until it should again.
func (s *Server) aliveIfDue() time.Duration {
	now := s.clock()
	if wait := s.saves.noted.Add(aliveEvery).Sub(now); wait > 0 {
		return wait
	}
	s.markAlive()
	s.saves.noted = now
	return aliveEvery
}

// removeTemps removes the temporary files a save killed halfway left in the
// data directory, best effort.
func (s *Server) removeTemps() {
	for _, p := range []string{s.snapshotPath(), s.previousPath(), s.durablePath(), s.alivePath(), filepath.Join(s.dataDir, "ui.key")} {
		if n, err := fsutil.RemoveTemps(p); n > 0 || (err != nil && !errors.Is(err, fs.ErrNotExist)) {
			s.log.Info("removed the temporary files of saves that did not finish", "file", p, "removed", n, "err", err)
		}
	}
}

// save writes a snapshot if the board changed since the last one, and
// records how it went. The snapshot is streamed, compressed, to a temporary
// file beside the last, which it then replaces (fsutil.WriteFileFunc); the
// one it replaces is kept as board.json.prev. Once stop is closed, the save
// is abandoned at its next write; a nil stop never is.
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
		return zipped(w, func(w io.Writer) error {
			var err error
			version, err = s.board.WriteSnapshot(abandonable{stop: stop, w: w}, s.now())
			return err
		})
	})
	return version, err
}

// zipped has write write to w through gzip, at its fastest: a board's
// snapshot shrinks to a fifth or less.
func zipped(w io.Writer, write func(io.Writer) error) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	err = write(zw)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return err
}

// saveDurable writes the board's durable part if it changed since the last
// was saved, as kept digests it (board.Durable.Kept), or if anything else in
// it did and the last was saved durableEvery ago. It records how it went,
// and reports whether it wrote it.
func (s *Server) saveDurable(force bool) (bool, error) {
	if s.dataDir == "" || s.board.Version() == s.durable.saved() {
		return false, nil
	}
	start := s.clock()
	d := s.board.Durable(s.now())
	kept := d.Kept()
	if was, at := s.durable.held(); kept == was && start.Sub(at) < durableEvery && !force {
		return false, nil
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		s.durable.finished(start, s.clock(), d.Version(), err, s.log)
		return false, err
	}
	err := s.saveFile(s.durablePath(), 0o600, func(w io.Writer) error {
		return zipped(w, func(w io.Writer) error {
			_, err := d.WriteTo(w)
			return err
		})
	})
	s.durable.finished(start, s.clock(), d.Version(), err, s.log)
	if err == nil {
		s.durable.keep(kept)
	}
	return true, err
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

// persist saves the board while the server runs, as saves are due, and
// notes every aliveEvery that it runs. Once the server starts to stop, it
// abandons a save in flight and returns: Serve saves once more when the
// requests are done.
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
			t.Reset(min(s.saveIfDue(s.closing), s.aliveIfDue()))
		}
	}
}

// saveIfDue saves the board's durable part, and then the whole board, if
// it changed and a save is due, and returns how long until it should look
// again. The board's save is abandoned once stop is closed.
func (s *Server) saveIfDue(stop <-chan struct{}) time.Duration {
	return min(s.saveDurableIfDue(), s.saveBoardIfDue(stop))
}

func (s *Server) saveDurableIfDue() time.Duration {
	if wait := s.durable.wait(s.clock(), durableSoon, durableEvery); wait > 0 {
		return wait
	}
	if wrote, _ := s.saveDurable(false); !wrote { // logged and recorded
		return minSaveEvery
	}
	return max(minSaveEvery, s.durable.wait(s.clock(), durableSoon, durableEvery))
}

func (s *Server) saveBoardIfDue(stop <-chan struct{}) time.Duration {
	if wait := s.saves.wait(s.clock(), bulkSaveEvery, time.Hour); wait > 0 {
		return wait
	}
	if s.board.Version() == s.saves.saved() {
		return minSaveEvery
	}
	_ = s.save(stop) // logged and recorded
	return max(minSaveEvery, s.saves.wait(s.clock(), bulkSaveEvery, time.Hour))
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
