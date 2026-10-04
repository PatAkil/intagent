package board

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/glob"
)

// checkIndexes recomputes the board's indexes from its claims and sessions,
// by brute force, and reports the first way they differ.
func (b *Board) checkIndexes() error {
	want := map[string][]*claim{}
	for id, c := range b.claims {
		if c.ID != id || c.removed {
			return fmt.Errorf("claim %s is filed under %s, removed %t", c.ID, id, c.removed)
		}
		if b.claims[b.byKey[c.key()]] == nil {
			return fmt.Errorf("claim %s cannot be found by its key", id)
		}
		want[c.Repo] = append(want[c.Repo], c)
	}
	for k, id := range b.byKey {
		if c := b.claims[id]; c == nil || c.key() != k {
			return fmt.Errorf("key %s names claim %s, which is not there or has another key", k, id)
		}
	}
	for repo, r := range b.byRepo {
		var got []*claim
		removed := 0
		for i, c := range r.claims {
			if i > 0 && r.claims[i-1].ID > c.ID {
				return fmt.Errorf("%s's index is out of order at %d: %s before %s", repo, i, r.claims[i-1].ID, c.ID)
			}
			if c.removed {
				removed++
				continue
			}
			got = append(got, c)
		}
		if removed != r.removed || 2*removed > len(r.claims) {
			return fmt.Errorf("%s's index holds %d removed claims of %d and counts %d", repo, removed, len(r.claims), r.removed)
		}
		w := want[repo]
		slices.SortFunc(w, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
		if len(w) == 0 || !slices.Equal(got, w) {
			return fmt.Errorf("%s's index lists %d claims, the board has %d there", repo, len(got), len(w))
		}
	}
	for repo := range want {
		if b.byRepo[repo] == nil {
			return fmt.Errorf("%s has claims and no index", repo)
		}
	}
	listed := 0
	for id, m := range b.claimSessions {
		if len(m) == 0 {
			return fmt.Errorf("claim %s keeps an empty set of sessions", id)
		}
		for k, s := range m {
			if b.sessions[k] != s || s.ClaimID != id {
				return fmt.Errorf("session %s is listed under claim %s, but is of %s or not on the board", k, id, s.ClaimID)
			}
			listed++
		}
	}
	for k, s := range b.sessions {
		if s.Key != k || b.claimSessions[s.ClaimID][k] != s {
			return fmt.Errorf("session %s of claim %s is not listed under it", k, s.ClaimID)
		}
	}
	if listed != len(b.sessions) {
		return fmt.Errorf("%d sessions listed under claims, %d on the board", listed, len(b.sessions))
	}
	for id, c := range b.claims {
		if err := c.checkFootprintIndex(); err != nil {
			return fmt.Errorf("claim %s: %w", id, err)
		}
	}
	return nil
}

// checkFootprintIndex compares a claim's paths in order and its latest
// change in each area with its footprint.
func (c *claim) checkFootprintIndex() error {
	if len(c.sortedPaths) != len(c.Footprint) {
		return fmt.Errorf("%d paths in order, %d in the footprint", len(c.sortedPaths), len(c.Footprint))
	}
	for i, p := range c.sortedPaths {
		if _, ok := c.Footprint[p]; !ok || (i > 0 && c.sortedPaths[i-1] >= p) {
			return fmt.Errorf("path %d, %s, is out of order or not in the footprint", i, p)
		}
	}
	latest := make(map[string]time.Time, len(c.areaAt))
	for _, t := range c.Footprint {
		if at, ok := latest[t.Area]; t.Area != "" && (!ok || t.At.After(at)) {
			latest[t.Area] = t.At
		}
	}
	if len(latest) != len(c.areaAt) {
		return fmt.Errorf("%d areas indexed, %d in the footprint", len(c.areaAt), len(latest))
	}
	for a, at := range latest {
		if got, ok := c.areaAt[a]; !ok || !got.Equal(at) {
			return fmt.Errorf("the latest change in %s is %s, the index says %s", a, at, got)
		}
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func mustIndex(t *testing.T, b *Board, after string) {
	t.Helper()
	if err := b.checkIndexes(); err != nil {
		t.Fatalf("after %s: %v", after, err)
	}
}

// The indexes follow every change a random history makes: claims opened,
// released and forgotten, sessions that move between worktrees and are
// dropped, footprints reconciled, edits, a restart, and sweeps past every
// threshold.
func TestIndexesFollowEveryChange(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(5))
	for round := range 12 {
		n := 0
		b := New(DefaultConfig(), withIDs(func(prefix string) string { n++; return fmt.Sprintf("%s%04d", prefix, n) }))
		now := t0
		for step := range 300 {
			now = now.Add(time.Duration(rng.Intn(90)) * time.Second)
			if rng.Intn(40) == 0 {
				now = now.Add([]time.Duration{11 * time.Minute, 3 * time.Hour, 25 * time.Hour, 8 * 24 * time.Hour}[rng.Intn(4)])
			}
			m := fmt.Sprintf("m%d", rng.Intn(4))
			// A session's agent may report from either of two worktrees.
			w := Where{Repo: fmt.Sprintf("r%d", rng.Intn(2)), Host: "h", Worktree: fmt.Sprintf("/w/%s/%d", m, rng.Intn(3))}
			ev := HookEvent{Member: m, Agent: AgentClaudeCode, SessionID: fmt.Sprintf("s%d", rng.Intn(3)), Where: w}
			what := ""
			switch r := rng.Intn(20); {
			case r == 0:
				b.Sweep(now)
				what = "a sweep"
			case r == 1:
				data, _, err := b.Snapshot(now)
				if err != nil {
					t.Fatal(err)
				}
				b = New(DefaultConfig(), withIDs(func(prefix string) string { n++; return fmt.Sprintf("%s%04d", prefix, n) }))
				if err := b.Restore(data); err != nil {
					t.Fatal(err)
				}
				what = "a restore"
			case r < 4:
				_, _ = b.Declare(now, DeclareRequest{Member: m, Where: w, Patterns: []string{randomPattern(rng)}})
				what = "a declare"
			case r < 6:
				_, _ = b.Release(now, ReleaseRequest{Member: m, Where: w})
				what = "a release"
			default:
				ev.Kind = []Kind{KindSessionStart, KindPrompt, KindPreEdit, KindPostEdit, KindToolEnd, KindStop, KindSessionEnd, KindHeartbeat}[rng.Intn(8)]
				switch ev.Kind {
				case KindPreEdit, KindPostEdit:
					ev.Paths = indexFiles(rng, 1+rng.Intn(3))
				case KindPrompt:
				default:
					if rng.Intn(3) > 0 {
						ev.Footprint = &Footprint{Files: indexFiles(rng, rng.Intn(8))}
					}
				}
				_, _ = b.Hook(now, ev)
				what = string(ev.Kind)
			}
			mustIndex(t, b, fmt.Sprintf("round %d step %d, %s", round, step, what))
		}
	}
}

// indexFiles are files whose areas often change, and whose clock may run
// back: the cases where the latest change in an area must be found again.
func indexFiles(rng *rand.Rand, n int) []PathRef {
	out := make([]PathRef, n)
	for i := range out {
		out[i] = PathRef{Path: fmt.Sprintf("a%d/f%d.go", rng.Intn(3), rng.Intn(6)), Area: []string{"", "a0", "a1", "a2"}[rng.Intn(4)]}
	}
	return out
}

// A claim's index of areas answers what a walk of its footprint answers,
// over random changes: files added and changed one at a time, at times that
// sometimes run back, in areas that change; whole footprints replaced; and
// intents that cover areas too. Each step compares every area.
func TestAreaIndexMatchesTheFootprintWalk(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(6))
	areas := make([]string, 50)
	for i := range areas {
		areas[i] = fmt.Sprintf("svc%d/pkg%d", i/10, i%10)
	}
	b := New(DefaultConfig())
	compared, steps := 0, 42_000
	for step := range steps {
		if step%2000 == 0 {
			b = New(DefaultConfig())
			c := &claim{ID: "c_1", Repo: repo, Member: "m", Host: "h", Worktree: "/w"}
			if rng.Intn(2) == 0 {
				c.Intents = []Intent{{Pattern: areas[rng.Intn(len(areas))] + "/**", DeclaredAt: t0.Add(time.Hour)}}
			}
			b.addClaim(c)
		}
		c := b.claims["c_1"]
		at := t0.Add(time.Duration(rng.Intn(7200)-600) * time.Second)
		if rng.Intn(50) == 0 {
			at = time.Time{} // a touch saved without a time
		}
		path := fmt.Sprintf("f%03d.go", rng.Intn(300))
		area := ""
		if rng.Intn(10) > 0 {
			area = areas[rng.Intn(len(areas))]
		}
		switch r := rng.Intn(100); {
		case r < 3:
			fp := map[string]*touch{}
			var paths []string
			for p, t := range c.Footprint {
				if rng.Intn(2) == 0 {
					fp[p] = t
					paths = append(paths, p)
				}
			}
			c.setFootprint(fp, paths)
		default:
			c.putTouch(path, &touch{Area: area, At: at})
		}
		walk := map[string]time.Time{}
		for _, t := range c.Footprint {
			if t.Area != "" && t.At.After(walk[t.Area]) {
				walk[t.Area] = t.At
			}
		}
		for _, a := range areas {
			got, ok := b.workedInArea(c, a)
			want, found := walk[a]
			for _, in := range c.Intents {
				if dir := glob.LiteralDir(in.Pattern); dir == a || strings.HasPrefix(a, dir+"/") || strings.HasPrefix(dir, a+"/") {
					want, found = maxTime(want, in.DeclaredAt), true
				}
			}
			if step%50 == 0 {
				if w, f := oldWorkedInArea(c, a); f != found || !w.Equal(want) {
					t.Fatalf("step %d: area %s: the walk says %s, %t; the old search %s, %t", step, a, want, found, w, f)
				}
			}
			if ok != found || !got.Equal(want) {
				t.Fatalf("step %d: area %s: the index says %s, %t; the walk %s, %t", step, a, got, ok, want, found)
			}
			compared++
		}
		if step%1000 == 0 {
			mustIndex(t, b, fmt.Sprintf("step %d", step))
		}
	}
	if compared < 2_000_000 {
		t.Fatalf("only %d areas compared", compared)
	}
}

// The conflicts of a path are those the old walk of every claim, footprint
// and session found, on random boards seen at moments when their claims are
// running, stalled, dormant or gone.
func TestConflictsMatchTheOracle(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	found := 0
	for range 300 {
		b, now := randomBoard(t, rng, DefaultConfig(), rng.Intn(2) == 0)
		mustIndex(t, b, "a random history")
		ids := slices.Sorted(maps.Keys(b.claims))
		for _, later := range []time.Duration{0, 11 * time.Minute, 25 * time.Hour} {
			now := now.Add(later)
			for _, id := range ids {
				c := b.claims[id]
				var s *session
				for _, x := range b.claimSessions[id] {
					if s == nil || x.Key < s.Key {
						s = x
					}
				}
				if s == nil {
					s = &session{Key: "nobody"}
				}
				paths := randomFiles(rng, 1+rng.Intn(4))
				got, want := b.newCheck(now, c, s, paths), b.oldCheck(now, c, s, paths)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("conflicts of %v for %s:\n got: %+v\nwant: %+v", paths, id, got, want)
				}
				found += len(got)
			}
		}
	}
	t.Logf("%d conflicts compared", found)
	if found < 5000 {
		t.Fatalf("only %d conflicts compared", found)
	}
}

// The overlaps a new intent meets are those a match against every file of
// every claim finds, for patterns rooted at a file, at a directory, at a
// prefix of other names, and nowhere.
func TestIntentOverlapsMatchTheOracle(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(8))
	// Names that sort between a directory and its files: a/b, a/b-x/c,
	// a/b.go, a/b/c, a/b0.
	names := []string{"a/b", "a/b-x/c.go", "a/b.go", "a/b/c.go", "a/b/d/e.go", "a/b0", "a/c/b.go", "b/a.go", "a/b/c.go/x", "ab/c.go"}
	pats := []string{"a/b", "a/b/**", "a/b/*", "a/b*", "a/b/c.go", "**/c.go", "*", "a/*/c.go", "a/b-x", "a", "a/b/c.go/x",
		"a/[ab]/**", "b/**", "ab", "a/b0", "**"}
	compared := 0
	for range 400 {
		n := 0
		b := New(DefaultConfig(), withIDs(func(prefix string) string { n++; return fmt.Sprintf("%s%03d", prefix, n) }))
		now := t0
		for k := range 2 + rng.Intn(6) {
			m := fmt.Sprintf("m%d", k)
			w := Where{Repo: repo, Host: m, Worktree: "/w/" + m}
			var files []PathRef
			for _, name := range names {
				if rng.Intn(3) == 0 {
					files = append(files, PathRef{Path: name})
				}
			}
			now = now.Add(time.Second)
			if _, err := b.Hook(now, HookEvent{Kind: KindPostEdit, Member: m, Agent: AgentClaudeCode, SessionID: "s", Where: w, Paths: files}); err != nil {
				t.Fatal(err)
			}
			if rng.Intn(3) == 0 {
				_, _ = b.Declare(now, DeclareRequest{Member: m, Where: w, Patterns: []string{pats[rng.Intn(len(pats))]},
					Mode: []Mode{ModeShared, ModeExclusive}[rng.Intn(2)]})
			}
		}
		now = now.Add(time.Duration(rng.Intn(3)) * 10 * time.Minute)
		for _, id := range slices.Sorted(maps.Keys(b.claims)) {
			for _, p := range pats {
				in := Intent{Pattern: p, Mode: ModeShared, DeclaredAt: now}
				got := b.intentOverlaps(b.claims[id], in, b.liveAt(now))
				want := b.oldIntentOverlaps(b.claims[id], in, b.oldLiveClaims(now))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("overlaps of %s for %s:\n got: %+v\nwant: %+v", p, id, got, want)
				}
				compared += len(got)
			}
		}
	}
	if compared < 5000 {
		t.Fatalf("only %d overlaps compared", compared)
	}
}

// A repository's index skips the claims taken off the board, and drops them
// once they are half of it; an empty repository has no index.
func TestRemovedClaimsLeaveTheIndex(t *testing.T) {
	b := New(DefaultConfig())
	var cs []*claim
	for i := range 10 {
		c := &claim{ID: fmt.Sprintf("c_%02d", 9-i), Repo: repo, Member: "m", Host: "h", Worktree: fmt.Sprint(i)}
		b.addClaim(c)
		cs = append(cs, c)
	}
	ids := func() []string {
		var out []string
		for c := range b.claimsIn(repo) {
			out = append(out, c.ID)
		}
		return out
	}
	for i, c := range cs {
		b.removeClaim(c)
		mustIndex(t, b, fmt.Sprintf("removing %s", c.ID))
		if got := ids(); len(got) != 9-i || !slices.IsSorted(got) {
			t.Fatalf("after removing %d claims the index yields %v", i+1, got)
		}
		if r := b.byRepo[repo]; r != nil && len(r.claims) > 2*(9-i) {
			t.Fatalf("after removing %d claims the index still holds %d", i+1, len(r.claims))
		}
	}
	if len(b.byRepo) != 0 {
		t.Fatalf("an empty repository keeps its index: %v", b.byRepo)
	}
}

// A snapshot that names a claim or a session twice restores as before, the
// later one kept, with indexes that agree.
func TestRestoreOfDuplicatesKeepsIndexesRight(t *testing.T) {
	c1 := &claim{ID: "c_1", Repo: repo, Member: "m", Host: "h", Worktree: "/a", Footprint: map[string]*touch{"x.go": {Area: "x", At: t0}}}
	c2 := &claim{ID: "c_1", Repo: repo, Member: "m", Host: "h", Worktree: "/b"}
	s1 := &session{Key: "k", ClaimID: "c_1", LastSeen: t0, Phase: phaseWorking}
	s2 := &session{Key: "k", ClaimID: "c_2", LastSeen: t0, Phase: phaseWorking}
	data, err := json.Marshal(snapshot{Format: snapshotFormat, Claims: []*claim{c1, c2}, Sessions: []*session{s1, s2}})
	if err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig())
	if err := b.Restore(data); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, b, "restoring duplicates")
	if len(b.claims) != 1 || b.claims["c_1"].Worktree != "/b" || len(b.sessions) != 1 || b.sessions["k"].ClaimID != "c_2" {
		t.Fatalf("claims %v, sessions %v", b.claims, b.sessions)
	}
}
