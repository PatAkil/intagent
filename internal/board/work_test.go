package board

import (
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
)

// Tests that count the work a call does on a large board, rather than time
// it: counts hold on a busy machine. Each fails on the implementation it
// replaced, which oracle_test.go keeps.

// A reconcile reports breaches by checking its new files against the
// exclusive intents of the claims that hold reservations, and nothing else:
// not every teammate's files and areas, however many there are.
func TestReconcileComparesOnlyReservations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exclusive float64
	}{
		{"nobody holds a reservation", 0},
		{"teammates hold reservations", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := smallShape()
			sh.intents, sh.exclusive = 1, tc.exclusive
			sb := buildBoard(t, sh)
			live, holders := sb.liveClaims(sb.now), 0
			for _, c := range sb.claimsInRepo(scaleRepo(0)) {
				if live[c.ID] && slices.ContainsFunc(c.Intents, func(in Intent) bool { return in.Mode == ModeExclusive }) {
					holders++
				}
			}
			if (holders > 0) != (tc.exclusive > 0) {
				t.Fatalf("%d claims hold reservations", holders)
			}
			ev := sb.fresh(1, 0, KindSessionStart)
			ev.Footprint = sb.footprint(1, 200)
			trace = &workTrace{}
			t.Cleanup(func() { trace = nil })
			sb.hook(t, ev)
			if trace.conflictsFor != 0 || trace.conflictWith != 0 || trace.workedInArea != 0 {
				t.Fatalf("a reconcile of %d new files made %d conflict checks against %d claims and %d area scans",
					len(ev.Footprint.Files), trace.conflictsFor, trace.conflictWith, trace.workedInArea)
			}
		})
	}
}

// A greeting ranks each claim in the repository once, however many there
// are, and keeps the rows it shows.
func TestGreetingRanksEachClaimOnce(t *testing.T) {
	sh := smallShape()
	sh.claims, sh.busy, sh.sessions, sh.dormant = 300, 280, 300, 20
	sh.files, sh.dist, sh.intents = 6, filesFixed, 0.2
	sb := buildBoard(t, sh)
	c := sb.findClaim(sb.member(3), sb.where(3))
	trace = &workTrace{}
	t.Cleanup(func() { trace = nil })
	sb.renderStart(sb.now, c)
	// The old greeting ranked two claims for every comparison its sort made:
	// 5720 rankings on this board.
	if others := sh.busy + sh.dormant - 1; trace.ranked != others {
		t.Fatalf("a greeting among %d other claims ranked claims %d times", others, trace.ranked)
	}
}

// The feed holds the last KeepActivities activities, in order, whatever its
// length and however many have been recorded. Every record checks the
// feed's length and its two ends; the whole feed is compared after the
// first few records, as it fills, every 997th record and the last.
func TestFeedKeepsTheLastActivities(t *testing.T) {
	t.Parallel()
	const records = 200_000
	for _, keep := range []int{1, 7, 300, 3000} {
		b := New(Config{KeepActivities: keep})
		for n := 1; n <= records; n++ {
			b.record(Activity{Kind: ActivityFileChanged, Repo: repo, Text: strconv.Itoa(n), Paths: []string{"a.go"}})
			b.pending = b.pending[:0]
			first := max(1, n-keep+1)
			if len(b.recent) != n-first+1 {
				t.Fatalf("keep %d, %d recorded: the feed holds %d", keep, n, len(b.recent))
			}
			if lo, hi := b.recent[0].Seq, b.recent[len(b.recent)-1].Seq; lo != uint64(first) || hi != uint64(n) {
				t.Fatalf("keep %d, %d recorded: the feed runs from seq %d to %d, want %d to %d", keep, n, lo, hi, first, n)
			}
			if n > 8 && n != keep && n != keep+1 && n%997 != 0 && n != records {
				continue
			}
			for i, a := range b.recent {
				if want := first + i; a.Seq != uint64(want) || a.Text != strconv.Itoa(want) {
					t.Fatalf("keep %d, %d recorded: entry %d is seq %d %q, want %d", keep, n, i, a.Seq, a.Text, want)
				}
			}
		}
	}
}

// Recording an activity into a full feed does not copy the feed: append
// moves it to a new array about once per KeepActivities records, so a record
// averages well under 0.05 allocations and 1 KB. The old record copied all
// 300 activities, 74 KB, every time. Counted over many records, since
// testing.AllocsPerRun rounds down and would pass a record that copied the
// feed every other time.
func TestRecordDoesNotCopyTheFeed(t *testing.T) {
	b := New(DefaultConfig())
	a := Activity{Kind: ActivityFileChanged, Repo: repo, Paths: []string{"a.go"}}
	for range b.cfg.KeepActivities {
		b.record(a)
	}
	const records = 20_000
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range records {
		b.record(a)
		b.pending = b.pending[:0]
	}
	runtime.ReadMemStats(&after)
	allocs := float64(after.Mallocs-before.Mallocs) / records
	bytes := float64(after.TotalAlloc-before.TotalAlloc) / records
	if allocs >= 0.05 || bytes >= 1024 {
		t.Fatalf("recording into a full feed of %d made %.3f allocations and %.0f bytes per activity",
			b.cfg.KeepActivities, allocs, bytes)
	}
}

// A sweep reads each session once, however many worktrees have gone quiet:
// it does not search the sessions again for each claim.
func TestSweepReadsEachSessionOnce(t *testing.T) {
	sb := newScaleBoard(smallShape(), DefaultConfig(), t0, 0)
	const quiet = 2000
	for k := range quiet {
		sb.hook(t, sb.fresh(k, k%3, KindHeartbeat))
	}
	for _, d := range []time.Duration{sb.cfg.IdleAfter + time.Minute, 15 * time.Second} {
		sb.now = sb.now.Add(d)
		trace = &workTrace{}
		sb.Sweep(sb.now)
		visits := trace.sessionVisits
		trace = nil
		// The old sweep read sessions 2,028,882 times here: up to all 2000
		// for each claim it searched.
		if visits > quiet {
			t.Fatalf("a sweep of %d quiet sessions read sessions %d times", quiet, visits)
		}
	}
	if len(sb.claims) != quiet || len(sb.sessions) != quiet {
		t.Fatalf("%d claims and %d sessions after the sweeps, want %d each", len(sb.claims), len(sb.sessions), quiet)
	}
}
