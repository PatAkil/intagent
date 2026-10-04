package board

import (
	"fmt"
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
			live, holders := sb.oldLiveClaims(sb.now), 0
			for c := range sb.claimsIn(scaleRepo(0)) {
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

// counted runs fn with the work counter set, and returns what it counted.
func counted(t *testing.T, fn func()) workTrace {
	t.Helper()
	trace = &workTrace{}
	defer func() { trace = nil }()
	fn()
	return *trace
}

// sameWork reports whether two calls did the same work, but for the
// sessions they read: liveness reads a claim's sessions until it finds a
// live one, and when the claim has live and ended ones, which it reads first
// depends on the order of a map. Tests bound those instead.
func sameWork(a, b workTrace) bool {
	a.sessionVisits, b.sessionVisits = 0, 0
	return a == b
}

// A pre_edit reads the claims of its own repository, and the sessions of
// those that matter to it: a board with 2000 more sessions, in other
// repositories, costs it the same work, and it reads no more than the
// sessions of its repository's claims for each path. The old check read
// every session on the board twice, to build maps of the live ones.
func TestPreEditIgnoresOtherRepositories(t *testing.T) {
	base := buildBoard(t, smallShape())
	// Each claim in the repository also has a session that ended, so that
	// telling whether it is live reads one session or two.
	for i := range base.sh.busy {
		ev := base.event(i, KindHeartbeat)
		ev.SessionID += "-ended"
		base.hook(t, ev)
		ev.Kind = KindSessionEnd
		base.hook(t, ev)
	}
	quiet, busy := base.fork(t), base.fork(t)
	for k := range 2000 {
		busy.hook(t, busy.fresh(k, 1+k%2, KindHeartbeat))
	}
	quiet.now = busy.now
	ev := base.event(1, KindPreEdit, base.path(base.baseArea(1), 7), base.path(base.baseArea(2), 3))
	var work [2]workTrace
	for i, sb := range []*scaleBoard{quiet, busy} {
		work[i] = counted(t, func() { sb.hook(t, ev) })
	}
	if !sameWork(work[0], work[1]) {
		t.Fatalf("a pre_edit's work grew with 2000 sessions in other repositories:\nwithout: %+v\n   with: %+v", work[0], work[1])
	}
	inRepo := 0
	for c := range busy.claimsIn(scaleRepo(0)) {
		inRepo += len(busy.claimSessions[c.ID])
	}
	for _, w := range work {
		if limit := len(ev.Paths) * (inRepo + 1); w.sessionVisits > limit {
			t.Fatalf("a pre_edit of %d paths read sessions %d times, more than the %d of its repository's claims for each",
				len(ev.Paths), w.sessionVisits, inRepo)
		}
	}
	if work[0].conflictWith == 0 || work[0].liveClaims == 0 {
		t.Fatalf("the pre_edit compared no claims: %+v", work[0])
	}
}

// addDormant puts n claims in a repository whose agents left long ago, with
// files in areas: claims the board keeps for a week.
func addDormant(b *Board, repo string, n, files int, at time.Time, areas []string) {
	for i := range n {
		c := &claim{ID: fmt.Sprintf("c_dormant%05d", i), Repo: repo, Member: fmt.Sprintf("d%03d", i%50), Host: "h",
			Worktree: fmt.Sprintf("/old/%05d", i), CreatedAt: at, UpdatedAt: at}
		b.addClaim(c)
		fp := map[string]*touch{}
		var paths []string
		for f := range files {
			area := areas[(i+f)%len(areas)]
			p := fmt.Sprintf("%s/old%05d_%d.go", area, i, f)
			fp[p] = &touch{Area: area, At: at, FromGit: true}
			paths = append(paths, p)
		}
		c.setFootprint(fp, paths)
	}
}

// A dormant claim costs a pre_edit one look, however many files it changed:
// whether it worked in the edit's area is a lookup, not a walk of its files.
// The old check walked every dormant claim's footprint for the area.
func TestDormantClaimsCostAPreEditOneLookEach(t *testing.T) {
	const dormant, files = 3000, 40
	base := buildBoard(t, smallShape())
	ev := base.event(1, KindPreEdit, base.path(base.baseArea(1), 7))
	var areas []string
	for a := range base.sh.areas {
		areas = append(areas, areaName(a))
	}
	without, with := base.fork(t), base.fork(t)
	addDormant(with.Board, scaleRepo(0), dormant, files, t0.Add(-30*24*time.Hour), areas)
	mustIndex(t, with.Board, "adding dormant claims")
	w0 := counted(t, func() { without.hook(t, ev) })
	w1 := counted(t, func() { with.hook(t, ev) })
	for _, c := range []struct {
		name       string
		got, under int
	}{
		{"claims compared", w1.conflictWith, w0.conflictWith + dormant},
		{"claims asked for their area", w1.workedInArea, w0.workedInArea + dormant},
		{"claims asked whether live", w1.liveClaims, w0.liveClaims + dormant},
		{"sessions read", w1.sessionVisits, w0.sessionVisits + dormant},
		{"footprint entries read", w1.areaVisits, 0},
	} {
		if c.got > c.under {
			t.Errorf("%s: %d with %d dormant claims of %d files, at most %d", c.name, c.got, dormant, files, c.under)
		}
	}
	if w1.workedInArea < dormant {
		t.Fatalf("the pre_edit asked %d claims for their area: the dormant ones were not compared", w1.workedInArea)
	}
}

// A pre_edit allocates no more on a board with thousands of sessions than
// on a small one: liveness is asked of the claims that matter, not built
// into maps of every session. The old check allocated maps of every
// session: 219 KB on the target board.
func TestPreEditAllocationsAreBounded(t *testing.T) {
	sb := buildBoard(t, smallShape())
	for k := range 2000 {
		sb.hook(t, sb.fresh(k, k%3, KindHeartbeat))
	}
	ev := sb.event(1, KindPreEdit, sb.path(sb.baseArea(1), 7))
	sb.hook(t, ev)
	const runs = 200
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		sb.hook(t, ev)
	}
	runtime.ReadMemStats(&after)
	allocs := float64(after.Mallocs-before.Mallocs) / runs
	bytes := float64(after.TotalAlloc-before.TotalAlloc) / runs
	if allocs > 300 || bytes > 32<<10 {
		t.Fatalf("a pre_edit on a board of %d sessions made %.0f allocations of %.0f bytes", len(sb.sessions), allocs, bytes)
	}
}

// A declared intent is matched against the files under the directory it is
// rooted in, not every file of every teammate: thousands more files
// elsewhere cost a declaration nothing. The old declaration matched each of
// its patterns against every file of every other claim.
func TestDeclareMatchesOnlyFilesUnderItsDirectory(t *testing.T) {
	var matches []int
	for _, elsewhere := range []int{10, 2000} {
		h := newHarness(t)
		h.edit("bob", "b1", "a/p1/x.go", "a/p2/y.go")
		var far []string
		for i := range elsewhere {
			far = append(far, fmt.Sprintf("z/p%d/f%d.go", i%40, i))
		}
		h.edit("carol", "c1", far...)
		patterns := make([]string, 50)
		for i := range patterns {
			patterns[i] = fmt.Sprintf("a/p%d/**", i)
		}
		w := counted(t, func() {
			res := h.declare("alice", ModeShared, "", patterns...)
			if len(res.Overlaps) != 2 {
				t.Fatalf("the declaration met %d overlaps, want bob's 2", len(res.Overlaps))
			}
		})
		matches = append(matches, w.globMatch)
	}
	if matches[0] != matches[1] {
		t.Fatalf("a declaration of 50 patterns made %d matches with 10 files elsewhere and %d with 2000", matches[0], matches[1])
	}
}
