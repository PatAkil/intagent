package board

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/fsutil"
	"github.com/patakil/intagent/internal/glob"
)

// Benchmarks at the scale intagent is meant for, on boards from the scale
// kit. Building a target board takes seconds, so each shape is built once
// per run and every benchmark works on its own copy:
//
//	go test ./internal/board -run '^$' -bench Scale -benchmem

var (
	builtMu sync.Mutex
	built   = map[shape]*scaleBoard{}
)

// boardOf returns a copy of a board of the shape, building it on first use.
func boardOf(tb testing.TB, sh shape) *scaleBoard {
	tb.Helper()
	builtMu.Lock()
	defer builtMu.Unlock()
	sb := built[sh]
	if sb == nil {
		sb = buildBoard(tb, sh)
		built[sh] = sb
	}
	return sb.fork(tb)
}

// A worktree new to the busy repository whose footprint holds 2000 files,
// on a board of 300 claims there with 200 files each.
func BenchmarkScaleReconcileNewClaim(b *testing.B) {
	sb := boardOf(b, targetShape(filesFixed, 200))
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		ev := sb.fresh(i, 0, KindSessionStart)
		ev.Footprint = sb.footprint(100_000+i, 2000)
		sb.tick()
		b.StartTimer()
		if _, err := sb.Hook(sb.now, ev); err != nil {
			b.Fatal(err)
		}
	}
}

// A session starting in a busy-repository worktree with no new files: the
// greeting ranks the repository's 300 claims, of about 140 files each with a
// heavy tail.
func BenchmarkScaleSessionStart(b *testing.B) {
	sb := boardOf(b, targetShape(filesLognormal, 68))
	b.ResetTimer()
	for i := range b.N {
		sb.hook(b, sb.event(i%sb.sh.busy, KindSessionStart))
	}
}

// 500 agents start within seconds in new worktrees, 150 of them in the busy
// repository, on a board half full: the sum is the time they hold the lock.
func BenchmarkScaleStartStorm(b *testing.B) {
	sh := targetShape(filesLognormal, 68)
	sh.claims, sh.busy, sh.sessions = 500, 150, 500
	base := boardOf(b, sh)
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		sb := base.fork(b)
		b.StartTimer()
		for k := range 500 {
			r := 0
			if k >= 150 {
				r = 1 + k%(sh.repos-1)
			}
			sb.hook(b, sb.fresh(k, r, KindSessionStart))
		}
	}
}

// cappedShape is the busy repository alone, its 300 claims at the cap.
func cappedShape() shape {
	sh := targetShape(filesAtCap, 0)
	sh.claims, sh.busy, sh.sessions, sh.repos, sh.intents = 300, 300, 300, 1, 0
	return sh
}

// One pre_edit of one file in the busy repository.
func BenchmarkScalePreEdit(b *testing.B) {
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"typical", targetShape(filesLognormal, 23)},
		{"mixed", targetShape(filesMixed, 50)},
		{"cap", cappedShape()},
	} {
		b.Run(tc.name, func(b *testing.B) {
			sb := boardOf(b, tc.sh)
			ev := sb.event(1, KindPreEdit, sb.path(sb.baseArea(1)+3, 7))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				sb.hook(b, ev)
			}
		})
	}
}

// A sweep once n agents, each in its own worktree, have gone quiet: it
// announced them, and their sessions are kept an hour more. By the old
// Sweep in oracle_test.go and the current one.
func BenchmarkScaleSweepQuiet(b *testing.B) {
	for _, n := range []int{2000, 20000} {
		sb := newScaleBoard(targetShape(filesFixed, 0), DefaultConfig(), t0, 0)
		for k := range n {
			sb.hook(b, sb.fresh(k, 1+k%29, KindHeartbeat))
		}
		sb.now = sb.now.Add(sb.cfg.IdleAfter + time.Minute)
		sb.Sweep(sb.now)
		sb.now = sb.now.Add(15 * time.Second)
		for _, v := range []struct {
			name  string
			sweep func(time.Time)
		}{{"old", sb.oldSweep}, {"new", sb.Sweep}} {
			b.Run(fmt.Sprintf("%d/%s", n, v.name), func(b *testing.B) {
				for range b.N {
					v.sweep(sb.now)
				}
			})
		}
	}
}

// Recording one activity once the feed is full, by the old record in
// oracle_test.go and the current one.
func BenchmarkScaleRecord(b *testing.B) {
	for _, keep := range []int{300, 3000} {
		for _, version := range []string{"old", "new"} {
			b.Run(fmt.Sprintf("%d/%s", keep, version), func(b *testing.B) {
				bd := New(Config{KeepActivities: keep})
				record := bd.record
				if version == "old" {
					record = bd.oldRecord
				}
				a := Activity{At: t0, Kind: ActivityFileChanged, Repo: "github.com/acme/mono", Member: "m", Paths: []string{"a/b.go"}}
				for range keep {
					record(a)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					record(a)
					bd.pending = bd.pending[:0]
				}
			})
		}
	}
}

// What a pre_edit or a check of 1 and of 200 files in the busy repository
// finds, by the old walk of every claim, footprint and session in
// oracle_test.go and through the indexes.
func BenchmarkScaleCheck(b *testing.B) {
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"mixed", targetShape(filesMixed, 50)},
		{"cap", cappedShape()},
	} {
		sb := boardOf(b, tc.sh)
		c := sb.findClaim(sb.member(1), sb.where(1))
		s := sb.sessions[sessionKey(sb.member(1), AgentClaudeCode, "s00001")]
		for _, n := range []int{1, 200} {
			var paths []PathRef
			for k := range n {
				paths = append(paths, sb.path(sb.baseArea(1)+k%7, k))
			}
			for _, v := range []struct {
				name  string
				check func(time.Time, *claim, *session, []PathRef) []Conflict
			}{{"old", sb.oldCheck}, {"new", sb.newCheck}} {
				b.Run(fmt.Sprintf("%s/%d/%s", tc.name, n, v.name), func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						v.check(sb.now, c, s, paths)
					}
				})
			}
		}
	}
}

// The overlaps 50 newly declared patterns meet in the busy repository at
// the cap, by the old match of every file in oracle_test.go and through
// each claim's ordered paths.
func BenchmarkScaleDeclareOverlaps(b *testing.B) {
	sb := boardOf(b, cappedShape())
	c := sb.findClaim(sb.member(1), sb.where(1))
	var intents []Intent
	for k := range 50 {
		pat := fmt.Sprintf("%s/pkg%02d/**", areaName(sb.baseArea(1)+k%9), k%10)
		if k%5 == 4 {
			pat = fmt.Sprintf("%s/**/file1*.go", areaName(sb.baseArea(1)+k))
		}
		intents = append(intents, Intent{Pattern: pat, Mode: ModeShared, DeclaredAt: sb.now})
	}
	for _, v := range []struct {
		name     string
		overlaps func(in Intent) []Conflict
	}{
		{"old", func(in Intent) []Conflict { return sb.oldIntentOverlaps(c, in, sb.oldLiveClaims(sb.now)) }},
		{"new", func(in Intent) []Conflict { return sb.intentOverlaps(c, in, sb.liveAt(sb.now)) }},
	} {
		b.Run(v.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				for _, in := range intents {
					v.overlaps(in)
				}
			}
		})
	}
}

// The busy repository's view, which every dashboard reloads, by the old
// View in oracle_test.go and the current one.
func BenchmarkScaleView(b *testing.B) {
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"mixed", targetShape(filesMixed, 50)},
		{"cap", cappedShape()},
	} {
		sb := boardOf(b, tc.sh)
		for _, v := range []struct {
			name string
			view func(time.Time, string) View
		}{{"old", sb.oldView}, {"new", sb.View}} {
			b.Run(tc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					v.view(sb.now, scaleRepo(0))
				}
			})
		}
	}
}

// How long a view of the busy repository holds the board's lock: the old
// View, in oracle_test.go, held it throughout; View now holds it while it
// copies (viewCopy).
func BenchmarkScaleViewLock(b *testing.B) {
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"mixed", targetShape(filesMixed, 50)},
		{"cap", cappedShape()},
	} {
		sb := boardOf(b, tc.sh)
		for _, v := range []struct {
			name   string
			locked func()
		}{
			{"old", func() { sb.oldView(sb.now, scaleRepo(0)) }},
			{"new", func() { sb.viewCopy(sb.now, scaleRepo(0)) }},
		} {
			b.Run(tc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					v.locked()
				}
			})
		}
	}
}

// One member's 4 worktrees of 50 patterns each at glob.CleanPattern's
// bounds, and what they cost others with the bound on matching one call may
// do and without it: a check of 200 paths 63 segments deep against chains of
// ** and *, and against a ** and 30 literal segments, and a declaration of 50
// patterns against patterns of 64-byte wildcard segments.
func BenchmarkScaleCostlyPatterns(b *testing.B) {
	hostile := func(b *testing.B, patterns func(int) []string) *Board {
		bd := New(DefaultConfig())
		for k := range 4 {
			w := Where{Repo: repo, Host: "h", Worktree: fmt.Sprintf("/work/eve%d", k)}
			if _, err := bd.Declare(t0, DeclareRequest{Member: "eve", Where: w, Patterns: patterns(k)}); err != nil {
				b.Fatal(err)
			}
		}
		return bd
	}
	bob := Where{Repo: repo, Host: "h", Worktree: "/work/bob"}
	for _, tc := range []struct {
		name     string
		patterns func(int) []string
		dir      string
	}{{"chains", chainPatterns, "d"}, {"ladders", ladderPatterns, "a"}} {
		bd := hostile(b, tc.patterns)
		paths := refs(deepPaths(maxCheckPaths, tc.dir)...)
		b.Run("check/"+tc.name+"/bounded", func(b *testing.B) {
			for range b.N {
				if _, err := bd.Check(t0, CheckRequest{Member: "bob", Where: bob, Paths: paths}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("check/"+tc.name+"/unbounded", func(b *testing.B) {
			for range b.N {
				unboundedWork(bd, "bob", paths)
			}
		})
	}
	wide := hostile(b, widePatterns)
	declare := DeclareRequest{Member: "bob", Where: bob, Patterns: widePatterns(9)}
	b.Run("declare/bounded", func(b *testing.B) {
		for range b.N {
			if _, err := wide.Declare(t0, declare); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("declare/unbounded", func(b *testing.B) {
		c := wide.findClaim("bob", bob)
		for range b.N {
			wide.mu.Lock()
			wide.work = glob.NewWork(1 << 50)
			for _, p := range declare.Patterns {
				wide.intentOverlaps(c, Intent{Pattern: p}, wide.liveAt(t0))
			}
			wide.mu.Unlock()
		}
	})
}

// How long a snapshot holds the board's lock: the old one, in
// oracle_test.go, copied every footprint; Snapshot shares them. "all-cap"
// is 1000 claims at the cap, 2M files.
func BenchmarkScaleSnapshotLock(b *testing.B) {
	allCap := targetShape(filesAtCap, 0)
	allCap.intents = 0
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"mixed", targetShape(filesMixed, 50)},
		{"cap", cappedShape()},
		{"all-cap", allCap},
	} {
		sb := boardOf(b, tc.sh)
		for _, v := range []struct {
			name   string
			locked func()
		}{
			{"old", func() { sb.oldSnapshotCopy(sb.now) }},
			{"new", func() { sb.snapshotCopy(sb.now) }},
		} {
			b.Run(tc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					v.locked()
				}
			})
		}
	}
}

// A whole save to a file: the old one marshalled the snapshot whole, in
// oracle_test.go; WriteSnapshot streams it. peak-MB is the most heap in use
// above the board's while it saves, sampled every millisecond. "dormant" is
// a week of per-task worktrees: 1000 live claims and 10,000 dormant ones of
// 50 files, 550k in all.
func BenchmarkScaleSnapshotWrite(b *testing.B) {
	dormant := targetShape(filesFixed, 50)
	dormant.dormant = 10_000
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"mixed", targetShape(filesMixed, 50)},
		{"dormant", dormant},
		{"cap", cappedShape()},
	} {
		sb := boardOf(b, tc.sh)
		path := filepath.Join(b.TempDir(), "board.json")
		for _, v := range []struct {
			name string
			save func() error
		}{
			{"old", func() error {
				data, _, err := sb.oldSnapshot(sb.now)
				if err != nil {
					return err
				}
				return fsutil.WriteFile(path, data, 0o600)
			}},
			{"new", func() error {
				return fsutil.WriteFileFunc(path, 0o600, func(w io.Writer) error {
					_, err := sb.WriteSnapshot(w, sb.now)
					return err
				})
			}},
		} {
			b.Run(tc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				var peak uint64
				for range b.N {
					runtime.GC()
					p, err := peakHeap(v.save)
					if err != nil {
						b.Fatal(err)
					}
					peak = max(peak, p)
				}
				b.ReportMetric(float64(peak)/(1<<20), "peak-MB")
				if fi, err := os.Stat(path); err == nil {
					b.ReportMetric(float64(fi.Size())/(1<<20), "file-MB")
				}
			})
		}
	}
}

// peakHeap runs f and returns the most heap in use above what was in use
// when it started, sampled every millisecond.
func peakHeap(f func() error) (uint64, error) {
	read := func() uint64 {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/heap/unused:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64() + s[1].Value.Uint64()
	}
	base := read()
	var peak atomic.Uint64
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			if h := read(); h > base && h-base > peak.Load() {
				peak.Store(h - base)
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	err := f()
	close(done)
	<-sampled
	return peak.Load(), err
}

// A restore from a file: read whole first, as Restore did, or decoded as it
// is read. peak-MB is the most heap in use while it restores, sampled every
// millisecond, the board it builds included.
func BenchmarkScaleRestore(b *testing.B) {
	dormant := targetShape(filesFixed, 50)
	dormant.dormant = 10_000
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"dormant", dormant},
		{"cap", cappedShape()},
	} {
		sb := boardOf(b, tc.sh)
		path := filepath.Join(b.TempDir(), "board.json")
		if err := fsutil.WriteFileFunc(path, 0o600, func(w io.Writer) error {
			_, err := sb.WriteSnapshot(w, sb.now)
			return err
		}); err != nil {
			b.Fatal(err)
		}
		now := sb.now
		sb = nil
		for _, v := range []struct {
			name    string
			restore func() error
		}{
			{"whole", func() error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				return New(DefaultConfig()).Restore(bytes.NewReader(data), now)
			}},
			{"streamed", func() error {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				return New(DefaultConfig()).Restore(f, now)
			}},
		} {
			b.Run(tc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				var peak uint64
				for range b.N {
					runtime.GC()
					p, err := peakHeap(v.restore)
					if err != nil {
						b.Fatal(err)
					}
					peak = max(peak, p)
				}
				b.ReportMetric(float64(peak)/(1<<20), "peak-MB")
			})
		}
	}
}
