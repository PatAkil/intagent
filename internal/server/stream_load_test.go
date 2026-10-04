package server

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// discardWriter is a dashboard that reads every byte at once: a stream's
// handler writes into it as fast as it can, and it counts what it got.
type discardWriter struct {
	header  http.Header
	bytes   atomic.Int64
	writes  atomic.Int64
	flushes atomic.Int64
}

func newDiscardWriter() *discardWriter { return &discardWriter{header: http.Header{}} }

func (d *discardWriter) Header() http.Header { return d.header }
func (d *discardWriter) WriteHeader(int)     {}
func (d *discardWriter) Flush()              { d.flushes.Add(1) }

func (d *discardWriter) Write(p []byte) (int, error) {
	d.bytes.Add(int64(len(p)))
	d.writes.Add(1)
	return len(p), nil
}

func (d *discardWriter) SetWriteDeadline(time.Time) error { return nil }

// streamLoad is a server whose busy repository has members × perMember
// claims of files each, with one live session per claim, as the scale
// findings measured it.
type streamLoad struct {
	s      *Server
	tokens []string
	evs    []board.HookEvent // one per session, Kind and Paths left to fill
	files  int
	t0     time.Time
}

const loadRepo = "github.com/acme/busy"

func newStreamLoad(tb testing.TB, members, perMember, files int) *streamLoad {
	tb.Helper()
	f := &streamLoad{files: files, t0: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	var ms []Member
	for i := range members {
		tok, hash, err := NewToken()
		if err != nil {
			tb.Fatal(err)
		}
		f.tokens = append(f.tokens, tok)
		ms = append(ms, Member{Name: fmt.Sprintf("m%02d", i), TokenSHA256: hash})
	}
	s, err := New(Options{Members: ms, Board: board.DefaultConfig(), Now: func() time.Time { return f.t0 }})
	if err != nil {
		tb.Fatal(err)
	}
	f.s = s
	for m := range members {
		for k := range perMember {
			i := m*perMember + k
			ev := board.HookEvent{Member: ms[m].Name, Agent: board.AgentClaudeCode, SessionID: fmt.Sprintf("s%d", i),
				Where: board.Where{Repo: loadRepo, Host: "h", Worktree: fmt.Sprintf("/w/%d", i), Branch: "b"}}
			f.evs = append(f.evs, ev)
			ev.Kind = board.KindPostEdit
			for j := range files {
				ev.Paths = []board.PathRef{{Path: fmt.Sprintf("svc%d/f%d.go", i, j)}}
				if _, err := s.board.Hook(f.t0, ev); err != nil {
					tb.Fatal(err)
				}
			}
		}
	}
	return f
}

// openStreams connects n streams for the busy repository through the server's
// own handler, spread over the members, each writing into a discardWriter.
// stop ends them and waits for their handlers to return.
func (f *streamLoad) openStreams(tb testing.TB, n int) (writers []*discardWriter, stop func()) {
	tb.Helper()
	h := f.s.Handler()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := range n {
		w := newDiscardWriter()
		writers = append(writers, w)
		req := httptest.NewRequest(http.MethodGet, "/v1/stream?repo="+loadRepo, nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+f.tokens[i%len(f.tokens)])
		wg.Add(1)
		go func() { defer wg.Done(); h.ServeHTTP(w, req) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.subscribers() < n {
		if time.Now().After(deadline) {
			tb.Fatalf("%d of %d streams subscribed", f.subscribers(), n)
		}
		time.Sleep(time.Millisecond)
	}
	return writers, func() { cancel(); wg.Wait() }
}

func (f *streamLoad) subscribers() int {
	f.s.hub.mu.Lock()
	defer f.s.hub.mu.Unlock()
	return len(f.s.hub.subs)
}

// hook sends one hook from a random session: a post_edit of one of its own
// files (one file.changed activity) with probability active, otherwise a
// pre_edit of one, which records nothing. It reports whether it recorded.
func (f *streamLoad) hook(r *rand.Rand, active float64) bool {
	i := r.IntN(len(f.evs))
	ev := f.evs[i]
	ev.Paths = []board.PathRef{{Path: fmt.Sprintf("svc%d/f%d.go", i, r.IntN(f.files))}}
	post := r.Float64() < active
	ev.Kind = board.KindPreEdit
	if post {
		ev.Kind = board.KindPostEdit
	}
	_, _ = f.s.board.Hook(f.t0, ev)
	return post
}

// activityRate runs writers goroutines sending hooks as fast as they can for
// d, a share active of which record an activity, and returns how many of
// those were answered per second.
func (f *streamLoad) activityRate(writers int, d time.Duration, active float64) float64 {
	var done atomic.Int64
	stop := time.Now().Add(d)
	start := time.Now()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for time.Now().Before(stop) {
				if f.hook(r, active) {
					done.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	return float64(done.Load()) / time.Since(start).Seconds()
}

// Dashboards watching the busy repository must not slow down the hooks that
// feed them. Each writer hands its activities to the streams on its way out
// of the board's lock, and the next writer waits for that while it holds the
// lock, so a costly hand-over or a CPU busy writing to streams holds up every
// hook. With 300 streams and 70% of hooks recording an activity, those hooks
// ran at 0.17-0.24 of their rate without streams.
func TestStreamsDoNotHoldUpTheBoard(t *testing.T) {
	f := newStreamLoad(t, 30, 10, 20)
	const writers, active = 8, 0.7
	d := 300 * time.Millisecond
	var alone, watched float64
	for range 2 { // the best of two, each way, on a machine shared with others
		alone = max(alone, f.activityRate(writers, d, active))
		_, stop := f.openStreams(t, 300)
		watched = max(watched, f.activityRate(writers, d, active))
		stop()
	}
	t.Logf("activity hooks/s: %.0f without streams, %.0f with 300 (%.2fx)", alone, watched, watched/alone)
	if watched < 0.5*alone {
		t.Fatalf("with 300 streams, activity hooks ran at %.2f of their rate without streams; want at least 0.5", watched/alone)
	}
}
