package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/fsutil"
)

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

// Saves are spaced by what they cost: nine times as long as the last took,
// within 1 and 30 seconds of its start. One a second, as before, a board
// whose saves take half a second spent a third of its time saving, and
// one whose saves take five seconds was saved without a pause.
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
			saves++
			clock.add(c.took)
			return fsutil.WriteFileFunc(path, perm, fill)
		}
		end := clock.read().Add(time.Minute)
		for n := 0; clock.read().Before(end); n++ {
			ts.change(t, n)
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
	saves := 0
	ts.saveFile = func(path string, perm fs.FileMode, fill func(io.Writer) error) error {
		saves++
		return fsutil.WriteFileFunc(path, perm, fill)
	}
	ts.change(t, 0)
	for range 10 {
		clock.add(ts.saveIfDue(nil))
	}
	if saves != 1 {
		t.Fatalf("%d saves of a board changed once", saves)
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
		attempts = append(attempts, clock.read())
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
	stale := []string{".board.json.ABCDEFGHIJKL.tmp", ".board.json.prev.MNOPQRSTUVWX.tmp", ".ui.key.YZ234567ABCD.tmp"}
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
