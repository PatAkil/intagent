package board

import (
	"bytes"
	"encoding/json"
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

// twin copies a board's claims, sessions, feed, stats and version into a new board.
func twin(b *Board) *Board {
	t := New(b.cfg)
	t.seq, t.version, t.recent = b.seq, b.version, slices.Clone(b.recent)
	for repo, st := range b.stats {
		c := *st
		t.stats[repo] = &c
	}
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

// Reservations declared at the same instant tie; the old function took the
// first conflict its sort left in front, which sortConflicts now orders
// whatever order the claims were found in.
func TestReportUncheckedMatchesTheOracleWithTies(t *testing.T) {
	testReportUncheckedMatchesTheOracle(t, true)
}

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

// oldView is View as it was, with oldSortedFiles' sort.
func (b *Board) oldView(now time.Time, repo string) View {
	repo = RepoID(repo)
	v := View{Repo: repo, At: now, Policy: b.cfg.Policy, LastSeq: b.seq, Claims: []ClaimView{}, Recent: []Activity{}}
	live := b.liveClaims(now)
	members := map[string]bool{}
	bySession := map[string][]SessionView{}
	for _, s := range b.sessions {
		st := b.state(now, s)
		bySession[s.ClaimID] = append(bySession[s.ClaimID], SessionView{
			ID: s.ID, Agent: s.Agent, State: st, Tool: s.Tool, ToolSince: s.ToolSince, StartedAt: s.StartedAt, LastSeen: s.LastSeen,
		})
	}
	claims := b.claimsInRepo(repo)
	changedBy := map[string]int{}
	for _, c := range claims {
		for p := range c.Footprint {
			changedBy[p]++
		}
	}
	for _, c := range claims {
		members[c.Member] = true
		cv := ClaimView{
			ID: c.ID, Member: c.Member, Host: c.Host, Worktree: c.Worktree, Branch: c.Branch, Task: c.Task,
			Active: live[c.ID], Intents: append([]Intent{}, c.Intents...), Truncated: c.FootprintTruncated,
			Sessions: bySession[c.ID], CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Files: []FileView{},
		}
		if cv.Sessions == nil {
			cv.Sessions = []SessionView{}
		}
		sort.Slice(cv.Sessions, func(i, j int) bool { return cv.Sessions[i].LastSeen.After(cv.Sessions[j].LastSeen) })
		for _, s := range cv.Sessions {
			if s.State.Live() {
				v.Sessions++
			}
		}
		files := oldSortedFiles(c)
		cv.FileCount = len(files)
		if len(files) > maxViewFiles {
			out := files[:maxViewFiles:maxViewFiles]
			for _, p := range files[maxViewFiles:] {
				if len(out) == 2*maxViewFiles {
					break
				}
				if changedBy[p] > 1 {
					out = append(out, p)
				}
			}
			files = out
		}
		for _, p := range files {
			t := c.Footprint[p]
			cv.Files = append(cv.Files, FileView{Path: p, Area: t.Area, At: t.At, FromGit: t.FromGit})
		}
		for _, it := range c.Inbox {
			if now.Sub(it.At) < inboxTTL && len(it.DeliveredTo) == 0 {
				cv.Pending++
			}
		}
		v.Claims = append(v.Claims, cv)
	}
	sort.SliceStable(v.Claims, func(i, j int) bool {
		if v.Claims[i].Active != v.Claims[j].Active {
			return v.Claims[i].Active
		}
		return v.Claims[i].UpdatedAt.After(v.Claims[j].UpdatedAt)
	})
	for m := range members {
		v.Members = append(v.Members, m)
	}
	sort.Strings(v.Members)
	for _, a := range b.recent {
		if a.Repo == repo {
			v.Recent = append(v.Recent, a)
		}
	}
	v.Stats = b.statsFor(repo)
	return v
}

// tiedBoard is a board whose claims all found their whole footprints at the
// same instant: every file of every claim has one time.
func tiedBoard(t *testing.T, claims, files int) *scaleBoard {
	t.Helper()
	sb := newScaleBoard(smallShape(), DefaultConfig(), t0, 0)
	for i := range claims {
		ev := sb.event(i, KindHeartbeat)
		ev.Where.Repo = scaleRepo(0)
		ev.Footprint = sb.footprint(i, files)
		if _, err := sb.Hook(sb.now, ev); err != nil {
			t.Fatal(err)
		}
	}
	return sb
}

// A view lists the same files, in the same order, as the old one: on boards
// whose files were found at many moments, whose claims are all at the cap,
// and whose files all share one time.
func TestViewMatchesTheOracle(t *testing.T) {
	mixed := smallShape()
	capped := smallShape()
	capped.dist, capped.claims, capped.busy, capped.sessions, capped.dormant = filesAtCap, 8, 6, 8, 2
	for _, tc := range []struct {
		name string
		sb   func(*testing.T) *scaleBoard
	}{
		{"mixed", func(t *testing.T) *scaleBoard { return buildBoard(t, mixed) }},
		{"at the cap", func(t *testing.T) *scaleBoard { return buildBoard(t, capped) }},
		{"all files tied", func(t *testing.T) *scaleBoard { return tiedBoard(t, 12, 700) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := tc.sb(t)
			capped := 0
			for r := range sb.sh.repos {
				got, err := json.Marshal(sb.View(sb.now, scaleRepo(r)))
				if err != nil {
					t.Fatal(err)
				}
				want, err := json.Marshal(sb.oldView(sb.now, scaleRepo(r)))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("the view of %s differs from the old one", scaleRepo(r))
				}
				for _, c := range sb.View(sb.now, scaleRepo(r)).Claims {
					if c.FileCount > maxViewFiles {
						capped++
					}
				}
			}
			if capped == 0 {
				t.Fatal("no claim has more files than a view lists: the cap is not exercised")
			}
		})
	}
}

// oldRecord is record as it was: it copied the whole feed into a new array
// on every activity once the feed was full.
func (b *Board) oldRecord(a Activity) {
	b.seq++
	a.Seq = b.seq
	b.recent = append(b.recent, a)
	if over := len(b.recent) - b.cfg.KeepActivities; over > 0 {
		b.recent = append(b.recent[:0:0], b.recent[over:]...)
	}
	b.pending = append(b.pending, a)
}

// The feed holds the last KeepActivities activities, in order, whatever its
// length and however many have been recorded.
func TestFeedKeepsTheLastActivities(t *testing.T) {
	for _, keep := range []int{1, 7, 300, 3000} {
		b := New(Config{KeepActivities: keep})
		for n := 1; n <= 200_000; n++ {
			b.record(Activity{Kind: ActivityFileChanged, Repo: repo, Text: fmt.Sprint(n), Paths: []string{"a.go"}})
			b.pending = b.pending[:0]
			if n%997 != 0 && n != 200_000 && n > keep+1 {
				continue
			}
			first := max(1, n-keep+1)
			if len(b.recent) != n-first+1 {
				t.Fatalf("keep %d, %d recorded: the feed holds %d", keep, n, len(b.recent))
			}
			for i, a := range b.recent {
				if want := uint64(first + i); a.Seq != want || a.Text != fmt.Sprint(want) {
					t.Fatalf("keep %d, %d recorded: entry %d is seq %d %q, want %d", keep, n, i, a.Seq, a.Text, want)
				}
			}
		}
	}
}

// Recording an activity into a full feed does not copy the feed: across many
// records, the array is replaced about once per KeepActivities of them.
func TestRecordDoesNotCopyTheFeed(t *testing.T) {
	b := New(DefaultConfig())
	a := Activity{Kind: ActivityFileChanged, Repo: repo, Paths: []string{"a.go"}}
	for range b.cfg.KeepActivities {
		b.record(a)
	}
	// AllocsPerRun rounds down: under one allocation per record is 0. The old
	// record allocated a new array for every one.
	if allocs := testing.AllocsPerRun(1000, func() { b.record(a); b.pending = b.pending[:0] }); allocs != 0 {
		t.Fatalf("recording into a full feed of %d made %.0f allocations per activity", b.cfg.KeepActivities, allocs)
	}
}

// oldSweep is Sweep as it was: for every claim with nothing left, it
// searched every session for one still attached to it.
func (b *Board) oldSweep(now time.Time) {
	changed := false
	keys := make([]string, 0, len(b.sessions))
	for k := range b.sessions {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int {
		if c := b.sessions[x].LastSeen.Compare(b.sessions[y].LastSeen); c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	for _, k := range keys {
		s := b.sessions[k]
		st := b.state(now, s)
		if st != s.Reported {
			if st == StateStalled || st == StateGone {
				c := b.claims[s.ClaimID]
				repo, member := "", s.Member
				if c != nil {
					repo = c.Repo
				}
				text := fmt.Sprintf("silent for %s", ago(now, s.LastSeen))
				if st == StateStalled && s.Tool != "" {
					text = fmt.Sprintf("inside %s for %s", s.Tool, ago(now, s.ToolSince))
				}
				kind := ActivitySessionGone
				if st == StateStalled {
					kind = ActivitySessionStalled
				}
				b.record(Activity{At: now, Kind: kind, Repo: repo, Member: member, ClaimID: s.ClaimID, Session: s.ID, Agent: s.Agent, Text: text})
			}
			s.Reported = st
			changed = true
		}
		if !st.Live() && now.Sub(s.LastSeen) > keepEndedFor && (st == StateEnded || now.Sub(s.LastSeen) > b.cfg.IdleAfter+keepEndedFor) {
			delete(b.sessions, k)
			changed = true
		}
	}
	live := b.liveClaims(now)
	ids := make([]string, 0, len(b.claims))
	for id := range b.claims {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := b.claims[id]
		if live[c.ID] {
			continue
		}
		switch {
		case len(c.Intents) == 0 && len(c.Footprint) == 0 && !b.oldHasSessions(c.ID):
			b.deleteClaim(now, c, ActivityClaimReleased)
			changed = true
		case now.Sub(c.UpdatedAt) > b.cfg.ForgetAfter:
			b.deleteClaim(now, c, ActivityClaimForgotten)
			changed = true
		}
	}
	for m, ts := range b.notes {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > noteWindow {
			delete(b.notes, m)
		}
	}
	if changed {
		b.changed()
	}
}

func (b *Board) oldHasSessions(claimID string) bool {
	for _, s := range b.sessions {
		if s.ClaimID == claimID {
			return true
		}
	}
	return false
}

// canonical is a board's snapshot with its claims and sessions in order.
func canonical(t *testing.T, b *Board, now time.Time) string {
	t.Helper()
	data, _, err := b.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(s.Claims, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
	slices.SortFunc(s.Sessions, func(x, y *session) int { return strings.Compare(x.Key, y.Key) })
	return jsonOf(s)
}

// sweptState is what a sweep changes: which sessions and claims remain, what
// each session was last announced as, and the board's version.
func sweptState(b *Board) string {
	var sb strings.Builder
	for _, k := range slices.Sorted(maps.Keys(b.sessions)) {
		fmt.Fprintf(&sb, "%s=%s ", k, b.sessions[k].Reported)
	}
	fmt.Fprintf(&sb, "| %v | %d", slices.Sorted(maps.Keys(b.claims)), b.version)
	return sb.String()
}

// Sweeps past the stall, idle, keep and forget lines leave random boards as
// the old Sweep left them, and announce the same, on boards with sessions
// that went quiet working, waiting, inside tools and after ending.
func TestSweepMatchesTheOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	announced, released := 0, 0
	for range 300 {
		oldB, now := randomBoard(t, rng, DefaultConfig(), true)
		newB := twin(oldB)
		var told []Activity // Sweep hands its activities to the notifier as it unlocks
		newB.notify = func(a []Activity) { told = append(told, a...) }
		for _, d := range []time.Duration{0, 11 * time.Minute, 46 * time.Minute, 2*time.Hour + time.Minute, 15 * time.Second,
			time.Hour, 7 * 24 * time.Hour} {
			now = now.Add(d)
			oldB.oldSweep(now)
			newB.Sweep(now)
			if len(oldB.pending)+len(told) > 0 && !reflect.DeepEqual(oldB.pending, told) {
				t.Fatalf("sweep at %s announced differently:\nold: %+v\nnew: %+v", now, oldB.pending, told)
			}
			if o, n := sweptState(oldB), sweptState(newB); o != n {
				t.Fatalf("sweep at %s left the board differently:\nold: %s\nnew: %s", now, o, n)
			}
			for _, a := range told {
				switch a.Kind {
				case ActivitySessionStalled, ActivitySessionGone:
					announced++
				case ActivityClaimReleased, ActivityClaimForgotten:
					released++
				}
			}
			oldB.pending, told = nil, nil
		}
		if o, n := canonical(t, oldB, now), canonical(t, newB, now); o != n {
			t.Fatalf("the sweeps left the board differently:\nold: %s\nnew: %s", o, n)
		}
	}
	t.Logf("%d sessions announced, %d claims released or forgotten", announced, released)
	if announced < 500 || released < 500 {
		t.Fatalf("%d sessions announced and %d claims released: the boards do not exercise the sweep", announced, released)
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
