package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// rawStream opens a stream as member and returns its reader. A test that
// waits for more than the stream sends fails after 10 s rather than hang.
func (ts *testServer) rawStream(t *testing.T, member, query string, header http.Header) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	ts.hub.closeUnless(func(memberHash) bool { return false }) // as rotating bob's token does; the handler is stuck in its first write
	if resp := ts.openStream(t, "bob"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second stream while the first is stuck: %d", resp.StatusCode)
	}
	close(w.release)
	<-done
	if resp := ts.openStream(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a stream after the stuck one returned: %d", resp.StatusCode)
	}
}

// readGoodbye reads what is left of a stream the server ended: the delay it
// gives its client to reconnect after, then the end.
func readGoodbye(t *testing.T, r *bufio.Reader) int {
	t.Helper()
	rest := readBlocks(t, r, 1)
	var retry int
	if _, err := fmt.Sscanf(rest, "retry: %d\n\n", &retry); err != nil || retry < retryBase || retry >= retryBase+retrySpread {
		t.Fatalf("the stream ended with %q, want a retry: of 3 to 8 s", rest)
	}
	if line, err := r.ReadString('\n'); err == nil {
		t.Fatalf("the stream went on after its goodbye: %q", line)
	}
	return retry
}

// A change to the team file ends only the streams whose token it revoked: a
// new member, or a touch of the file, used to disconnect every dashboard,
// and all of them reconnected and reloaded the board 3 s later, together.
func TestMemberChangesEndOnlyTheirStreams(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.PublicRead = true })
	cookie := http.Header{"Cookie": {cookieName + "=" + ts.login(t, "bob", nil)}}
	open := func(member string, h http.Header) *bufio.Reader {
		r, closeStream := ts.rawStream(t, member, "", h)
		t.Cleanup(closeStream)
		readBlocks(t, r, 1)
		return r
	}
	alice, bob, bobCookie, anyone := open("alice", nil), open("bob", nil), open("", cookie), open("", nil)
	streams := func() int {
		ts.hub.mu.Lock()
		defer ts.hub.mu.Unlock()
		return len(ts.hub.subs)
	}
	member := func(name, hash string) Member { return Member{Name: name, TokenSHA256: hash} }
	aliceM, bobM := member("alice", HashToken(ts.tokens["alice"])), member("bob", HashToken(ts.tokens["bob"]))
	_, carolHash, _ := NewToken()
	_, rotated, _ := NewToken()

	for _, step := range []struct {
		what    string
		members []Member
		ended   []*bufio.Reader
	}{
		{"the same members", []Member{aliceM, bobM}, nil},
		{"carol added", []Member{aliceM, bobM, member("carol", carolHash)}, nil},
		{"bob's token rotated", []Member{aliceM, member("bob", rotated)}, []*bufio.Reader{bob, bobCookie}},
		{"alice removed", []Member{member("bob", rotated)}, []*bufio.Reader{alice}},
	} {
		before := streams()
		if err := ts.SetMembers(step.members); err != nil {
			t.Fatal(err)
		}
		for _, r := range step.ended {
			readGoodbye(t, r)
		}
		if got, want := streams(), before-len(step.ended); got != want {
			t.Fatalf("%s: %d streams open, want %d", step.what, got, want)
		}
	}
	// No member change revokes the stream opened without a token.
	ts.hub.publish([]board.Activity{{Seq: 1, At: ts.now(), Kind: board.ActivityNoteSent, Repo: repo, Member: "bob"}})
	if evs := readEvents(t, anyone, 1); evs[0].Seq != 1 {
		t.Fatalf("events: %+v", evs)
	}
}

// A token rotated after a stream's request was authorised, but before the
// stream subscribed, must not keep the stream: closing the streams already
// subscribed missed it, and it carried the team's activity until the server
// restarted. The finding saw 1.2 such streams survive each rotation.
func TestRevokedTokenCannotSlipIntoAStream(t *testing.T) {
	ts := newTestServer(t)
	cookie := ts.login(t, "alice", nil)
	for _, c := range []struct {
		member string
		auth   func(*http.Request)
	}{
		{"bob", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+ts.tokens["bob"]) }},
		{"alice", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie}) }},
	} {
		h := ts.read(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The token is rotated between authorising the request and subscribing its stream.
			var ms []Member
			for _, name := range []string{"alice", "bob"} {
				hash := HashToken(ts.tokens[name])
				if name == c.member {
					_, hash, _ = NewToken()
				}
				ms = append(ms, Member{Name: name, TokenSHA256: hash})
			}
			if err := ts.SetMembers(ms); err != nil {
				t.Error(err)
			}
			ts.handleStream(w, r)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := httptest.NewRequest(http.MethodGet, "/v1/stream", nil).WithContext(ctx)
		c.auth(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		cancel()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s's stream, rotated in between: status %d, want 401", c.member, rec.Code)
		}
		if ts.subscribed(1) {
			t.Fatalf("%s's refused stream is still subscribed", c.member)
		}
	}
}

// When the server ends its streams together, at shutdown, each tells its
// dashboard to come back after its own delay, 3 to 8 s, so they do not all
// reconnect and reload the board at once.
func TestShutdownSpreadsReconnects(t *testing.T) {
	ts := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Serve(ctx, ln) }()
	ts.url = "http://" + ln.Addr().String()
	var streams []*bufio.Reader
	for range 5 {
		r, closeStream := ts.rawStream(t, "bob", "", nil)
		defer closeStream()
		readBlocks(t, r, 1)
		streams = append(streams, r)
	}
	cancel()
	got, want := map[int]bool{}, map[int]bool{}
	for i, r := range streams {
		got[readGoodbye(t, r)] = true
		want[reconnectDelay("bob", uint64(i+1))] = true
	}
	if len(got) < 2 || len(got) != len(want) {
		t.Fatalf("reconnect delays %v, want %v", got, want)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A stream that reconnects after the board let go of some of what it missed
// is told so before the replay, and only then: the dashboard marks the hole
// and reloads, rather than show a feed with a silent gap in it.
func TestStreamSignalsAGapInItsReplay(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.Board.KeepActivities = 4 })
	for _, p := range []string{"x/y.go", "x/z.go", "x/w.go", "x/v.go"} { // seqs 1-3, then 4, 5 and 6
		ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", p), nil)
	}
	kept := ts.Board().Since("", 0) // 3 to 6; 1 and 2 were let go of
	replay := oldFrame(kept[0]) + oldFrame(kept[1]) + oldFrame(kept[2]) + oldFrame(kept[3])
	prelude := "retry: 3000\n: connected\n\n"
	for _, c := range []struct {
		last string
		want string
	}{
		{"1", prelude + "event: gap\ndata: {\"after\":1}\n\n" + replay},
		{"2", prelude + replay},
		{"6", prelude},
	} {
		r, closeStream := ts.rawStream(t, "bob", "?repo="+repo, http.Header{"Last-Event-Id": {c.last}})
		got := readBlocks(t, r, strings.Count(c.want, "\n\n"))
		closeStream()
		if got != c.want {
			t.Errorf("after %s:\n%q\nwant\n%q", c.last, got, c.want)
		}
	}
}

// gatedWriter is a client that records what its stream writes. While held,
// each write says so on waiting, then waits until the client is released.
type gatedWriter struct {
	*discardWriter
	waiting chan struct{}
	mu      sync.Mutex
	out     bytes.Buffer
	gate    chan struct{} // nil while released
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{discardWriter: newDiscardWriter(), waiting: make(chan struct{})}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	gate := w.gate
	w.mu.Unlock()
	if gate != nil {
		w.waiting <- struct{}{}
		<-gate
	}
	w.mu.Lock()
	w.out.Write(p)
	w.mu.Unlock()
	return w.discardWriter.Write(p)
}

func (w *gatedWriter) hold() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gate = make(chan struct{})
}

func (w *gatedWriter) release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.gate)
	w.gate = nil
}

func (w *gatedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.String()
}

// serveStream runs a stream for member through the server's handler into w,
// and returns a channel closed when the handler returns. The stream ends
// with the test.
func (ts *testServer) serveStream(t *testing.T, w http.ResponseWriter, member, query string) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/stream"+query, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+ts.tokens[member])
	done := make(chan struct{})
	go func() { defer close(done); ts.Handler().ServeHTTP(w, req) }()
	t.Cleanup(func() { cancel(); <-done })
	return done
}

// A stream that falls more than streamRing publishes behind is ended, and
// its dashboard reconnects and catches up from the last event it saw. It
// must never be handed what newer publishes left in the ring in place of what
// it missed: it would skip its mark past a thousand activities it never sent,
// and its dashboard would neither reconnect nor show a gap.
func TestStreamTooFarBehindIsEnded(t *testing.T) {
	ts := newTestServer(t)
	w := newGatedWriter()
	done := ts.serveStream(t, w, "bob", "?repo="+repo)
	prelude := "retry: 3000\n: connected\n\n"
	for w.bytes.Load() < int64(len(prelude)) {
		time.Sleep(time.Millisecond)
	}
	act := func(seq int) board.Activity {
		return board.Activity{Seq: uint64(seq), At: ts.now(), Kind: board.ActivityFileChanged, Repo: repo, Member: "alice",
			Paths: []string{fmt.Sprintf("x/f%d.go", seq)}}
	}
	w.hold()
	ts.hub.publish([]board.Activity{act(1)})
	<-w.waiting // the stream is writing 1 to a client that takes nothing
	for seq := 2; seq <= streamRing+100; seq++ {
		ts.hub.publish([]board.Activity{act(seq)})
	}
	w.release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		got := w.String()
		t.Fatalf("a stream %d publishes behind was not ended; it sent %d activities, the last ending %q",
			streamRing+99, strings.Count(got, "event: activity"), got[max(0, len(got)-80):])
	}
	retry := reconnectDelay("bob", 1)
	if got, want := w.String(), prelude+oldFrame(act(1))+fmt.Sprintf("retry: %d\n\n", retry); got != want {
		t.Fatalf("the stream sent\n%q\nwant\n%q", got, want)
	}
}

// A stream sends each activity once, though one replayed on reconnection can
// also arrive live, and a repository's stream sends only that repository's.
func TestStreamSendsEachActivityOnceAndOnlyItsRepository(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil) // seqs 1-3
	acts := ts.Board().Since("", 0)
	r, closeStream := ts.rawStream(t, "bob", "?repo="+repo, http.Header{"Last-Event-Id": {"1"}})
	defer closeStream()
	if got, want := readBlocks(t, r, 3), "retry: 3000\n: connected\n\n"+oldFrame(acts[1])+oldFrame(acts[2]); got != want {
		t.Fatalf("replay:\n%q\nwant\n%q", got, want)
	}
	// 3 again, as when it is published just after the stream subscribed;
	// another repository's 4; then 5.
	other := board.Activity{Seq: 4, At: ts.now(), Kind: board.ActivityNoteSent, Repo: "github.com/acme/other", Member: "bob"}
	next := board.Activity{Seq: 5, At: ts.now(), Kind: board.ActivityNoteSent, Repo: repo, Member: "bob"}
	ts.hub.publish([]board.Activity{acts[2]})
	ts.hub.publish([]board.Activity{other})
	ts.hub.publish([]board.Activity{next})
	if got := readBlocks(t, r, 1); got != oldFrame(next) {
		t.Fatalf("after the replay the stream sent\n%q\nwant\n%q", got, oldFrame(next))
	}
}

// A stream writes a large catch-up a piece at a time, each with a deadline
// of its own, so a client that reads it slowly but steadily gets all of it.
func TestStreamWritesInPieces(t *testing.T) {
	ts := newTestServer(t)
	w := newDiscardWriter()
	ts.serveStream(t, w, "bob", "?repo="+repo)
	for !ts.subscribed(1) {
		time.Sleep(time.Millisecond)
	}
	var acts []board.Activity
	var want, largest int64
	for i := range 2000 {
		a := board.Activity{Seq: uint64(i + 1), At: ts.now(), Kind: board.ActivityFileChanged, Repo: repo, Member: "alice",
			Paths: []string{fmt.Sprintf("x/f%d.go", i)}}
		acts = append(acts, a)
		want += int64(len(oldFrame(a)))
		largest = max(largest, int64(len(oldFrame(a))))
	}
	ts.hub.publish(acts)
	waitForBytes(t, []*discardWriter{w}, int64(len("retry: 3000\n: connected\n\n"))+want)
	if got, limit := w.largest.Load(), streamPiece+largest; got >= limit {
		t.Fatalf("%d bytes of activities went out in a write of %d; want writes under %d", want, got, limit)
	}
}

// failingFlusher is a client whose flushes fail, as one whose write deadline
// has passed.
type failingFlusher struct{ *discardWriter }

func (w failingFlusher) FlushError() error {
	w.flushes.Add(1)
	return errors.New("write deadline exceeded")
}

// A stream whose flush fails ends there: its client takes nothing more, and
// the handler must not hold on to it until the next write fails as well.
func TestStreamEndsWhenAFlushFails(t *testing.T) {
	ts := newTestServer(t)
	w := failingFlusher{newDiscardWriter()}
	done := ts.serveStream(t, w, "bob", "")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream went on after its flush failed")
	}
	if w.writes.Load() != 1 || w.flushes.Load() != 1 {
		t.Fatalf("%d writes and %d flushes; want the stream ended after its first", w.writes.Load(), w.flushes.Load())
	}
}

// The ring holds the last streamRing publishes, and no more: a stream that
// many behind takes them all, and one further behind is told it cannot.
func TestHubKeepsTheLastStreamRingPublishes(t *testing.T) {
	for _, behind := range []int{1, streamRing - 1, streamRing, streamRing + 1, 2 * streamRing} {
		h := newHub(StreamLimits{})
		h.subscribe(repo, memberHash{name: "bob"})
		for i := range 3*streamRing + 7 { // the ring wraps
			h.publish([]board.Activity{{Seq: uint64(i + 1), Repo: repo}})
		}
		from := h.next - uint64(behind)
		batches, next, _, ok := h.since(from, nil)
		if want := behind <= streamRing; ok != want {
			t.Fatalf("%d behind: ok %v, want %v", behind, ok, want)
		}
		if next != h.next {
			t.Fatalf("%d behind: next %d, want %d", behind, next, h.next)
		}
		if !ok {
			continue
		}
		if len(batches) != behind {
			t.Fatalf("%d behind: %d publishes", behind, len(batches))
		}
		for i, b := range batches {
			if want := from + uint64(i) + 1; len(b) != 1 || b[0].seq != want {
				t.Fatalf("%d behind: publish %d holds %+v, want seq %d", behind, i, b, want)
			}
		}
	}
}
