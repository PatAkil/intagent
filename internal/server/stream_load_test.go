package server

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
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
	largest atomic.Int64 // the largest write
	flushes atomic.Int64
}

func newDiscardWriter() *discardWriter { return &discardWriter{header: http.Header{}} }

func (d *discardWriter) Header() http.Header { return d.header }
func (d *discardWriter) WriteHeader(int)     {}
func (d *discardWriter) Flush()              { d.flushes.Add(1) }

func (d *discardWriter) Write(p []byte) (int, error) {
	d.bytes.Add(int64(len(p)))
	d.writes.Add(1)
	for n := int64(len(p)); ; {
		if l := d.largest.Load(); n <= l || d.largest.CompareAndSwap(l, n) {
			break
		}
	}
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

// burst sends n activity hooks from writers goroutines at once, each from a
// random session, and returns the activities they recorded.
func (f *streamLoad) burst(tb testing.TB, n, writers int) []board.Activity {
	tb.Helper()
	before := f.s.board.Since("", 0)
	last := before[len(before)-1].Seq
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for range n / writers {
				f.hook(r, 1)
			}
		}()
	}
	wg.Wait()
	acts := f.s.board.Since(loadRepo, last)
	if len(acts) != n/writers*writers {
		tb.Fatalf("%d hooks recorded %d activities", n/writers*writers, len(acts))
	}
	return acts
}

// mallocs counts the heap allocations every goroutine makes while fn runs.
func mallocs(fn func()) uint64 {
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	fn()
	runtime.ReadMemStats(&m1)
	return m1.Mallocs - m0.Mallocs
}

// waitForBytes waits until every writer has received want bytes.
func waitForBytes(tb testing.TB, writers []*discardWriter, want int64) {
	tb.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(time.Millisecond) {
		behind := 0
		for _, w := range writers {
			if w.bytes.Load() < want {
				behind++
			}
		}
		if behind == 0 {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("%d of %d streams have not received their %d bytes", behind, len(writers), want)
		}
	}
}

// flushes counts the flushes of every writer.
func flushes(writers []*discardWriter) (n int64) {
	for _, w := range writers {
		n += w.flushes.Load()
	}
	return n
}

// Dashboards watching the busy repository must not slow down the hooks that
// feed them. Each writer hands its activities to the streams on its way out
// of the board's lock, and the next writer waits for that while it holds the
// lock, so a hand-over that costs something per stream, or a CPU kept busy
// writing to streams, holds up every hook: with 300 streams and 70% of hooks
// recording an activity, those hooks ran at 0.10-0.24 of their rate without
// streams. The work that did it is counted here rather than timed, so a slow
// or shared machine cannot fail the test. Each stream encoded each activity
// itself, 5 allocations a time (8 with the race detector), where it now takes
// frames encoded once, 0.2; and it flushed each activity, where it now
// flushes at most once per streamLinger. BenchmarkActivityHooks times the
// hooks themselves.
func TestStreamsDoNotHoldUpTheBoard(t *testing.T) {
	const streams, n, writers = 300, 200, 8
	f := newStreamLoad(t, 30, 10, 20)
	alone := mallocs(func() { f.burst(t, n, writers) })

	ws, stop := f.openStreams(t, streams)
	defer stop()
	prelude := int64(len("retry: 3000\n: connected\n\n"))
	waitForBytes(t, ws, prelude)
	flushed := flushes(ws)
	var acts []board.Activity
	start := time.Now()
	watched := mallocs(func() {
		acts = f.burst(t, n, writers)
		want := prelude
		for _, a := range acts {
			want += int64(len(oldFrame(a)))
		}
		waitForBytes(t, ws, want)
	})
	took := time.Since(start)
	flushed = flushes(ws) - flushed

	perDelivery := (float64(watched) - float64(alone)) / float64(streams*len(acts))
	t.Logf("%d activities to %d streams in %s: %d allocations, %d without streams (%.2f more per delivery); %d flushes",
		len(acts), streams, took.Round(time.Millisecond), watched, alone, perDelivery, flushed)
	if perDelivery >= 1 {
		t.Errorf("the streams made %.2f allocations each per activity; want less than 1, as they take frames encoded once", perDelivery)
	}
	// One flush per stream for its first wake-up, then at most one per
	// streamLinger, and one more should a keep-alive fall in the burst.
	if limit := int64(streams) * (2 + int64(took/streamLinger)); flushed > limit {
		t.Errorf("%d streams flushed %d times for %d activities in %s; want at most %d, one per stream per %s",
			streams, flushed, len(acts), took.Round(time.Millisecond), limit, streamLinger)
	}
}

// BenchmarkActivityHooks times hooks sent from parallel writers, 70% of
// which record an activity, without streams and with 300 on their
// repository. The two should take about as long; with 300 streams, before
// frames and the linger, the hooks took up to ten times as long, and less
// only as streams fell behind and were dropped.
//
//	go test ./internal/server -run '^$' -bench ActivityHooks -cpu 4
func BenchmarkActivityHooks(b *testing.B) {
	for _, n := range []int{0, 300} {
		b.Run(fmt.Sprintf("streams=%d", n), func(b *testing.B) {
			f := newStreamLoad(b, 30, 10, 20)
			if n > 0 {
				_, stop := f.openStreams(b, n)
				defer stop()
			}
			var seed atomic.Uint64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				r := rand.New(rand.NewPCG(seed.Add(1), 7))
				for pb.Next() {
					f.hook(r, 0.7)
				}
			})
		})
	}
}

// BenchmarkHubPublish measures what handing one activity to the streams
// costs the board's writer, which waits for it while the next writer holds
// the board's lock: the same however many streams are open.
//
//	go test ./internal/server -run '^$' -bench HubPublish
func BenchmarkHubPublish(b *testing.B) {
	for _, n := range []int{0, 1, 300, 5000} {
		b.Run(fmt.Sprintf("streams=%d", n), func(b *testing.B) {
			h := newHub(StreamLimits{Total: max(n, 1), PerMember: max(n, 1)})
			for range n {
				h.subscribe(loadRepo, memberHash{name: "m"})
			}
			a := board.Activity{At: time.Now(), Kind: board.ActivityFileChanged, Repo: loadRepo, Member: "m", Paths: []string{"svc/f.go"}}
			for b.Loop() {
				a.Seq++
				h.publish([]board.Activity{a})
			}
		})
	}
}
