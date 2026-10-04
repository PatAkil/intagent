package server

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// oldFrame is how the stream wrote an activity before frames were encoded
// once for every stream; the bytes must not change.
func oldFrame(a board.Activity) string {
	data, _ := json.Marshal(a)
	return fmt.Sprintf("id: %d\nevent: activity\ndata: %s\n\n", a.Seq, data)
}

// rawStream opens a stream as member and returns its reader once the server
// has subscribed it.
func (ts *testServer) rawStream(t *testing.T, member, query string, header http.Header) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.url+"/v1/stream"+query, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	if member != "" {
		req.Header.Set("Authorization", "Bearer "+ts.tokens[member])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream: %v %v", resp, err)
	}
	return bufio.NewReader(resp.Body), func() { cancel(); _ = resp.Body.Close() }
}

// readBlocks reads n blank-line-terminated blocks: events, comments, fields.
func readBlocks(t *testing.T, r *bufio.Reader, n int) string {
	t.Helper()
	var b strings.Builder
	for n > 0 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended with %d blocks to go after %q: %v", n, b.String(), err)
		}
		b.WriteString(line)
		if line == "\n" {
			n--
		}
	}
	return b.String()
}

var claimIDs = regexp.MustCompile(`"claim_id":"[^"]*"`)

// compareGolden compares got with a golden file, or rewrites the file with -update.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run the test with -update to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs; if the change is meant, read the diff after running with -update\ngot:\n%s\nwant:\n%s", p, got, want)
	}
}

// The bytes a stream sends are what every dashboard, old or new, parses:
// encoding each activity once for every stream must not change them.
func TestStreamBytes(t *testing.T) {
	ts := newTestServer(t)
	r, closeStream := ts.rawStream(t, "bob", "?repo="+repo, nil)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	live := readBlocks(t, r, 4) // connected; claim.opened, session.started, file.changed
	closeStream()

	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/z.go"), nil)
	r, closeStream = ts.rawStream(t, "bob", "?repo="+repo, http.Header{"Last-Event-Id": {"2"}})
	defer closeStream()
	replay := readBlocks(t, r, 3) // connected; what came after event 2

	acts := ts.Board().Since("", 0)
	if len(acts) != 4 {
		t.Fatalf("activities: %+v", acts)
	}
	prelude := "retry: 3000\n: connected\n\n"
	if want := prelude + oldFrame(acts[0]) + oldFrame(acts[1]) + oldFrame(acts[2]); live != want {
		t.Fatalf("live stream:\n%q\nwant\n%q", live, want)
	}
	if want := prelude + oldFrame(acts[2]) + oldFrame(acts[3]); replay != want {
		t.Fatalf("replay:\n%q\nwant\n%q", replay, want)
	}
	compareGolden(t, "stream.golden", claimIDs.ReplaceAllString(live+"--- reconnected with Last-Event-ID: 2\n"+replay, `"claim_id":"c-alice"`))
}

// One publish is one place in a stream's queue however many activities it
// holds: a sweep that finds a fleet of 1000 agents stalled must not end every
// dashboard's stream, as it did when each activity took a place of its own
// among 256.
func TestStreamTakesASweepWhole(t *testing.T) {
	ts := newTestServer(t)
	r, closeStream := ts.rawStream(t, "bob", "?repo="+repo, nil)
	defer closeStream()
	readBlocks(t, r, 1)
	var acts []board.Activity
	for i := range 1000 {
		acts = append(acts, board.Activity{Seq: uint64(i + 1), At: ts.now(), Kind: board.ActivitySessionStalled,
			Repo: repo, Member: "alice", Session: fmt.Sprintf("s%d", i), Text: "silent for 10m"})
	}
	ts.hub.publish(acts)
	evs := readEvents(t, r, 1000)
	if evs[0].Seq != 1 || evs[999].Seq != 1000 {
		t.Fatalf("events %d to %d", evs[0].Seq, evs[999].Seq)
	}
}

// A stream writes what has queued up at most every streamLinger, however fast
// activities come: one write and flush per activity is what made 300
// dashboards cost the server its CPU in a burst.
func TestStreamsLingerBetweenWrites(t *testing.T) {
	ts := newTestServer(t)
	w := newDiscardWriter()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/stream?repo="+repo, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ts.Handler().ServeHTTP(w, req) }()
	defer func() { cancel(); wg.Wait() }()
	for !ts.subscribed(1) {
		time.Sleep(time.Millisecond)
	}

	const n = 500
	var want int64
	start := time.Now()
	for i := range n {
		a := board.Activity{Seq: uint64(i + 1), At: ts.now(), Kind: board.ActivityFileChanged, Repo: repo, Member: "alice",
			Paths: []string{fmt.Sprintf("x/f%d.go", i)}}
		want += int64(len(oldFrame(a)))
		ts.hub.publish([]board.Activity{a})
		time.Sleep(2 * time.Millisecond)
	}
	prelude := int64(len("retry: 3000\n: connected\n\n"))
	for deadline := time.Now().Add(10 * time.Second); w.bytes.Load() < prelude+want; {
		if time.Now().After(deadline) {
			t.Fatalf("got %d bytes, want %d", w.bytes.Load(), prelude+want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	took := time.Since(start)
	// One flush per streamLinger at most, besides the first; one per activity
	// before. The bound leaves five times the room the measured count needs.
	flushes := w.flushes.Load()
	t.Logf("%d activities in %s: %d writes, %d flushes", n, took.Round(time.Millisecond), w.writes.Load(), flushes)
	if limit := int64(n / 5); flushes > limit {
		t.Fatalf("%d flushes for %d activities in %s; want at most %d", flushes, n, took, limit)
	}
}

// subscribed reports whether at least n streams are open.
func (ts *testServer) subscribed(n int) bool {
	ts.hub.mu.Lock()
	defer ts.hub.mu.Unlock()
	return len(ts.hub.subs) >= n
}

// A dashboard that stops reading must not hold its stream's handler in a
// write until TCP gives up, about 15 minutes: the write deadline releases it.
func TestStalledStreamIsReleased(t *testing.T) {
	defer func(d time.Duration) { streamWriteTimeout = d }(streamWriteTimeout)
	streamWriteTimeout = time.Second
	ts := newTestServer(t)
	hs := httptest.NewUnstartedServer(ts.Handler())
	hs.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		_ = c.(*net.TCPConn).SetWriteBuffer(8 << 10) // so the kernel cannot hold the whole stream
		return ctx
	}
	hs.Start()
	defer hs.Close()

	c, err := net.Dial("tcp", hs.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.(*net.TCPConn).SetReadBuffer(4 << 10)
	fmt.Fprintf(c, "GET /v1/stream?repo=%s HTTP/1.1\r\nHost: intagent\r\nAuthorization: Bearer %s\r\n\r\n", repo, ts.tokens["bob"])
	for !ts.subscribed(1) {
		time.Sleep(time.Millisecond)
	}

	// 4 MB of activities, which the client never reads.
	long := strings.Repeat("d", 90) + "/"
	var acts []board.Activity
	for i := range 200 {
		a := board.Activity{Seq: uint64(i + 1), At: ts.now(), Kind: board.ActivityFileChanged, Repo: repo, Member: "alice"}
		for j := range 200 {
			a.Paths = append(a.Paths, fmt.Sprintf("%s%d/%d.go", long, i, j))
		}
		acts = append(acts, a)
	}
	start := time.Now()
	ts.hub.publish(acts)
	for ts.subscribed(1) {
		if time.Since(start) > streamWriteTimeout+4*time.Second {
			t.Fatalf("the stream's handler is still writing to a client that reads nothing, %s after the deadline", time.Since(start)-streamWriteTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("released %s after the activities were published, with a %s deadline", time.Since(start).Round(time.Millisecond), streamWriteTimeout)
}

// openStream opens a stream as member ("" for none) and returns the response,
// whatever its status.
func (ts *testServer) openStream(t *testing.T, member string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/stream", nil)
	if member != "" {
		req.Header.Set("Authorization", "Bearer "+ts.tokens[member])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// streamsOpen counts the streams whose handler has not returned.
func (ts *testServer) streamsOpen() int {
	ts.hub.mu.Lock()
	defer ts.hub.mu.Unlock()
	return ts.hub.total
}

// One token, or anyone on a public board, must not be able to open streams
// without bound: 5000 from one token took 1.65 GB and let 62-99% of the
// team's edits through unchecked.
func TestStreamLimits(t *testing.T) {
	ts := newTestServer(t, func(o *Options) {
		o.PublicRead = true
		o.Streams = StreamLimits{PerMember: 2, Anonymous: 1, Total: 4}
	})
	want := func(resp *http.Response, code int, what string) {
		t.Helper()
		if resp.StatusCode != code {
			t.Fatalf("%s: status %d, want %d", what, resp.StatusCode, code)
		}
		if code == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "30" {
			t.Fatalf("%s: Retry-After %q", what, resp.Header.Get("Retry-After"))
		}
	}
	alice := ts.openStream(t, "alice")
	want(alice, http.StatusOK, "alice's first")
	want(ts.openStream(t, "alice"), http.StatusOK, "alice's second")
	want(ts.openStream(t, "alice"), http.StatusTooManyRequests, "alice's third")
	want(ts.openStream(t, ""), http.StatusOK, "the first without a token")
	want(ts.openStream(t, ""), http.StatusTooManyRequests, "the second without a token")
	want(ts.openStream(t, "bob"), http.StatusOK, "bob's first")
	want(ts.openStream(t, "bob"), http.StatusTooManyRequests, "bob's second, over the total")

	_ = alice.Body.Close()
	for deadline := time.Now().Add(5 * time.Second); ts.streamsOpen() > 3; {
		if time.Now().After(deadline) {
			t.Fatal("a closed stream still holds its place")
		}
		time.Sleep(time.Millisecond)
	}
	want(ts.openStream(t, "bob"), http.StatusOK, "bob's second, after alice closed one")
}

// stuckWriter is a client whose connection takes nothing until released.
type stuckWriter struct {
	*discardWriter
	release chan struct{}
}

func (w stuckWriter) Write(p []byte) (int, error) {
	<-w.release
	return w.discardWriter.Write(p)
}

// A stream the hub has let go of still counts while its handler is stuck
// writing to its client: otherwise one token could hold any number of
// handlers, and their buffers, by never reading.
func TestStreamHoldsItsPlaceUntilItsHandlerReturns(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.Streams = StreamLimits{PerMember: 1} })
	w := stuckWriter{newDiscardWriter(), make(chan struct{})}
	req := httptest.NewRequest(http.MethodGet, "/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
	done := make(chan struct{})
	go func() { defer close(done); ts.Handler().ServeHTTP(w, req) }()
	for !ts.subscribed(1) {
		time.Sleep(time.Millisecond)
	}
	ts.hub.closeAll() // as a token rotation does; the handler is stuck in its first write
	if resp := ts.openStream(t, "bob"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second stream while the first is stuck: %d", resp.StatusCode)
	}
	close(w.release)
	<-done
	if resp := ts.openStream(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a stream after the stuck one returned: %d", resp.StatusCode)
	}
}
