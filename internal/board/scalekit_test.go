package board

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

// The scale kit builds boards of a given shape, for benchmarks and for tests
// that count the work one call does on a large board. It builds through the
// board's own API (Hook, Declare, Sweep) on a clock that moves forward, so
// the board holds what a team would leave behind: files found by git at a
// few moments, files hooks saw changed one at a time, intents, inboxes and
// claims whose agents left. Filling the maps directly, or a frozen clock,
// made earlier measurements wrong: IDs collided, and a footprint whose files
// all share one time sorts faster than a real one.

// footprintDist says how many files the claims of a shape have changed.
type footprintDist int

const (
	filesFixed     footprintDist = iota // every claim: files
	filesMixed                          // files, and every 20th claim at the cap
	filesAtCap                          // every claim at the cap
	filesLognormal                      // median files, sigma 1.2, up to the cap: most small, a few large
)

// shape describes a board to build.
type shape struct {
	members   int // members; claims and sessions are spread over them
	repos     int // repositories; the first is the busy one
	claims    int // live claims, each in its own worktree with one session
	busy      int // of the live claims, those in the busy repository
	sessions  int // live sessions; those past claims join the busy repository's claims
	dormant   int // claims in the busy repository whose agents left more than DormantFor ago
	files     int // changed files per claim, as dist says
	dist      footprintDist
	areas     int     // areas per repository
	perArea   int     // files per area, in packages of 20
	intents   float64 // share of live claims that declare two intents
	exclusive float64 // share of those whose first intent is exclusive
	depth     int     // directories between a package and its files: the scale of path lengths
	seed      int64
}

// targetShape is the board intagent is meant for: 200 members with 1000 live
// sessions in 30 repositories, 300 claims in the busy one, whose 100k files
// are in 500 areas of 200.
func targetShape(dist footprintDist, files int) shape {
	return shape{members: 200, repos: 30, claims: 1000, busy: 300, sessions: 1000, files: files, dist: dist,
		areas: 500, perArea: 200, intents: 0.7, exclusive: 0.1, depth: 1, seed: 1}
}

// smallShape is a board small enough to build in every test run.
func smallShape() shape {
	return shape{members: 8, repos: 3, claims: 24, busy: 12, sessions: 30, dormant: 4, files: 12, dist: filesMixed,
		areas: 20, perArea: 40, intents: 0.5, exclusive: 0.3, depth: 1, seed: 1}
}

// scaleBoard is a built board, with its clock and IDs, to go on driving it.
type scaleBoard struct {
	*Board
	sh  shape
	now time.Time
	ids int // IDs handed out so far
}

const scaleStep = 10 * time.Millisecond // between two events while building

func newScaleBoard(sh shape, cfg Config, now time.Time, ids int) *scaleBoard {
	sb := &scaleBoard{sh: sh, now: now, ids: ids}
	sb.Board = New(cfg, withIDs(func(prefix string) string { sb.ids++; return fmt.Sprintf("%s%07d", prefix, sb.ids) }))
	return sb
}

// buildBoard builds a board of the shape with the default configuration.
//
// It builds with reservations off, and copies the result into a board with
// the default policy: until the intents are declared, last, no claim holds a
// reservation, so the two boards are the same, and the build skips the work
// reservations cost on every reconcile.
func buildBoard(tb testing.TB, sh shape) *scaleBoard {
	tb.Helper()
	cfg := DefaultConfig()
	cfg.Policy.Block = ActionOff
	sb := newScaleBoard(sh, cfg, t0, 0)
	sb.build(tb)
	return sb.forkWith(tb, DefaultConfig())
}

func (sb *scaleBoard) build(tb testing.TB) {
	tb.Helper()
	sh := sb.sh
	// The claims whose agents left first, so that they are dormant when the
	// live ones arrive.
	for d := range sh.dormant {
		ev := sb.dormantEvent(d, KindHeartbeat)
		ev.Footprint = sb.footprint(sh.claims+d, sb.filesOf(sh.claims+d))
		sb.hook(tb, ev)
		sb.hook(tb, sb.dormantEvent(d, KindSessionEnd))
	}
	if sh.dormant > 0 {
		sb.now = sb.now.Add(sb.cfg.DormantFor + time.Hour)
		sb.Sweep(sb.now)
	}
	for i := range sh.claims {
		sb.startClaim(tb, i)
	}
	// Intents last, once every footprint they meet is in place.
	for i := range sh.claims {
		if r := sb.rngOf(i, 1); r.Float64() < sh.intents {
			sb.declareFor(tb, i, r)
		}
	}
	for j := sh.claims; j < sh.sessions; j++ {
		i := (j - sh.claims) % max(1, sh.busy)
		ev := sb.event(i, KindHeartbeat)
		ev.SessionID = fmt.Sprintf("s%05d", j)
		sb.hook(tb, ev)
		ev.Kind, ev.Prompt = KindPrompt, "Help with "+sb.where(i).Branch
		sb.hook(tb, ev)
	}
	sb.tick()
	sb.Sweep(sb.now)
}

// startClaim plays a session's start in claim i's worktree: git finds half
// the changed files, the agent edits a few one at a time, and a shell
// command's end finds the rest. The first event is a heartbeat, which
// changes the board as a session_start does but leaves out the greeting:
// nothing reads it, and on a large board it takes most of the build.
func (sb *scaleBoard) startClaim(tb testing.TB, i int) {
	fp := sb.footprint(i, sb.filesOf(i))
	half := len(fp.Files) / 2
	ev := sb.event(i, KindHeartbeat)
	ev.Footprint = &Footprint{Files: fp.Files[:half]}
	sb.hook(tb, ev)
	ev = sb.event(i, KindPrompt)
	ev.Prompt = fmt.Sprintf("Rework %s for ticket %d", areaName(sb.baseArea(i)), 1000+i)
	sb.hook(tb, ev)
	for k := half; k < min(half+3, len(fp.Files)); k++ {
		ev := sb.event(i, KindPostEdit, fp.Files[k])
		ev.ToolUseID = fmt.Sprintf("e%d", k)
		sb.hook(tb, ev)
	}
	ev = sb.event(i, KindToolEnd)
	ev.Tool, ev.Footprint = "Bash", fp
	sb.hook(tb, ev)
}

// declareFor declares two intents for claim i: a package, two files, or a
// wildcard in its first area, then the next package. The first may be
// exclusive, unless it clashes with a teammate's reservation, which the
// board then refuses.
func (sb *scaleBoard) declareFor(tb testing.TB, i int, r *rand.Rand) {
	tb.Helper()
	a, pkg := sb.baseArea(i), r.Intn(max(1, sb.sh.perArea/20))
	var first []string
	switch k := r.Float64(); {
	case k < 0.6:
		first = []string{fmt.Sprintf("%s/pkg%02d/**", areaName(a), pkg)}
	case k < 0.9:
		first = []string{sb.path(a, pkg*20+r.Intn(20)).Path, sb.path(a, pkg*20+r.Intn(20)).Path}
	default:
		first = []string{fmt.Sprintf("%s/**/file1*.go", areaName(a))}
	}
	mode := ModeShared
	if r.Float64() < sb.sh.exclusive {
		mode = ModeExclusive
	}
	next := []string{fmt.Sprintf("%s/pkg%02d/**", areaName(a), (pkg+1)%max(1, sb.sh.perArea/20))}
	for _, d := range []DeclareRequest{
		{Where: sb.where(i), Summary: fmt.Sprintf("Move %s into its own package", first[0]), Patterns: first, Mode: mode},
		{Where: sb.where(i), Summary: "Then its neighbour", Patterns: next, Mode: ModeShared},
	} {
		d.Member = sb.member(i)
		sb.tick()
		if _, err := sb.Declare(sb.now, d); err != nil {
			tb.Fatalf("declare for claim %d: %v", i, err)
		}
	}
}

func (sb *scaleBoard) tick() { sb.now = sb.now.Add(scaleStep) }

// hook sends an event a step after the last one.
func (sb *scaleBoard) hook(tb testing.TB, ev HookEvent) HookResult {
	tb.Helper()
	sb.tick()
	res, err := sb.Hook(sb.now, ev)
	if err != nil {
		tb.Fatalf("%s for %s: %v", ev.Kind, ev.Where.Worktree, err)
	}
	return res
}

// fork copies the board, through a snapshot, into one that goes on numbering
// IDs where this one left off, so a benchmark can start each round afresh.
func (sb *scaleBoard) fork(tb testing.TB) *scaleBoard { return sb.forkWith(tb, sb.cfg) }

// forkWith is fork with another configuration.
func (sb *scaleBoard) forkWith(tb testing.TB, cfg Config) *scaleBoard {
	tb.Helper()
	data, _, err := sb.Snapshot(sb.now)
	if err != nil {
		tb.Fatal(err)
	}
	fb := newScaleBoard(sb.sh, cfg, sb.now, sb.ids)
	if err := fb.Restore(data); err != nil {
		tb.Fatal(err)
	}
	return fb
}

// --- names ---------------------------------------------------------------

func scaleRepo(r int) string { return fmt.Sprintf("github.com/acme/repo%02d", r) }

func areaName(a int) string { return fmt.Sprintf("services/svc%03d", a) }

func (sb *scaleBoard) member(i int) string { return fmt.Sprintf("m%03d", i%sb.sh.members) }

// repoOf puts the first busy live claims in the busy repository and spreads
// the others; claims past the live ones are the dormant ones.
func (sb *scaleBoard) repoOf(i int) string {
	if i < sb.sh.busy || i >= sb.sh.claims || sb.sh.repos == 1 {
		return scaleRepo(0)
	}
	return scaleRepo(1 + (i-sb.sh.busy)%(sb.sh.repos-1))
}

func (sb *scaleBoard) where(i int) Where {
	m := sb.member(i)
	return Where{Repo: sb.repoOf(i), Host: m + "-laptop", Worktree: fmt.Sprintf("/work/%s/wt%05d", m, i), Branch: fmt.Sprintf("feat/c%05d", i)}
}

// event is a hook event from live claim i's first session.
func (sb *scaleBoard) event(i int, kind Kind, paths ...PathRef) HookEvent {
	ev := HookEvent{Kind: kind, Member: sb.member(i), Agent: AgentClaudeCode, SessionID: fmt.Sprintf("s%05d", i), Where: sb.where(i), Paths: paths}
	if kind == KindPreEdit || kind == KindPostEdit {
		ev.Tool = "Edit"
	}
	return ev
}

func (sb *scaleBoard) dormantEvent(d int, kind Kind) HookEvent {
	return sb.event(sb.sh.claims+d, kind)
}

// fresh is an event from a new worktree k in repository r: a claim the board
// has not seen.
func (sb *scaleBoard) fresh(k, r int, kind Kind) HookEvent {
	m := sb.member(k)
	return HookEvent{Kind: kind, Member: m, Agent: AgentClaudeCode, SessionID: fmt.Sprintf("n%05d", k),
		Where: Where{Repo: scaleRepo(r), Host: m + "-laptop", Worktree: fmt.Sprintf("/work/%s/new%05d", m, k), Branch: fmt.Sprintf("feat/n%05d", k)}}
}

// path is file f of area a: in package f/20, under depth more directories.
func (sb *scaleBoard) path(a, f int) PathRef {
	area := areaName(a)
	dir := fmt.Sprintf("%s/pkg%02d/%s", area, f/20, strings.Repeat("internal/", sb.sh.depth))
	return PathRef{Path: fmt.Sprintf("%sfile%03d.go", dir, f), Area: area}
}

// --- footprints ----------------------------------------------------------

func (sb *scaleBoard) rngOf(i, salt int) *rand.Rand {
	return rand.New(rand.NewSource(sb.sh.seed*1_000_003 + int64(i)*7919 + int64(salt)))
}

// filesOf is how many files claim i has changed.
func (sb *scaleBoard) filesOf(i int) int {
	// A claim cannot change more files than the repository has.
	limit := min(DefaultConfig().MaxFootprint, sb.sh.areas*sb.sh.perArea)
	switch sb.sh.dist {
	case filesMixed:
		if i%20 == 0 {
			return limit
		}
	case filesAtCap:
		return limit
	case filesLognormal:
		n := float64(sb.sh.files) * math.Exp(1.2*sb.rngOf(i, 2).NormFloat64())
		return min(limit, int(n))
	}
	return min(limit, sb.sh.files)
}

func (sb *scaleBoard) baseArea(i int) int { return (i * 17) % sb.sh.areas }

// footprint is n changed files of claim i: 40 to an area, from its base area
// on, at random within each, so that neighbours share areas and some files.
func (sb *scaleBoard) footprint(i, n int) *Footprint {
	r := sb.rngOf(i, 3)
	fp := &Footprint{Files: make([]PathRef, 0, n)}
	seen := make(map[string]bool, n)
	for k := 0; len(fp.Files) < n; k++ {
		p := sb.path((sb.baseArea(i)+k/40)%sb.sh.areas, r.Intn(sb.sh.perArea))
		if !seen[p.Path] {
			seen[p.Path] = true
			fp.Files = append(fp.Files, p)
		}
	}
	return fp
}

// --- the kit's own test --------------------------------------------------

// The builder makes the board its shape describes.
func TestScaleKitBuildsTheShape(t *testing.T) {
	sh := smallShape()
	sb := buildBoard(t, sh)
	live := sb.oldLiveClaims(sb.now)
	perRepo, dormant, liveSessions, intents := map[string]int{}, 0, 0, 0
	for _, c := range sb.claims {
		perRepo[c.Repo]++
		if !live[c.ID] && sb.now.Sub(c.UpdatedAt) > sb.cfg.DormantFor {
			dormant++
		}
		intents += len(c.Intents)
	}
	for _, s := range sb.sessions {
		if sb.state(sb.now, s).Live() {
			liveSessions++
		}
	}
	if len(sb.claims) != sh.claims+sh.dormant || dormant != sh.dormant || len(live) != sh.claims || liveSessions != sh.sessions {
		t.Fatalf("claims %d (want %d), dormant %d (want %d), live claims %d (want %d), live sessions %d (want %d)",
			len(sb.claims), sh.claims+sh.dormant, dormant, sh.dormant, len(live), sh.claims, liveSessions, sh.sessions)
	}
	if got := perRepo[scaleRepo(0)]; got != sh.busy+sh.dormant {
		t.Errorf("busy repository has %d claims, want %d", got, sh.busy+sh.dormant)
	}
	if intents == 0 {
		t.Error("no claim declared an intent")
	}
	for i := range sh.claims {
		c := sb.findClaim(sb.member(i), sb.where(i))
		if c == nil {
			t.Fatalf("claim %d is missing", i)
		}
		if got, want := len(c.Footprint), sb.filesOf(i); got != want {
			t.Errorf("claim %d has %d files, want %d", i, got, want)
		}
		// Files arrive at several moments, as they do from git and hooks.
		var times []time.Time
		for _, f := range c.Footprint {
			times = append(times, f.At)
		}
		slices.SortFunc(times, time.Time.Compare)
		if len(c.Footprint) > 4 && len(slices.Compact(times)) < 3 {
			t.Errorf("claim %d's %d files carry %d distinct times", i, len(c.Footprint), len(slices.Compact(times)))
		}
	}
	// A fork goes on numbering where the board left off: a new claim must not
	// replace an existing one.
	fb := sb.fork(t)
	before := len(fb.claims)
	fb.hook(t, fb.fresh(1, 0, KindSessionStart))
	if len(fb.claims) != before+1 {
		t.Fatalf("a new worktree after a fork left %d claims, want %d", len(fb.claims), before+1)
	}
}
