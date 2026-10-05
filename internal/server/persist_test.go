package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/fsutil"
)

// saveIfDue looks as the persister's two loops do (persist), the durable
// part's first, and returns how long until either should look again: tests
// drive the persister with it, on a clock they move. The whole board's save
// is abandoned once stop is closed.
func (s *Server) saveIfDue(stop <-chan struct{}) time.Duration {
	return min(s.saveDurableIfDue(), s.saveBoardIfDue(stop))
}

// handClock is the server's request clock in a test, moved by hand.
type handClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *handClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *handClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// persistServer is a test server with a data directory, a clock moved by
// hand for its saves, and its logs recorded.
func persistServer(t *testing.T, mutate ...func(*Options)) (*testServer, *handClock, *logRecorder) {
	t.Helper()
	logs := &logRecorder{}
	dir := t.TempDir()
	ts := newTestServer(t, append([]func(*Options){func(o *Options) {
		o.DataDir = dir
		o.Logger = slog.New(logs)
	}}, mutate...)...)
	clock := &handClock{now: time.Unix(1_790_000_000, 0)}
	ts.Server.clock = clock.read
	return ts, clock, logs
}

// change changes the board, as a hook does.
func (ts *testServer) change(t *testing.T, n int) {
	t.Helper()
	ev := hookEv(board.KindPostEdit, "alice", "a1", "x/"+strings.Repeat("y", n%7+1)+".go")
	ev.Member = "alice"
	if _, err := ts.Board().Hook(ts.now(), ev); err != nil {
		t.Fatal(err)
	}
}

// declare declares an intent for member, as the intents endpoint does.
func (ts *testServer) declare(t *testing.T, member, pattern string) {
	t.Helper()
	if _, err := ts.Board().Declare(ts.now(), board.DeclareRequest{Member: member, Where: board.Where{Repo: repo, Host: "h",
		Worktree: "/w/" + member}, Patterns: []string{pattern}, Mode: board.ModeExclusive}); err != nil {
		t.Fatal(err)
	}
}

// The whole board is saved at most every bulkSaveEvery while it changes,
// and its durable part within durableEvery of a change to what agents send
// again, and within a second of a change to an intent. Before, every change
// was saved a second or so later, the whole board each time: on boards the
// acceptance churn left, 20 to 37 GB an hour.
func TestSavesWriteWhatChanged(t *testing.T) {
	ts, clock, _ := persistServer(t)
	written := map[string][]time.Time{}
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		written[filepath.Base(path)] = append(written[filepath.Base(path)], clock.read())
		return fsutil.WriteFileFunc(path, perm, fill)
	}
	start := clock.read()
	var declared []time.Time
	for n := 0; clock.read().Before(start.Add(16 * time.Minute)); n++ {
		ts.change(t, n) // a file changed, at every look
		if n%120 == 60 {
			ts.declare(t, "alice", fmt.Sprintf("svc%d/**", n))
			declared = append(declared, clock.read())
		}
		clock.add(ts.saveIfDue(nil))
	}
	var whole []time.Duration
	for _, at := range written["board.json"] {
		whole = append(whole, at.Sub(start))
	}
	if !slices.Equal(whole, []time.Duration{0, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}) {
		t.Errorf("the whole board was saved at %v, want every 5 minutes", whole)
	}
	durable := written["board.json.durable"]
	if n := len(durable); n < 32 || n > 32+len(declared)+1 {
		t.Errorf("the durable part was saved %d times in 16 minutes, want every 30 seconds and after each declaration", n)
	}
	for _, at := range declared {
		i, _ := slices.BinarySearchFunc(durable, at, time.Time.Compare)
		if i == len(durable) || durable[i].Sub(at) > time.Second {
			t.Errorf("an intent declared %v in was not saved within a second", at.Sub(start))
		}
	}
}

// Saves of the durable part are spaced by what they cost: nine times as
// long as the last took, within 1 and 30 seconds of its start. One a second,
// a board whose saves take half a second would spend a third of its time
// saving, and one whose saves take five seconds would be saved without a
// pause.
func TestSavesArePacedByTheirCost(t *testing.T) {
	for _, c := range []struct {
		took     time.Duration
		min, max int
	}{
		{10 * time.Millisecond, 55, 60},  // every 1.01 s
		{500 * time.Millisecond, 14, 14}, // every 4.5 s
		{5 * time.Second, 2, 2},          // every 30 s
		{20 * time.Second, 2, 2},         // every 30 s, not 180
	} {
		ts, clock, _ := persistServer(t)
		saves := 0
		ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
			if filepath.Base(path) == "board.json.durable" {
				saves++
				clock.add(c.took)
			}
			return fsutil.WriteFileFunc(path, perm, fill)
		}
		end := clock.read().Add(time.Minute)
		for n := 0; clock.read().Before(end); n++ {
			ts.declare(t, "alice", fmt.Sprintf("svc%d/**", n))
			clock.add(ts.saveIfDue(nil))
		}
		if saves < c.min || saves > c.max {
			t.Errorf("saves taking %s: %d in a minute, want %d to %d", c.took, saves, c.min, c.max)
		}
	}
}

// A board that has not changed is not saved, however often it is asked.
func TestUnchangedBoardIsNotSaved(t *testing.T) {
	ts, clock, _ := persistServer(t)
	saves := map[string]int{}
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		saves[filepath.Base(path)]++
		return fsutil.WriteFileFunc(path, perm, fill)
	}
	ts.change(t, 0)
	for range 100 {
		clock.add(ts.saveIfDue(nil))
	}
	if saves["board.json"] != 1 || saves["board.json.durable"] != 1 || len(saves) != 2 {
		t.Fatalf("saves of a board changed once: %v", saves)
	}
}

// When saves fail, the server says so: in /healthz, which stays 200 and
// answers 503 with ?strict=1, without the data directory's path; in its log
// at the first failure, once a minute after, and when saves work again. It
// retries after 1, 2, 4, 8 and 16 seconds, then every 30, not every second.
func TestFailingSavesAreVisible(t *testing.T) {
	ts, clock, logs := persistServer(t)
	failing := true
	var attempts []time.Time
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		if filepath.Base(path) == "board.json" {
			attempts = append(attempts, clock.read())
		}
		if failing {
			return &fs.PathError{Op: "write", Path: "/srv/secret/board.json", Err: syscall.ENOSPC}
		}
		return fsutil.WriteFileFunc(path, perm, fill)
	}
	health := func(query string) (int, health) {
		t.Helper()
		var h health
		code := ts.do(t, http.MethodGet, "/healthz"+query, "", nil, &h)
		return code, h
	}
	if code, h := health("?strict=1"); code != http.StatusOK || h.Snapshot == nil || !h.Snapshot.OK {
		t.Fatalf("before any save: %d %+v", code, h.Snapshot)
	}
	start := clock.read()
	for n := 0; clock.read().Before(start.Add(5 * time.Minute)); n++ {
		ts.change(t, n)
		clock.add(ts.saveIfDue(nil))
	}
	var gaps []time.Duration
	for i := 1; i < len(attempts); i++ {
		gaps = append(gaps, attempts[i].Sub(attempts[i-1]))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if i >= len(gaps) || gaps[i] != w {
			t.Fatalf("retried after %v, want %v and then every 30s", gaps[:min(len(gaps), 10)], want)
		}
	}
	if first, again := logs.lines("the board's snapshot could not be saved"), logs.lines("the board's snapshot still cannot"); len(first) != 1 || len(again) != 4 {
		t.Fatalf("logged the first failure %d times and later ones %d times in 5 minutes, want 1 and 4", len(first), len(again))
	}
	code, h := health("")
	if code != http.StatusOK || h.Snapshot == nil || h.Snapshot.OK || h.Snapshot.Attempts != len(attempts) ||
		h.Snapshot.Error != "write: "+syscall.ENOSPC.Error() || !h.Snapshot.FailingSince.Equal(start) {
		t.Fatalf("failing: %d %+v", code, h.Snapshot)
	}
	if code, h := health("?strict=1"); code != http.StatusServiceUnavailable || h.OK {
		t.Fatalf("failing, strict: %d %+v", code, h)
	}
	failing = false
	clock.add(30 * time.Second)
	ts.saveIfDue(nil)
	if code, h := health("?strict=1"); code != http.StatusOK || !h.Snapshot.OK || h.Snapshot.Attempts != 0 || !h.Snapshot.SavedAt.Equal(clock.read()) {
		t.Fatalf("saved again: %d %+v", code, h.Snapshot)
	}
	if len(logs.lines("the board's snapshot is saved again")) != 1 {
		t.Fatalf("recovery not logged: %v", logs.lines(""))
	}
	if _, err := os.Stat(ts.snapshotPath()); err != nil {
		t.Fatal(err)
	}
}

// What /healthz says of a failed save names the operation and the system's
// error, never a path.
func TestSaveFailureCauseNamesNoPath(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&fs.PathError{Op: "open", Path: "/srv/x/.board.json.ABC.tmp", Err: syscall.EACCES}, "open: " + syscall.EACCES.Error()},
		{&os.LinkError{Op: "rename", Old: "/a", New: "/b", Err: syscall.EIO}, "rename: " + syscall.EIO.Error()},
		{errors.New("/srv/x links to /y, in a directory that cannot be written"), "the snapshot could not be written"},
	} {
		if got := causeOf(c.err); got != c.want {
			t.Errorf("causeOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A slow or stuck disk does not stop the sweeper: while a save hangs, a
// session that went quiet is still announced as stalled. When saves and
// sweeps shared a goroutine, it was not.
func TestSweepsWhileASaveHangs(t *testing.T) {
	ts, _, _ := persistServer(t, func(o *Options) { o.SweepEvery = 10 * time.Millisecond })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		once.Do(func() { close(entered) })
		<-release
		return fsutil.WriteFileFunc(path, perm, fill)
	}
	stop := serveTest(t, ts)
	defer func() { _ = stop() }()
	defer close(release)
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no save started")
	}
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Hour) // the board's clock: past the stall line
	ts.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, a := range ts.Board().View(ts.now(), repo).Recent {
			if a.Kind == board.ActivitySessionStalled {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no sweep announced the quiet session while a save hung")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serveTest runs the test server's Serve on a local port, and returns a stop
// that cancels it and returns what it returned.
func serveTest(t *testing.T, ts *testServer) (stop func() error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Serve(ctx, ln) }()
	ts.url = "http://" + ln.Addr().String()
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(20 * time.Second):
				result = errors.New("Serve did not return")
			}
		})
		return result
	}
}

// A save in flight when the server stops is abandoned at its next write,
// and the final save, once the requests are done, holds everything: here a
// claim made while the abandoned save was in flight.
func TestServeAbandonsASaveInFlight(t *testing.T) {
	ts, _, logs := persistServer(t)
	entered := make(chan struct{})
	var mu sync.Mutex
	var results []error
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		if filepath.Base(path) != "board.json" {
			return fsutil.WriteFileFunc(path, perm, fill)
		}
		mu.Lock()
		first := len(results) == 0
		mu.Unlock()
		if first {
			close(entered)
			<-ts.closing // until the server starts to stop
		}
		err := fsutil.WriteFileFunc(path, perm, fill)
		mu.Lock()
		results = append(results, err)
		mu.Unlock()
		return err
	}
	stop := serveTest(t, ts)
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no save started")
	}
	ts.do(t, http.MethodPost, "/v1/hook", "bob", hookEv(board.KindPrompt, "bob", "b1"), nil)
	if err := stop(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 2 || !errors.Is(results[0], errAbandoned) || results[1] != nil {
		t.Fatalf("saves: %v, want one abandoned and the final one", results)
	}
	restored := board.New(board.DefaultConfig())
	f, err := os.Open(ts.snapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := restored.Restore(f, ts.now()); err != nil {
		t.Fatal(err)
	}
	if v := restored.View(ts.now(), repo); len(v.Claims) != 2 {
		t.Fatalf("the final snapshot holds %d claims, want alice's and bob's", len(v.Claims))
	}
	if lines := logs.lines("the board's snapshot could not"); len(lines) != 0 {
		t.Fatalf("an abandoned save was logged as failed: %v", lines)
	}
	entries, _ := os.ReadDir(ts.dataDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// A clean stop that cannot write its final snapshot says so: Serve returns
// the error, and serve exits non-zero. It returned nil.
func TestServeReturnsTheFinalSnapshotsError(t *testing.T) {
	ts, _, _ := persistServer(t)
	ts.saveFile = func(string, fs.FileMode, func(io.Writer) error) error {
		return &fs.PathError{Op: "write", Path: "board.json", Err: syscall.ENOSPC}
	}
	stop := serveTest(t, ts)
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	err := stop()
	if !errors.Is(err, syscall.ENOSPC) || !strings.Contains(err.Error(), "final snapshot") {
		t.Fatalf("Serve = %v, want the final snapshot's error", err)
	}
}

// A save that takes long is logged once, while it is still in flight.
func TestSlowSaveIsLogged(t *testing.T) {
	ts, _, logs := persistServer(t)
	ts.slowSave = 20 * time.Millisecond
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		deadline := time.Now().Add(10 * time.Second)
		for len(logs.lines("a snapshot of the board has been saving")) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // past the threshold twice over
		return fsutil.WriteFileFunc(path, perm, fill)
	}
	ts.change(t, 0)
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(logs.lines("a snapshot of the board has been saving")); n != 1 {
		t.Fatalf("a slow save was logged %d times, want once", n)
	}
}

// Each save keeps the snapshot it replaces as board.json.prev.
func TestSaveKeepsThePreviousSnapshot(t *testing.T) {
	ts, clock, _ := persistServer(t)
	var saved [][]byte
	for n := range 3 {
		ts.change(t, n)
		clock.add(time.Minute)
		if err := ts.save(nil); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(ts.snapshotPath())
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, data)
	}
	if prev, err := os.ReadFile(ts.previousPath()); err != nil || !bytes.Equal(prev, saved[1]) {
		t.Fatalf("board.json.prev is not the snapshot before the last: %v", err)
	}
}

// A damaged snapshot is set aside, and the server starts from the one saved
// before it, or empty: before, it did not start at all, and every agent
// went unchecked until someone removed the file. A snapshot it cannot read,
// or of a newer format, still stops it, untouched.
func TestDamagedSnapshotIsSetAside(t *testing.T) {
	ts, clock, _ := persistServer(t)
	dir := ts.dataDir
	ts.change(t, 0)
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	ts.change(t, 1)
	clock.add(time.Minute)
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(ts.previousPath())
	if err != nil {
		t.Fatal(err)
	}
	restart := func() (*Server, *logRecorder, error) {
		logs := &logRecorder{}
		s, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}, DataDir: dir,
			Logger: slog.New(logs), Now: ts.now})
		return s, logs, err
	}
	damaged := []byte(`{"format":1,"claims":[{"id":"c_1","repo":`)
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setAside := func() []string {
		t.Helper()
		found, _ := filepath.Glob(filepath.Join(dir, "board.json.corrupt-*"))
		for _, f := range found {
			if data, _ := os.ReadFile(f); !bytes.Equal(data, damaged) {
				t.Errorf("%s does not hold the damaged snapshot", f)
			}
			_ = os.Remove(f)
		}
		return found
	}

	write("board.json", damaged)
	s, logs, err := restart()
	if err != nil {
		t.Fatalf("a damaged snapshot with a good previous one: %v", err)
	}
	want := board.New(board.DefaultConfig())
	if err := want.Restore(bytes.NewReader(prev), ts.now()); err != nil {
		t.Fatal(err)
	}
	if got, w := jsonView(s.Board().View(ts.now(), repo)), jsonView(want.View(ts.now(), repo)); got != w {
		t.Fatalf("restored\n%s\nwant the previous snapshot's\n%s", got, w)
	}
	if len(setAside()) != 1 || len(logs.lines("the board's snapshot is damaged")) != 1 {
		t.Fatalf("not set aside, or not logged: %v", logs.lines(""))
	}
	if s.saves.saved() == s.Board().Version() {
		t.Fatal("the board restored from the previous snapshot would not be saved")
	}

	write("board.json", damaged)
	if err := os.Remove(ts.previousPath()); err != nil {
		t.Fatal(err)
	}
	s, logs, err = restart()
	if err != nil || len(s.Board().View(ts.now(), repo).Claims) != 0 || len(setAside()) != 1 ||
		len(logs.lines("starting with an empty board")) != 1 {
		t.Fatalf("a damaged snapshot alone: %v, %v", err, logs.lines(""))
	}

	write("board.json", []byte(`{"format":2,"claims":[]}`))
	if _, _, err := restart(); err == nil || len(setAside()) != 0 {
		t.Fatalf("a newer snapshot: %v", err)
	}
	if err := os.Remove(ts.snapshotPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ts.snapshotPath(), 0o700); err != nil { // reads fail
		t.Fatal(err)
	}
	if _, _, err := restart(); err == nil || len(setAside()) != 0 {
		t.Fatalf("an unreadable snapshot: %v", err)
	}
}

// Once a damaged snapshot is set aside, the one saved before it takes its
// place: a server stopped before its next save (killed while it saved, say)
// restores it again, and one that cannot read it stops again. Before, the
// next start found no board.json and started empty without a word, and two
// saves later board.json.prev was overwritten too.
func TestPreviousSnapshotOutlivesTheNextStart(t *testing.T) {
	ts, clock, _ := persistServer(t)
	for n := range 2 {
		ts.change(t, n)
		clock.add(time.Minute)
		if err := ts.save(nil); err != nil {
			t.Fatal(err)
		}
	}
	restart := func() (*Server, error) {
		return New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}, DataDir: ts.dataDir,
			Logger: slog.New(&logRecorder{}), Now: ts.now})
	}
	damage := func(path string, data string) {
		t.Helper()
		if err := os.Remove(path); err != nil { // not through a link the file may share
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	damage(ts.snapshotPath(), `{"format":1,"claims":[{"id":`)
	for start := 1; start <= 2; start++ { // the second without a save between
		s, err := restart()
		if err != nil {
			t.Fatalf("start %d: %v", start, err)
		}
		if v := s.Board().View(ts.now(), repo); len(v.Claims) != 1 || len(v.Claims[0].Files) != 1 {
			t.Fatalf("start %d restored %d claims, want the previous snapshot's one, with one file", start, len(v.Claims))
		}
	}
	if found, _ := filepath.Glob(filepath.Join(ts.dataDir, "board.json.corrupt-*")); len(found) != 1 {
		t.Fatalf("set aside: %v, want the damaged snapshot once", found)
	}

	// One it cannot read stops every start, not just the first.
	damage(ts.snapshotPath(), `{"format":1,"claims":[{"id":`)
	damage(ts.previousPath(), `{"format":2,"claims":[]}`)
	for start := 1; start <= 2; start++ {
		if _, err := restart(); err == nil {
			t.Fatalf("start %d with a previous snapshot of a newer format began", start)
		}
	}

	// One damaged too is not left in board.json, to be set aside at every
	// start.
	damage(ts.snapshotPath(), `{"format":1,"claims":[{"id":`)
	damage(ts.previousPath(), `{"format":1,"sessions":[{"key":`)
	if s, err := restart(); err != nil || len(s.Board().View(ts.now(), repo).Claims) != 0 {
		t.Fatalf("both damaged: %v", err)
	}
	if _, err := os.Stat(ts.snapshotPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("board.json after both were found damaged: %v", err)
	}
}

// A restart counts as its agents' silence the time the server ran after
// its last save, and as downtime only the time it was down. A board that
// does not change is not saved again, and its snapshot says only when it
// last changed: here 09:00, after which the server ran on until 09:10,
// hearing nothing from alice's working agent. Down a minute, it took the
// eleven minutes since the snapshot for downtime, and at 09:11:30 counted
// the agent as silent for 30 seconds rather than ten and a half minutes,
// and announced no stall. The server notes that it runs when it stops,
// and every aliveEvery while it runs, which bounds what a crash credits
// too: here at 09:09.
func TestRestartCountsOnlyTheDowntime(t *testing.T) {
	ts, _, _ := persistServer(t)
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil) // working at 09:00
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	setBoardTime := func(at time.Time) {
		ts.mu.Lock()
		ts.clock = at
		ts.mu.Unlock()
	}
	nine := ts.now().Add(9 * time.Minute)
	setBoardTime(nine)
	stop := serveTest(t, ts)
	// A crash at 09:09: what the data directory holds once the server has
	// noted that it runs, at its first look, a second after it started.
	crash := t.TempDir()
	deadline := time.Now().Add(10 * time.Second)
	for !ts.lastAlive().Equal(nine) {
		if time.Now().After(deadline) {
			t.Fatal("a running server did not note that it runs")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, p := range []string{ts.snapshotPath(), ts.alivePath()} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(crash, filepath.Base(p)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A clean stop at 09:10, with nothing changed since 09:00.
	setBoardTime(nine.Add(time.Minute))
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, dir string
		restart   time.Time
	}{{"stopped at 09:10", ts.dataDir, nine.Add(2 * time.Minute)}, {"crashed at 09:09", crash, nine.Add(time.Minute)}} {
		s, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}, DataDir: c.dir,
			Logger: slog.New(&logRecorder{}), Now: func() time.Time { return c.restart }})
		if err != nil {
			t.Fatal(err)
		}
		at := nine.Add(150 * time.Second) // 09:11:30, silent ten and a half minutes of the server's time
		s.Board().Sweep(at)
		stalled := false
		for _, a := range s.Board().View(at, repo).Recent {
			stalled = stalled || a.Kind == board.ActivitySessionStalled
		}
		if !stalled {
			t.Errorf("%s and restarted a minute later: a1, last heard at 09:00, is not stalled at 09:11:30", c.name)
		}
	}
}

// The server notes that it runs every aliveEvery, not at each of the
// persister's looks, which come every second while the board is quiet.
func TestServerNotesThatItRunsEveryHalfMinute(t *testing.T) {
	ts, clock, _ := persistServer(t)
	at := func(d time.Duration) time.Time {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		ts.clock = ts.clock.Add(d)
		clock.add(d)
		return ts.clock
	}
	first := at(0)
	if wait := ts.aliveIfDue(); wait != aliveEvery || !ts.lastAlive().Equal(first) {
		t.Fatalf("first look: next in %s, noted %s; want %s and %s", wait, ts.lastAlive(), aliveEvery, first)
	}
	at(10 * time.Second)
	if wait := ts.aliveIfDue(); wait != 20*time.Second || !ts.lastAlive().Equal(first) {
		t.Fatalf("10 s later: next in %s, noted %s; want 20s and the first", wait, ts.lastAlive())
	}
	if later := at(20 * time.Second); ts.aliveIfDue() != aliveEvery || !ts.lastAlive().Equal(later) {
		t.Fatalf("30 s later: noted %s, want %s", ts.lastAlive(), later)
	}
}

func jsonView(v board.View) string {
	var b bytes.Buffer
	writeJSON(&recorder{body: &b, header: http.Header{}}, http.StatusOK, v)
	return b.String()
}

// recorder is the least of an http.ResponseWriter.
type recorder struct {
	body   *bytes.Buffer
	header http.Header
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *recorder) WriteHeader(int)             {}

// The temporary files of saves killed halfway are removed at start, and
// nothing else.
func TestStartRemovesStaleTemporaryFiles(t *testing.T) {
	ts, _, _ := persistServer(t)
	ts.change(t, 0)
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	dir := ts.dataDir
	stale := []string{".board.json.ABCDEFGHIJKL.tmp", ".board.json.prev.MNOPQRSTUVWX.tmp", ".board.json.durable.EFGHIJKLMNOP.tmp",
		".ui.key.YZ234567ABCD.tmp"}
	for _, f := range append(stale, "notes.txt") {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken("x")}}, DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range stale {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there", f)
		}
	}
	for _, f := range []string{"board.json", "ui.key", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// killed copies what the data directory of ts holds now into a new one, as
// a server killed now would leave it, and starts a server from it.
func (ts *testServer) killed(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(ts.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "board.json") && e.Type().IsRegular() {
			data, err := os.ReadFile(filepath.Join(ts.dataDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}, DataDir: dir,
		Logger: slog.New(&logRecorder{}), Now: ts.now})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A crash loses about a second of what agents cannot send again: an intent
// declared, and a note held for a member, a second before the server was
// killed are restored. What its agents send again, here a file changed after
// the last save of the whole board, may be lost, as the docs say.
func TestACrashKeepsAnIntentDeclaredASecondBefore(t *testing.T) {
	ts, clock, _ := persistServer(t)
	ts.change(t, 0)
	clock.add(ts.saveIfDue(nil)) // the whole board, at once
	for n := 1; n < 120; n++ {   // two minutes of hooks
		ts.change(t, n)
		clock.add(ts.saveIfDue(nil))
	}
	ts.declare(t, "alice", "svc/**")
	if res, err := ts.Board().Note(ts.now(), board.NoteRequest{Member: "alice", Where: board.Where{Repo: repo, Host: "h",
		Worktree: "/w/alice"}, To: "bob", Text: "svc is mine today", ToMember: true}); err != nil || res.HeldFor != "bob" {
		t.Fatalf("note to bob: %+v %v", res, err)
	}
	ev := hookEv(board.KindPostEdit, "alice", "a1", "x/late.go")
	ev.Member = "alice"
	if _, err := ts.Board().Hook(ts.now(), ev); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Second)
	ts.saveIfDue(nil)
	s := ts.killed(t)
	var intents []string
	var files []string
	for _, c := range s.Board().View(ts.now(), repo).Claims {
		for _, in := range c.Intents {
			intents = append(intents, c.Member+":"+in.Pattern)
		}
		for _, f := range c.Files {
			files = append(files, f.Path)
		}
	}
	if !slices.Equal(intents, []string{"alice:svc/**"}) {
		t.Errorf("after the crash the board holds intents %v, want alice's", intents)
	}
	if slices.Contains(files, "x/late.go") {
		t.Errorf("the file changed after the last save of the whole board was restored: %v", files)
	}
	res, err := s.Board().Hook(ts.now(), board.HookEvent{Kind: board.KindSessionStart, Member: "bob", Agent: board.AgentCodex,
		SessionID: "b1", Where: board.Where{Repo: repo, Host: "h", Worktree: "/w/bob"}})
	if err != nil || !strings.Contains(res.Context, "svc is mine today") {
		t.Errorf("bob's next session, after the crash, was told %q (%v)", res.Context, err)
	}
}

// The durable part is saved while the whole board is being written: a
// reservation made during a whole save, which takes a second or more on a
// large board and longer on a slow disk, survives a crash before that save
// ends. Here the whole save runs until the test ends, and the server is
// killed, its data directory copied and restored, until the reservation
// blocks bob there. Before, one goroutine saved both, the durable part
// waited for the whole save, and the reservation was lost: from a whole
// save's copy of the board until its rename and a second after, nothing of
// the board reached the disk.
func TestADurablePartIsSavedWhileTheWholeBoardIsWritten(t *testing.T) {
	ts, _, _ := persistServer(t)
	ts.Server.clock = time.Now // the persister's timers run on the wall clock
	writing, killed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		if filepath.Base(path) != "board.json" {
			return fsutil.WriteFileFunc(path, perm, fill)
		}
		once.Do(func() { close(writing) })
		<-killed // the whole save is never renamed in
		return errors.New("killed")
	}
	stop := serveTest(t, ts)
	defer func() { close(killed); _ = stop() }()
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	select {
	case <-writing:
	case <-time.After(10 * time.Second):
		t.Fatal("no whole save started")
	}
	if _, err := ts.Board().Declare(ts.now(), board.DeclareRequest{Member: "alice", Where: where("alice"),
		Patterns: []string{"svc/**"}, Mode: board.ModeExclusive}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		b := ts.killed(t).Board()
		var got []board.Decision
		for range 2 {
			ev := hookEv(board.KindPreEdit, "bob", "b1", "svc/a.go")
			ev.Member = "bob"
			res, err := b.Hook(ts.now(), ev)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, res.Decision)
		}
		if slices.Equal(got, []board.Decision{board.DecisionRefuse, board.DecisionRefuse}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("10 seconds into a whole save, a crash loses the reservation alice made during it: bob's edit "+
				"inside it, and his retry: %v", got)
		}
	}
}

// The persister saves the durable part without a clean stop: a server whose
// process is killed a moment after an intent is declared restores it.
func TestServeSavesIntentsWithoutAStop(t *testing.T) {
	ts, _, _ := persistServer(t)
	ts.Server.clock = time.Now
	stop := serveTest(t, ts)
	defer func() { _ = stop() }()
	ts.do(t, http.MethodPost, "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	ts.declare(t, "alice", "svc/**")
	deadline := time.Now().Add(20 * time.Second)
	for {
		s := ts.killed(t)
		if v := s.Board().View(ts.now(), repo); slices.ContainsFunc(v.Claims, func(c board.ClaimView) bool { return len(c.Intents) == 1 }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the intent was not saved")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The board's snapshot and durable part are saved compressed with gzip; a
// snapshot an older server saved, as plain JSON, still restores.
func TestSnapshotsAreCompressed(t *testing.T) {
	ts, clock, _ := persistServer(t)
	ts.change(t, 0)
	ts.declare(t, "alice", "svc/**")
	clock.add(ts.saveIfDue(nil))
	for _, name := range []string{"board.json", "board.json.durable"} {
		data, err := os.ReadFile(filepath.Join(ts.dataDir, name))
		if err != nil || len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
			t.Errorf("%s is not compressed (%v)", name, err)
		}
	}
	plain, _, err := ts.Board().Snapshot(ts.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"board.json.durable", "board.json"} {
		if err := os.Remove(filepath.Join(ts.dataDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ts.snapshotPath(), plain, 0o600); err != nil {
		t.Fatal(err)
	}
	if v := ts.killed(t).Board().View(ts.now(), repo); len(v.Claims) != 2 || len(v.Claims[0].Intents)+len(v.Claims[1].Intents) != 1 {
		t.Fatalf("the plain snapshot restored %d claims: %+v", len(v.Claims), v.Claims)
	}
}

// A damaged durable part is set aside, logged, and the server starts from
// the snapshot; one of a newer format stops it, untouched.
func TestDamagedDurablePartIsSetAside(t *testing.T) {
	ts, clock, _ := persistServer(t)
	ts.change(t, 0)
	clock.add(ts.saveIfDue(nil))
	start := func() (*Server, *logRecorder, error) {
		logs := &logRecorder{}
		s, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}, DataDir: ts.dataDir,
			Logger: slog.New(logs), Now: ts.now})
		return s, logs, err
	}
	if err := os.WriteFile(ts.durablePath(), []byte(`{"format":1,"claims":[{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, logs, err := start()
	if err != nil || len(s.Board().View(ts.now(), repo).Claims) != 1 || len(logs.lines("the snapshot of the board's intents and notes is damaged")) != 1 {
		t.Fatalf("a damaged durable part: %v, %v", err, logs.lines(""))
	}
	if found, _ := filepath.Glob(ts.durablePath() + ".corrupt-*"); len(found) != 1 {
		t.Fatalf("set aside: %v", found)
	}
	if err := os.WriteFile(ts.durablePath(), []byte(`{"format":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := start(); err == nil {
		t.Fatal("a durable part of a newer format did not stop the start")
	}
	if _, err := os.Stat(ts.durablePath()); err != nil {
		t.Fatalf("the newer durable part was moved: %v", err)
	}
}

// Saves that fail leave nothing behind in the data directory: each links the
// snapshot aside as board.json.prev before it writes, and while saves fail
// board.json.prev already is board.json. A temporary link was left at every
// attempt, each holding a whole snapshot's disk, until the next start.
func TestFailingSavesLeaveNothingBehind(t *testing.T) {
	ts, clock, _ := persistServer(t)
	ts.change(t, 0)
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	ts.saveFile = func(string, fs.FileMode, func(io.Writer) error) error {
		return &fs.PathError{Op: "write", Path: "board.json", Err: syscall.ENOSPC}
	}
	for n := 1; n <= 5; n++ {
		ts.change(t, n)
		clock.add(time.Minute)
		if err := ts.save(nil); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("save %d: %v", n, err)
		}
	}
	entries, err := os.ReadDir(ts.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}
