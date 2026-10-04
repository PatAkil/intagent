package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// waitFor polls cond until it holds, failing the test after a generous while.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// heldViews makes the server's board builds count themselves, and the first
// one wait, after reading the board, until release is closed.
type heldViews struct {
	calls   atomic.Int64
	read    chan struct{} // closed when the first build has read the board
	release func()
}

func holdViews(t *testing.T, s *Server) *heldViews {
	release := make(chan struct{})
	h := &heldViews{read: make(chan struct{}), release: sync.OnceFunc(func() { close(release) })}
	t.Cleanup(h.release) // a failed test must not leave a request hanging
	s.boards.wrapView(func(view func(string) board.View) func(string) board.View {
		return func(repo string) board.View {
			v := view(repo)
			if h.calls.Add(1) == 1 {
				close(h.read)
				<-release
			}
			return v
		}
	})
	return h
}

// wrapView replaces the function builds read the board with, ordered after
// the builds before it.
func (c *boardBuilds) wrapView(wrap func(func(string) board.View) func(string) board.View) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.view = wrap(c.view)
}

func (c *boardBuilds) entries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.repos)
}

// waiting is how many requests wait for the build of repo that has not read
// the board yet; -1 when there is none.
func (c *boardBuilds) waiting(repo string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.repos[repo]; e != nil && e.next != nil {
		return e.next.requests
	}
	return -1
}

func (ts *testServer) getBoard(t *testing.T, member string) (board.View, int) {
	t.Helper()
	var v board.View
	code := ts.do(t, http.MethodGet, "/v1/board?repo="+repo, member, nil, &v)
	return v, code
}

func hasFile(v board.View, path string) bool {
	for _, c := range v.Claims {
		for _, f := range c.Files {
			if f.Path == path {
				return true
			}
		}
	}
	return false
}

func TestBoardSharesBuilds(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	held := holdViews(t, ts.Server)

	const readers = 100
	var wg sync.WaitGroup
	views := make([]board.View, readers)
	codes := make([]int, readers)
	get := func(i int) {
		defer wg.Done()
		var v board.View
		req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/board?repo="+repo, nil)
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		codes[i] = resp.StatusCode
		_ = json.NewDecoder(resp.Body).Decode(&v)
		views[i] = v
	}
	wg.Add(1)
	go get(0)
	<-held.read
	// Every request that arrives while that build is held shares the next.
	for i := 1; i < readers; i++ {
		wg.Add(1)
		go get(i)
	}
	waitFor(t, "the readers to queue", func() bool { return ts.boards.waiting(repo) == readers-1 })
	held.release()
	wg.Wait()
	if n := held.calls.Load(); n > 2 {
		t.Fatalf("%d readers made %d builds, want at most 2", readers, n)
	}
	for i := range readers {
		if codes[i] != http.StatusOK || !hasFile(views[i], "x/y.go") {
			t.Fatalf("reader %d: %d %+v", i, codes[i], views[i])
		}
	}
}

// A build that has read the board answers no request that arrived after the
// read: a reader always sees the writes it saw finish. A cache that serves an
// answer younger than some age, or a build open to requests until it returns,
// fails this.
func TestBoardReadYourWrites(t *testing.T) {
	ts := newTestServer(t)
	if v, _ := ts.getBoard(t, "bob"); len(v.Claims) != 0 {
		t.Fatalf("board not empty: %+v", v)
	}
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	if v, _ := ts.getBoard(t, "bob"); !hasFile(v, "x/y.go") {
		t.Fatalf("a read after a write misses it: %+v", v)
	}

	held := holdViews(t, ts.Server)
	first := make(chan board.View)
	go func() { v, _ := ts.getBoard(t, "bob"); first <- v }()
	<-held.read
	// The build in progress has read the board; this write comes after it.
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/late.go"), nil)
	second := make(chan board.View)
	go func() { v, _ := ts.getBoard(t, "bob"); second <- v }()
	// Give the second reader time to arrive, so that a build still taking
	// requests would take it; here it starts the next build instead.
	time.Sleep(50 * time.Millisecond)
	waitFor(t, "the second reader to queue", func() bool { return ts.boards.waiting(repo) == 1 })
	held.release()
	if v := <-first; hasFile(v, "x/late.go") {
		t.Fatal("the held build saw a write made after it read the board")
	}
	if v := <-second; !hasFile(v, "x/late.go") {
		t.Fatalf("a read after a write was answered from before it: %+v", v)
	}
}

// fakePacing replaces the clock and sleep that space builds.
type fakePacing struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (p *fakePacing) install(c *boardBuilds) {
	p.now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock = func() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.now }
	c.sleep = func(d time.Duration) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.slept = append(p.slept, d)
		p.now = p.now.Add(d)
	}
}

func (p *fakePacing) advance(d time.Duration) { p.mu.Lock(); defer p.mu.Unlock(); p.now = p.now.Add(d) }

func (p *fakePacing) sleeps() string { p.mu.Lock(); defer p.mu.Unlock(); return fmt.Sprint(p.slept) }

func TestBoardBuildsAreSpaced(t *testing.T) {
	ts := newTestServer(t)
	var p fakePacing
	p.install(ts.boards)
	var took atomic.Int64
	took.Store(int64(100 * time.Millisecond))
	ts.boards.wrapView(func(view func(string) board.View) func(string) board.View {
		return func(repo string) board.View { p.advance(time.Duration(took.Load())); return view(repo) }
	})

	ts.getBoard(t, "bob") // builds at once; takes 100ms
	took.Store(int64(300 * time.Millisecond))
	ts.getBoard(t, "bob") // waits until 250ms after the first started; takes 300ms
	ts.getBoard(t, "bob") // waits until 600ms after the second started
	want := fmt.Sprint([]time.Duration{150 * time.Millisecond, 300 * time.Millisecond})
	if got := p.sleeps(); got != want {
		t.Fatalf("slept %v, want %v", got, want)
	}
	p.advance(time.Second)
	ts.getBoard(t, "bob")
	if got := p.sleeps(); got != want {
		t.Fatalf("a build long after the last one waited: %v", got)
	}
}

func TestBoardBuildsOneAtATime(t *testing.T) {
	ts := newTestServer(t)
	held := holdViews(t, ts.Server)
	var other atomic.Int64
	ts.boards.wrapView(func(view func(string) board.View) func(string) board.View {
		return func(r string) board.View {
			if r == "github.com/acme/other" {
				other.Add(1)
			}
			return view(r)
		}
	})
	done := make(chan struct{})
	go func() { ts.getBoard(t, "bob"); done <- struct{}{} }()
	<-held.read
	go func() {
		ts.do(t, http.MethodGet, "/v1/board?repo=github.com/acme/other", "bob", nil, nil)
		done <- struct{}{}
	}()
	waitFor(t, "the other repository's build to wait", func() bool { return ts.boards.waiting("github.com/acme/other") == 1 })
	time.Sleep(20 * time.Millisecond)
	if other.Load() != 0 {
		t.Fatal("two builds ran at once")
	}
	held.release()
	<-done
	<-done
	if other.Load() != 1 {
		t.Fatalf("the other repository was built %d times", other.Load())
	}
}

func TestBoardBuildPanicFailsItsReadersAndNothingElse(t *testing.T) {
	ts := newTestServer(t)
	c := ts.boards
	var calls atomic.Int64
	c.wrapView(func(view func(string) board.View) func(string) board.View {
		return func(r string) board.View {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			return view(r)
		}
	})
	c.slot <- struct{}{} // hold the server's build slot so readers gather
	// The first to ask builds, and its goroutine panics as a handler's would;
	// the others share the failed build.
	results := make(chan any, 4)
	for range 4 {
		go func() {
			defer func() {
				if p := recover(); p != nil {
					results <- p
				}
			}()
			_, err := c.get(context.Background(), repo)
			results <- err
		}()
	}
	waitFor(t, "readers to gather", func() bool { return c.waiting(repo) == 4 })
	<-c.slot
	panics, failed := 0, 0
	for range 4 {
		var r any
		select {
		case r = <-results:
		case <-time.After(10 * time.Second):
			t.Fatal("a reader of the failed build still waits")
		}
		switch r := r.(type) {
		case string:
			panics++
		case error:
			if errors.Is(r, errBoardBuild) {
				failed++
			}
		}
	}
	if panics != 1 || failed != 3 {
		t.Fatalf("%d panics and %d failed readers, want 1 and 3", panics, failed)
	}
	b, err := c.get(context.Background(), repo)
	if err != nil || b.view.Repo != repo {
		t.Fatalf("after a failed build: %v %+v", err, b)
	}
}

func TestBoardEntriesAreBounded(t *testing.T) {
	ts := newTestServer(t)
	var p fakePacing
	p.install(ts.boards)
	for i := range maxBoardEntries + 40 {
		if code := ts.do(t, http.MethodGet, fmt.Sprintf("/v1/board?repo=r/%d", i), "bob", nil, nil); code != http.StatusOK {
			t.Fatalf("repo %d: %d", i, code)
		}
	}
	if n := ts.boards.entries(); n != maxBoardEntries {
		t.Fatalf("%d entries", n)
	}
	// Entries with nothing left to pace go as soon as another is wanted.
	p.advance(time.Second)
	ts.do(t, http.MethodGet, "/v1/board?repo=r/new", "bob", nil, nil)
	if n := ts.boards.entries(); n != 1 {
		t.Fatalf("%d entries after they aged", n)
	}
}

// An entry stays while a build of it runs, however long ago that build
// started: when a request for another repository prunes the entries, later
// requests for this one still queue behind the build and keep their spacing.
func TestBoardEntryStaysWhileItBuilds(t *testing.T) {
	ts := newTestServer(t)
	var p fakePacing
	p.install(ts.boards)
	read, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	var calls atomic.Int64
	ts.boards.wrapView(func(view func(string) board.View) func(string) board.View {
		return func(r string) board.View {
			if r == repo && calls.Add(1) == 1 {
				p.advance(time.Second) // past the entry's gap, while it builds
				close(read)
				<-release
			}
			return view(r)
		}
	})
	done := make(chan struct{}, 3)
	get := func(r string) {
		ts.do(t, http.MethodGet, "/v1/board?repo="+r, "bob", nil, nil)
		done <- struct{}{}
	}
	go get(repo)
	<-read
	go get("github.com/acme/other")
	waitFor(t, "the other repository's build to wait", func() bool { return ts.boards.waiting("github.com/acme/other") == 1 })
	go get(repo)
	waitFor(t, "the repository's next build to wait", func() bool { return ts.boards.waiting(repo) == 1 })
	free()
	for range 3 {
		<-done
	}
	// The first build read for 1 s, so the next starts 2 s after it did.
	if got, want := p.sleeps(), fmt.Sprint([]time.Duration{time.Second}); got != want {
		t.Fatalf("slept %v, want %v", got, want)
	}
}

func TestBoardAnswerEncodings(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	get := func(header http.Header) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/board?repo="+repo, nil)
		req.Header = header
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		// The transport neither adds Accept-Encoding nor decodes the answer.
		resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.Header.Get("Content-Length") != fmt.Sprint(len(body)) || resp.Header.Get("Vary") != "Accept-Encoding" {
			t.Fatalf("headers: %v for %d bytes", resp.Header, len(body))
		}
		return resp, body
	}
	resp, plain := get(http.Header{})
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("plain answer encoded %q", resp.Header.Get("Content-Encoding"))
	}
	// The plain answer is what writeJSON writes for the view.
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, ts.boardView(repo))
	if !bytes.Equal(plain, rec.Body.Bytes()) {
		t.Fatalf("answer changed:\n%s\n%s", plain, rec.Body.Bytes())
	}
	resp, packed := get(http.Header{"Accept-Encoding": {"br;q=1, gzip;q=0.5"}})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip not used: %v", resp.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	if unpacked, _ := io.ReadAll(zr); !bytes.Equal(unpacked, plain) {
		t.Fatal("the gzip answer is not the plain one")
	}
	if resp, _ := get(http.Header{"Accept-Encoding": {"gzip;q=0"}}); resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("gzip sent to a client that refused it")
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                         false,
		"gzip":                     true,
		"gzip, deflate, br, zstd":  true,
		"GZIP;q=0.001":             true,
		"gzip;q=0":                 false,
		"gzip; q=0.0, *":           false,
		"br, *;q=0.1":              true,
		"*;q=0":                    false,
		"x-gzip":                   true,
		"identity":                 false,
		"deflate, gzip;q=nonsense": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v", header, got)
		}
	}
}

func TestBoardETag(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	get := func(etag string) (int, string, []byte) {
		req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/board?repo="+repo, nil)
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.Header.Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
		}
		return resp.StatusCode, resp.Header.Get("ETag"), body
	}
	code, tag, _ := get("")
	if code != http.StatusOK || !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("first answer: %d %q", code, tag)
	}
	// Later, and after events elsewhere, the board shows the same: 304.
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Minute)
	ts.mu.Unlock()
	other := hookEv(board.KindPostEdit, "bob", "b1", "z.go")
	other.Where.Repo = "github.com/acme/other"
	ts.do(t, "POST", "/v1/hook", "bob", other, nil)
	if code, again, body := get(tag); code != http.StatusNotModified || again != tag || len(body) != 0 {
		t.Fatalf("unchanged board: %d %q %q", code, again, body)
	}
	if code, _, _ := get(`"nope", ` + tag); code != http.StatusNotModified {
		t.Fatalf("a list of tags: %d", code)
	}
	// A change in the repository is a new answer.
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/z.go"), nil)
	code, changed, body := get(tag)
	if code != http.StatusOK || changed == tag || !strings.Contains(string(body), "x/z.go") {
		t.Fatalf("changed board: %d %q", code, changed)
	}
}

func TestBoardETagIgnoresOnlyWhenAndSeq(t *testing.T) {
	v := board.View{Repo: repo, At: time.Unix(100, 5), LastSeq: 7, Claims: []board.ClaimView{{ID: "c_1", Member: "alice"}}, Epoch: "e1"}
	tag := func(v board.View) string {
		b, _ := json.Marshal(v)
		return boardETag(append(b, '\n'))
	}
	base := tag(v)
	if base == "" {
		t.Fatal("no tag for a view")
	}
	same := v
	same.At, same.LastSeq = time.Unix(999, 1), 12345
	if tag(same) != base {
		t.Fatal("the tag depends on when the view was read")
	}
	for name, change := range map[string]func(*board.View){
		"claims":   func(v *board.View) { v.Claims = []board.ClaimView{{ID: "c_1", Member: "bob"}} },
		"sessions": func(v *board.View) { v.Sessions = 1 },
		"epoch":    func(v *board.View) { v.Epoch = "e2" },
		"repo":     func(v *board.View) { v.Repo = "github.com/acme/other" },
	} {
		w := v
		change(&w)
		if tag(w) == base {
			t.Errorf("the tag ignores %s", name)
		}
	}
	for _, body := range []string{"", "{}", `{"repo":"x"}`, `{"repo":"x","at":"t"}`, `["repo"]`} {
		if got := boardETag([]byte(body)); got != "" {
			t.Errorf("boardETag(%q) = %q", body, got)
		}
	}
}

func TestEtagMatch(t *testing.T) {
	for _, c := range []struct {
		header, tag string
		want        bool
	}{
		{`W/"a"`, `W/"a"`, true},
		{`"a"`, `W/"a"`, true},
		{`"b", W/"a"`, `W/"a"`, true},
		{`*`, `W/"a"`, true},
		{`W/"b"`, `W/"a"`, false},
		{``, `W/"a"`, false},
		{`W/"a"`, ``, false},
	} {
		if got := etagMatch(c.header, c.tag); got != c.want {
			t.Errorf("etagMatch(%q, %q) = %v", c.header, c.tag, got)
		}
	}
}

func TestEpoch(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	v, _ := ts.getBoard(t, "bob")
	var repos []board.RepoSummary
	ts.do(t, http.MethodGet, "/v1/repos", "bob", nil, &repos)
	if v.Epoch == "" || len(repos) != 1 || repos[0].Epoch != v.Epoch {
		t.Fatalf("epochs: view %q, repos %+v", v.Epoch, repos)
	}
	if again := newTestServer(t); again.epoch == v.Epoch {
		t.Fatal("two server processes share an epoch")
	}
}

func TestAgentBoardText(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	ts.do(t, "POST", "/v1/hook", "bob", hookEv(board.KindPostEdit, "bob", "b1", "x/z.go"), nil)
	text := func(query string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/board?format=text&repo="+repo+query, nil)
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	w := where("bob")
	code, got := text("&host=" + w.Host + "&worktree=" + w.Worktree + "&limit=20")
	if code != http.StatusOK || !strings.Contains(got, "alice on b-alice") || strings.Contains(got, "bob on b-bob") {
		t.Fatalf("agent text: %d %s", code, got)
	}
	// Without a limit the text is the whole board, as before.
	if _, full := text(""); full != ts.Board().View(ts.now(), repo).Text()+"\n" {
		t.Fatalf("full text changed: %s", full)
	}
	for _, bad := range []string{"&limit=0", "&limit=-1", "&limit=x", "&limit="} {
		if code, _ := text(bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
}
