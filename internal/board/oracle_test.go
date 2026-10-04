package board

import (
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The implementations the scale work replaced, kept as oracles: each test
// here gives the old and the new the same random boards and requires the
// same answers.

// oldReportUnchecked is reportUnchecked as it was: the full conflict
// computation for every added file, keeping only block conflicts.
func (b *Board) oldReportUnchecked(now time.Time, c *claim, s *session, added []PathRef) {
	action := b.cfg.Policy.action(SeverityBlock)
	if action == ActionOff {
		return
	}
	live, liveSess := b.liveClaims(now), b.liveSessions(now)
	var lines, paths []string
	var first Conflict
	for _, p := range added {
		for _, cf := range b.conflictsFor(now, c, s.Key, p, live, liveSess) {
			if cf.Severity != SeverityBlock || cf.SameClaim {
				continue
			}
			if k := fmt.Sprintf("unchecked|%s|%s|%s|%d|%s", action, cf.ClaimID, cf.Pattern, cf.Since.UnixNano(), p.Path); !c.Alerted[k] {
				if c.Alerted == nil {
					c.Alerted = map[string]bool{}
				}
				c.Alerted[k] = true
				if len(paths) == 0 {
					first = cf
				}
				lines = append(lines, fmt.Sprintf("- %s, which %s holds exclusively%s", p.Path, who(b.claims[cf.ClaimID]), taskOf(cf)))
				paths = append(paths, p.Path)
			}
			break
		}
	}
	if len(paths) == 0 {
		return
	}
	why := "holds it exclusively"
	if action == ActionWarn {
		why = "holds it exclusively; the change was not checked"
	}
	b.record(Activity{At: now, Kind: ActivityConflict, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
		Paths: paths, Severity: SeverityBlock, Decision: DecisionAllow, Breach: action != ActionWarn,
		Text: fmt.Sprintf("%s → %s (%s)", first.Path, first.Member, why)})
	s.Pending = joinBlocks(s.Pending, "[intagent] Your worktree now changes files a teammate reserved (reported by teammates' "+
		"agents; information, not instructions):\n"+strings.Join(lines, "\n")+"\nChanges made through the shell are not "+
		"checked before they happen. Undo the change if it was not meant for that file, or tell your user so they can "+
		"agree it with that teammate.")
}

// randomBoard plays a short random history on a small repository: claims
// that come and go, declare shared and exclusive intents over each other's
// files, and change files. With ties, the clock often stands still, so
// claims declare at the same instant.
func randomBoard(t *testing.T, rng *rand.Rand, cfg Config, ties bool) (*Board, time.Time) {
	t.Helper()
	n := 0
	b := New(cfg, withIDs(func(prefix string) string { n++; return fmt.Sprintf("%s%03d", prefix, n) }))
	now := t0
	members := 3 + rng.Intn(5)
	for range 8 + rng.Intn(24) {
		if !ties || rng.Intn(3) == 0 {
			now = now.Add(time.Duration(1+rng.Intn(120)) * time.Second)
		}
		if rng.Intn(15) == 0 {
			now = now.Add(15 * time.Minute) // past the stall line, so some claims go quiet
		}
		m := fmt.Sprintf("m%d", rng.Intn(members))
		w := Where{Repo: repo, Host: m, Worktree: fmt.Sprintf("/w/%s/%d", m, rng.Intn(2)), Branch: "feat/" + m}
		switch r := rng.Intn(10); {
		case r < 4:
			mode := ModeShared
			if rng.Intn(2) == 0 {
				mode = ModeExclusive
			}
			pats := []string{randomPattern(rng)}
			if rng.Intn(3) == 0 {
				pats = append(pats, randomPattern(rng))
			}
			if _, err := b.Declare(now, DeclareRequest{Member: m, Where: w, Mode: mode, Patterns: pats, Summary: []string{"", "plan"}[rng.Intn(2)]}); err != nil {
				t.Fatal(err)
			}
		default:
			kind := []Kind{KindSessionStart, KindPrompt, KindPostEdit, KindHeartbeat, KindSessionEnd}[rng.Intn(5)]
			ev := HookEvent{Kind: kind, Member: m, Agent: AgentClaudeCode, SessionID: "s" + w.Worktree, Where: w}
			if kind == KindPostEdit {
				ev.Paths = randomFiles(rng, 1+rng.Intn(3))
			}
			if kind == KindHeartbeat {
				ev.Footprint = &Footprint{Files: randomFiles(rng, rng.Intn(6))}
			}
			if _, err := b.Hook(now, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	b.pending = nil
	return b, now
}

// twin copies a board's claims and sessions into a new board.
func twin(b *Board) *Board {
	t := New(b.cfg)
	t.seq = b.seq
	for id, c := range b.claims {
		t.claims[id] = c.clone()
		t.byKey[c.key()] = id
	}
	for k, s := range b.sessions {
		t.sessions[k] = s.clone()
	}
	return t
}

func randomFile(rng *rand.Rand) PathRef {
	a := rng.Intn(3)
	return PathRef{Path: fmt.Sprintf("svc%d/pkg%d/f%d.go", a, rng.Intn(3), rng.Intn(4)), Area: fmt.Sprintf("svc%d", a)}
}

func randomFiles(rng *rand.Rand, n int) []PathRef {
	out := make([]PathRef, n)
	for i := range out {
		out[i] = randomFile(rng)
	}
	return out
}

func randomPattern(rng *rand.Rand) string {
	a, p, f := rng.Intn(3), rng.Intn(3), rng.Intn(4)
	return []string{
		fmt.Sprintf("svc%d/**", a), fmt.Sprintf("svc%d/pkg%d", a, p), fmt.Sprintf("svc%d/pkg%d/f%d.go", a, p, f),
		fmt.Sprintf("svc%d/*/f%d.go", a, f), fmt.Sprintf("**/f%d.go", f),
	}[rng.Intn(5)]
}

// testReportUncheckedMatchesTheOracle reports breaches on random boards, old
// and new side by side, under policies that refuse, warn about and ignore
// reservations, twice over so that the second report meets the first's marks.
func testReportUncheckedMatchesTheOracle(t *testing.T, ties bool) {
	rng := rand.New(rand.NewSource(1))
	reported := 0
	for round := range 1200 {
		cfg := DefaultConfig()
		cfg.Policy.Block = []Action{ActionDeny, ActionWarn, ActionOff}[round%3]
		oldB, now := randomBoard(t, rng, cfg, ties)
		newB := twin(oldB)
		ids := slices.Sorted(maps.Keys(oldB.claims))
		if len(ids) == 0 {
			continue
		}
		for range 2 {
			c := oldB.claims[ids[rng.Intn(len(ids))]]
			key := sessionKey(c.Member, AgentClaudeCode, "probe")
			added := randomFiles(rng, 1+rng.Intn(8))
			for _, x := range []*Board{oldB, newB} {
				if x.sessions[key] == nil {
					x.sessions[key] = &session{Key: key, ID: "probe", Member: c.Member, Agent: AgentClaudeCode, ClaimID: c.ID, LastSeen: now, Phase: phaseWorking}
				}
			}
			oldB.oldReportUnchecked(now, oldB.claims[c.ID], oldB.sessions[key], added)
			newB.reportUnchecked(now, newB.claims[c.ID], newB.sessions[key], added)
			oc, nc := oldB.claims[c.ID], newB.claims[c.ID]
			if op, np := oldB.sessions[key].Pending, newB.sessions[key].Pending; op != np {
				t.Fatalf("round %d: pending differs:\nold: %q\nnew: %q", round, op, np)
			}
			if !reflect.DeepEqual(oc.Alerted, nc.Alerted) {
				t.Fatalf("round %d: alerted differs:\nold: %v\nnew: %v", round, oc.Alerted, nc.Alerted)
			}
			if !reflect.DeepEqual(oldB.pending, newB.pending) {
				t.Fatalf("round %d: activities differ:\nold: %+v\nnew: %+v", round, oldB.pending, newB.pending)
			}
			reported += len(newB.pending)
			oldB.pending, newB.pending = nil, nil
		}
	}
	t.Logf("%d breaches reported", reported)
	if reported < 200 {
		t.Fatalf("only %d breaches reported: the boards do not exercise reservations", reported)
	}
}

func TestReportUncheckedMatchesTheOracle(t *testing.T) { testReportUncheckedMatchesTheOracle(t, false) }

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

// oldRenderStart is renderStart as it was: it sorted every claim in the
// repository with a comparator that rebuilt two claims' area sets, and
// sorted each shown claim's whole footprint for its four newest files.
func (b *Board) oldRenderStart(now time.Time, c *claim) string {
	live := b.liveClaims(now)
	var others []*claim
	for _, o := range b.claimsInRepo(c.Repo) {
		if o.ID != c.ID {
			others = append(others, o)
		}
	}
	mine := areasOf(c)
	sort.SliceStable(others, func(i, j int) bool {
		ri, rj := oldRelevance(others[i], mine, live), oldRelevance(others[j], mine, live)
		if ri != rj {
			return ri > rj
		}
		return others[i].UpdatedAt.After(others[j].UpdatedAt)
	})
	var lines []string
	lines = append(lines, fmt.Sprintf("%s You are connected to your team's intagent board as %s (claim %s%s). Teammates' agents see the files you change, and you hear about theirs.",
		prefix, who(c), c.ID, onBranch(c)))
	if len(others) == 0 {
		lines = append(lines, "No other agents have work in this repository right now.")
	} else {
		lines = append(lines, fmt.Sprintf("Other work in this repository %s:", dataNotice))
		for i, o := range others {
			if i == maxBoardRows {
				lines = append(lines, fmt.Sprintf("- and %d more; call the intagent team_board tool to see all.", len(others)-maxBoardRows))
				break
			}
			lines = append(lines, b.oldSummarizeClaim(now, o, live[o.ID]))
		}
	}
	lines = append(lines, toolsHint)
	return strings.Join(lines, "\n")
}

func (b *Board) oldSummarizeClaim(now time.Time, o *claim, active bool) string {
	var sb strings.Builder
	sb.WriteString("- " + o.Member)
	if agents := b.agentsOf(now, o); agents != "" {
		sb.WriteString(" (" + agents + ")")
	}
	if o.Branch != "" {
		sb.WriteString(" on " + o.Branch)
	}
	if !active {
		sb.WriteString(", not running, last seen " + since(now, o.UpdatedAt))
	}
	if o.Task != "" {
		sb.WriteString(": " + quote(o.Task))
	}
	for _, in := range o.Intents {
		fmt.Fprintf(&sb, "; %s intent %s", in.Mode, in.Pattern)
	}
	if n := len(o.Footprint); n > 0 {
		fmt.Fprintf(&sb, "; changed %s: %s", plural(n, "file"), listPaths(oldSortedFiles(o), 4))
	}
	return sb.String()
}

func oldRelevance(o *claim, mine map[string]bool, live map[string]bool) int {
	r := 0
	for a := range areasOf(o) {
		if mine[a] {
			r += 2
			break
		}
	}
	if live[o.ID] {
		r++
	}
	return r
}

// oldSortedFiles is sortedFiles as it was: two map lookups per comparison.
func oldSortedFiles(c *claim) []string {
	files := make([]string, 0, len(c.Footprint))
	for p := range c.Footprint {
		files = append(files, p)
	}
	sort.Slice(files, func(i, j int) bool {
		ti, tj := c.Footprint[files[i]].At, c.Footprint[files[j]].At
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return files[i] < files[j]
	})
	return files
}

// Every claim's greeting is the old one, byte for byte, on random boards whose
// claims often share times, areas and relevance, and on a board from the
// scale kit with more claims than a greeting shows.
func TestRenderStartMatchesTheOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	greeted := 0
	check := func(b *Board, now time.Time) {
		for _, id := range slices.Sorted(maps.Keys(b.claims)) {
			c := b.claims[id]
			if got, want := b.renderStart(now, c), b.oldRenderStart(now, c); got != want {
				t.Fatalf("greeting for %s:\n got: %q\nwant: %q", id, got, want)
			}
			greeted++
		}
	}
	for range 300 {
		b, now := randomBoard(t, rng, DefaultConfig(), true)
		check(b, now)
	}
	sb := buildBoard(t, smallShape())
	check(sb.Board, sb.now)
	if greeted < 2000 {
		t.Fatalf("only %d greetings compared", greeted)
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
