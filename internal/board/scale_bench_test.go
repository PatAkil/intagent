package board

import (
	"fmt"
	"sync"
	"testing"
	"time"
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

// One pre_edit of one file in the busy repository.
func BenchmarkScalePreEdit(b *testing.B) {
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"typical", targetShape(filesLognormal, 23)},
		{"mixed", targetShape(filesMixed, 50)},
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
// announced them, and their sessions are kept an hour more.
func BenchmarkScaleSweepQuiet(b *testing.B) {
	for _, n := range []int{2000, 20000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			sb := newScaleBoard(targetShape(filesFixed, 0), DefaultConfig(), t0, 0)
			for k := range n {
				sb.hook(b, sb.fresh(k, 1+k%29, KindHeartbeat))
			}
			sb.now = sb.now.Add(sb.cfg.IdleAfter + time.Minute)
			sb.Sweep(sb.now)
			sb.now = sb.now.Add(15 * time.Second)
			b.ResetTimer()
			for range b.N {
				sb.Sweep(sb.now)
			}
		})
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

// The busy repository's view, which every dashboard reloads, by the old
// View in oracle_test.go and the current one.
func BenchmarkScaleView(b *testing.B) {
	capped := targetShape(filesAtCap, 0)
	capped.claims, capped.busy, capped.sessions, capped.repos, capped.intents = 300, 300, 300, 1, 0
	for _, tc := range []struct {
		name string
		sh   shape
	}{
		{"mixed", targetShape(filesMixed, 50)},
		{"cap", capped},
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
