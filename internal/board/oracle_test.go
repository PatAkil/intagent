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

	"github.com/patakil/intagent/internal/glob"
)

// The implementations the scale work replaced, kept as oracles: each test
// here gives the old and the new the same random boards and requires the
// same answers, and the benchmarks in scale_bench_test.go compare their cost.

// oldLiveClaims is how the board found live claims before it indexed each
// claim's sessions: a map of them all, built from every session.
func (b *Board) oldLiveClaims(now time.Time) map[string]bool {
	live := map[string]bool{}
	for _, s := range b.sessions {
		if b.state(now, s).Live() {
			live[s.ClaimID] = true
		}
	}
	return live
}

// oldLiveSessions is oldLiveClaims for sessions.
func (b *Board) oldLiveSessions(now time.Time) map[string]bool {
	live := map[string]bool{}
	for k, s := range b.sessions {
		if b.state(now, s).Live() {
			live[k] = true
		}
	}
	return live
}

// oldClaimsInRepo is a repository's claims in ID order, found among all of them.
func (b *Board) oldClaimsInRepo(repo string) []*claim {
	var out []*claim
	for _, c := range b.claims {
		if c.Repo == repo {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// oldConflictsFor is conflictsFor as it was: every claim on the board
// visited, each claim's footprint searched for the area, and liveness read
// from maps of the whole board.
func (b *Board) oldConflictsFor(now time.Time, self *claim, selfSession string, p PathRef, live map[string]bool, liveSess map[string]bool) []Conflict {
	var out []Conflict
	for _, o := range b.claims {
		if o.Repo != self.Repo || o.ID == self.ID {
			continue
		}
		if cf, ok := b.oldConflictWith(now, o, p, live[o.ID]); ok {
			out = append(out, cf)
		}
	}
	if t, ok := self.Footprint[p.Path]; ok && !t.FromGit && t.Session != "" && t.Session != selfSession && liveSess[t.Session] {
		out = append(out, Conflict{
			Path: p.Path, Area: p.Area, Severity: SeverityOverlap, ClaimID: self.ID, Member: self.Member, Branch: self.Branch, Task: self.Task,
			Why: "another live session in this same worktree changed this file", Since: t.At, Active: true, SameClaim: true,
		})
	}
	sortConflicts(out)
	return out
}

func (b *Board) oldConflictWith(now time.Time, o *claim, p PathRef, active bool) (Conflict, bool) {
	best := Conflict{Path: p.Path, Area: p.Area, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: active}
	consider := func(sev Severity, why, pattern string, since time.Time) {
		if sev > best.Severity {
			best.Severity, best.Why, best.Pattern, best.Since = sev, why, pattern, since
		}
	}
	for _, in := range o.Intents {
		if !glob.Match(in.Pattern, p.Path) {
			continue
		}
		sev := SeverityOverlap
		if in.Mode == ModeExclusive && active {
			sev = SeverityBlock
		}
		consider(sev, intentWhy(in), in.Pattern, in.DeclaredAt)
	}
	if t, ok := o.Footprint[p.Path]; ok {
		sev := SeverityOverlap
		why := "has unmerged changes to this file"
		if !active && now.Sub(o.UpdatedAt) > b.cfg.DormantFor {
			sev = SeverityNearby
			why = "had unmerged changes to this file (claim dormant for " + ago(now, o.UpdatedAt) + ")"
		}
		consider(sev, why, "", t.At)
	}
	if best.Severity < SeverityNearby && p.Area != "" {
		if since, ok := oldWorkedInArea(o, p.Area); ok {
			consider(SeverityNearby, "is working in the same area "+p.Area, "", since)
		}
	}
	return best, best.Severity > SeverityNone
}

// oldWorkedInArea is workedInArea as it was: a walk of the whole footprint.
func oldWorkedInArea(c *claim, area string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, t := range c.Footprint {
		if t.Area == area && t.At.After(latest) {
			latest, found = t.At, true
		}
	}
	for _, in := range c.Intents {
		dir := glob.LiteralDir(in.Pattern)
		if dir == "" {
			continue
		}
		if glob.Match(area, dir) || glob.Match(dir, area) {
			if in.DeclaredAt.After(latest) {
				latest = in.DeclaredAt
			}
			found = true
		}
	}
	return latest, found
}

// oldCheck is what a pre_edit or a check of paths found before the
// indexes: the conflicts of each path.
func (b *Board) oldCheck(now time.Time, c *claim, s *session, paths []PathRef) []Conflict {
	live, liveSess := b.oldLiveClaims(now), b.oldLiveSessions(now)
	var out []Conflict
	for _, p := range paths {
		out = append(out, b.oldConflictsFor(now, c, s.Key, p, live, liveSess)...)
	}
	return out
}

// newCheck is oldCheck through the indexes.
func (b *Board) newCheck(now time.Time, c *claim, s *session, paths []PathRef) []Conflict {
	live := b.liveAt(now)
	var out []Conflict
	for _, p := range paths {
		out = append(out, b.conflictsFor(live, c, s.Key, p)...)
	}
	return out
}

// oldIntentOverlaps is intentOverlaps as it was: every file of every other
// claim in the repository matched against the new intent.
func (b *Board) oldIntentOverlaps(c *claim, in Intent, live map[string]bool) []Conflict {
	var out []Conflict
	for _, o := range b.oldClaimsInRepo(c.Repo) {
		if o.ID == c.ID {
			continue
		}
		cf := Conflict{Path: in.Pattern, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: live[o.ID]}
		for _, oi := range o.Intents {
			if glob.Overlap(oi.Pattern, in.Pattern) {
				sev := SeverityOverlap
				if oi.Mode == ModeExclusive && cf.Active {
					sev = SeverityBlock
				}
				if sev > cf.Severity {
					cf.Severity, cf.Pattern, cf.Since = sev, oi.Pattern, oi.DeclaredAt
					cf.Why = fmt.Sprintf("declared %s intent %s", oi.Mode, oi.Pattern)
				}
			}
		}
		if cf.Severity < SeverityOverlap {
			var hit []string
			var since time.Time
			for p, t := range o.Footprint {
				if glob.Match(in.Pattern, p) {
					hit = append(hit, p)
					if t.At.After(since) {
						since = t.At
					}
				}
			}
			if len(hit) > 0 {
				sort.Strings(hit)
				cf.Severity, cf.Since = SeverityOverlap, since
				cf.Why = "has unmerged changes to " + listPaths(hit, 3)
			}
		}
		if cf.Severity > SeverityNone {
			out = append(out, cf)
		}
	}
	return out
}

// oldAgentsOf is agentsOf as it was, over every session on the board, with
// repeats counted as a greeting now counts them: what the oracle checks is
// which sessions it describes.
func (b *Board) oldAgentsOf(now time.Time, c *claim) string {
	counts := map[string]int{}
	for _, s := range b.sessions {
		if s.ClaimID != c.ID {
			continue
		}
		if st := b.state(now, s); st != StateEnded && st != StateGone {
			counts[string(s.Agent)+" "+string(st)]++
		}
	}
	var parts []string
	for k, n := range counts {
		if n > 1 {
			k = fmt.Sprintf("%d %s", n, k)
		}
		parts = append(parts, k)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func areasOf(c *claim) map[string]bool {
	out := map[string]bool{}
	for _, t := range c.Footprint {
		if t.Area != "" {
			out[t.Area] = true
		}
	}
	return out
}

// oldReportUnchecked is reportUnchecked as it was: the full conflict
// computation for every added file, keeping only block conflicts. It tells
// the sessions as reportUnchecked does now, so that the oracle checks which
// files and reservations are found.
func (b *Board) oldReportUnchecked(now time.Time, c *claim, s *session, added []PathRef) {
	action := b.cfg.Policy.action(SeverityBlock)
	if action == ActionOff {
		return
	}
	live, liveSess := b.oldLiveClaims(now), b.oldLiveSessions(now)
	var lines, paths []string
	var first Conflict
	for _, p := range added {
		for _, cf := range b.oldConflictsFor(now, c, s.Key, p, live, liveSess) {
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
	b.tellUnchecked(now, c, s, lines) // who is told, and in what words, came later
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
	for _, c := range b.claims {
		t.addClaim(c.oldClone()) // a deep copy: the old code changes maps in place
	}
	for _, s := range b.sessions {
		t.attachSession(s.clone(), s.ClaimID)
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
					x.attachSession(&session{Key: key, ID: "probe", Member: c.Member, Agent: AgentClaudeCode, LastSeen: now, Phase: phaseWorking}, c.ID)
				}
			}
			oldB.oldReportUnchecked(now, oldB.claims[c.ID], oldB.sessions[key], added)
			newB.reportUnchecked(now, newB.claims[c.ID], newB.sessions[key], added)
			oc, nc := oldB.claims[c.ID], newB.claims[c.ID]
			for k, was := range oldB.sessions {
				if op, np := was.Pending, newB.sessions[k].Pending; op != np {
					t.Fatalf("round %d: pending of %s differs:\nold: %q\nnew: %q", round, k, op, np)
				}
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

func TestReportUncheckedMatchesTheOracle(t *testing.T) {
	t.Parallel()
	testReportUncheckedMatchesTheOracle(t, false)
}

// Reservations declared at the same instant tie; the old function took the
// first conflict its sort left in front, which sortConflicts now orders
// whatever order the claims were found in.
func TestReportUncheckedMatchesTheOracleWithTies(t *testing.T) {
	t.Parallel()
	testReportUncheckedMatchesTheOracle(t, true)
}

// oldAlertOthers is alertOthers as it was: every new file compared with
// every intent of every other claim in the repository. It remembers and
// lists at most maxToldPaths files per pair of claims, as alertOthers does
// now, since what is compared is which claims and files match.
func (b *Board) oldAlertOthers(now time.Time, c *claim, paths []PathRef) {
	for _, o := range b.oldClaimsInRepo(c.Repo) {
		if o.ID == c.ID {
			continue
		}
		var hit []string
		told := o.Told[c.ID]
		for _, p := range paths {
			if _, ok := o.Footprint[p.Path]; !ok && !slices.ContainsFunc(o.Intents, func(in Intent) bool { return glob.Match(in.Pattern, p.Path) }) {
				continue
			}
			k := "touch|" + c.ID + "|" + p.Path
			if o.Alerted[k] {
				continue
			}
			if o.Alerted == nil {
				o.Alerted = map[string]bool{}
			}
			if told < maxToldPaths {
				o.Alerted[k] = true
				told++
			}
			hit = append(hit, p.Path)
		}
		if len(hit) == 0 {
			continue
		}
		if told != o.Told[c.ID] {
			if o.Told == nil {
				o.Told = map[string]int{}
			}
			o.Told[c.ID] = told
		}
		b.statsOf(c.Repo, now).Alerts++
		b.enqueue(now, o, InboxItem{Kind: "overlap", FromClaim: c.ID, From: c.Member, Paths: hit[:min(len(hit), maxToldPaths)],
			Text: fmt.Sprintf("%s also changed %s%s.", who(c), listPaths(hit, 5), onBranch(c))})
	}
}

// edgeFiles are names beside the ones randomBoard's patterns are rooted in:
// a file named like a directory, and directories that only begin like one.
var edgeFiles = []string{"svc1", "svc1.go", "svc1-old/pkg1/f1.go", "svc10/pkg1/f1.go", "svc1/pkg1.go", "svc1/pkg10/f1.go", "svc2/pkg0"}

// Alerts on random boards, old and new side by side, twice over so that the
// second meets the first's marks: each intent is now matched against the
// files under its directory alone, and must alert the same claims of the
// same files.
func TestAlertOthersMatchesTheOracle(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(3))
	alerts := 0
	for round := range 1500 {
		oldB, now := randomBoard(t, rng, DefaultConfig(), false)
		newB := twin(oldB)
		ids := slices.Sorted(maps.Keys(oldB.claims))
		if len(ids) == 0 {
			continue
		}
		for range 2 {
			c := oldB.claims[ids[rng.Intn(len(ids))]]
			seen := map[string]bool{}
			var added []PathRef
			for range 1 + rng.Intn(12) {
				p := randomFile(rng)
				if rng.Intn(4) == 0 {
					p = PathRef{Path: edgeFiles[rng.Intn(len(edgeFiles))]}
				}
				if !seen[p.Path] {
					seen[p.Path] = true
					added = append(added, p)
				}
			}
			oldB.oldAlertOthers(now, oldB.claims[c.ID], added)
			newB.alertOthers(now, newB.claims[c.ID], added)
			for _, id := range ids {
				oc, nc := oldB.claims[id], newB.claims[id]
				if !reflect.DeepEqual(oc.Alerted, nc.Alerted) {
					t.Fatalf("round %d: alerted of %s differs:\nold: %v\nnew: %v", round, id, oc.Alerted, nc.Alerted)
				}
				if a, b := inboxOf(oc), inboxOf(nc); a != b {
					t.Fatalf("round %d: inbox of %s differs:\nold: %s\nnew: %s", round, id, a, b)
				}
			}
			if a, b := oldB.statsOf(repo, now).Alerts, newB.statsOf(repo, now).Alerts; a != b {
				t.Fatalf("round %d: %d alerts counted by the old, %d by the new", round, a, b)
			}
		}
		alerts += newB.statsOf(repo, now).Alerts
	}
	t.Logf("%d alerts", alerts)
	if alerts < 1000 {
		t.Fatalf("only %d alerts: the boards do not exercise them", alerts)
	}
}

// inboxOf is a claim's inbox without the IDs, which each board draws at
// random; an empty one is empty however it was copied.
func inboxOf(c *claim) string {
	items := append([]InboxItem{}, c.Inbox...)
	for i := range items {
		items[i].ID = ""
	}
	return jsonOf(items)
}

// oldRenderStart is renderStart as it was: it sorted every claim in the
// repository with a comparator that rebuilt two claims' area sets, and
// sorted each shown claim's whole footprint for its four newest files. Its
// "and N more" line says what renderStart says now, since what the oracle
// checks is which claims a greeting shows, not how it words the rest.
func (b *Board) oldRenderStart(now time.Time, c *claim) string {
	live := b.oldLiveClaims(now)
	var others []*claim
	for _, o := range b.oldClaimsInRepo(c.Repo) {
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
				lines = append(lines, fmt.Sprintf("- and %d more; the intagent team_board tool lists more, and check_paths checks the files you plan to change.", len(others)-maxBoardRows))
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
	if agents := b.oldAgentsOf(now, o); agents != "" {
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
	t.Parallel()
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

// oldView is View as it was, with oldSortedFiles' sort.
func (b *Board) oldView(now time.Time, repo string) View {
	repo = RepoID(repo)
	v := View{Repo: repo, At: now, Policy: b.cfg.Policy, LastSeq: b.seq, Claims: []ClaimView{}, Recent: []Activity{}}
	live := b.oldLiveClaims(now)
	members := map[string]bool{}
	bySession := map[string][]SessionView{}
	for _, s := range b.sessions {
		st := b.state(now, s)
		bySession[s.ClaimID] = append(bySession[s.ClaimID], SessionView{
			ID: s.ID, Agent: s.Agent, State: st, Tool: s.Tool, ToolSince: s.ToolSince, StartedAt: s.StartedAt, LastSeen: s.LastSeen,
		})
	}
	claims := b.oldClaimsInRepo(repo)
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
			if now.Sub(it.At) < inboxTTL && !it.Shown {
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
	t.Parallel()
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

// oldSweep is Sweep as it was: for every claim with nothing left, it
// searched every session for one still attached to it. It marks idle gones,
// and tidies inboxes and alerts, as Sweep does now, since what is compared
// is how claims are found.
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
				b.record(Activity{At: now, Kind: kind, Repo: repo, Member: member, ClaimID: s.ClaimID, Session: s.ID, Agent: s.Agent, Text: text,
					Idle: st == StateGone && s.Phase == phaseWaiting})
			}
			s.Reported = st
			changed = true
		}
		if !st.Live() && now.Sub(s.LastSeen) > keepEndedFor && (st == StateEnded || now.Sub(s.LastSeen) > b.cfg.IdleAfter+keepEndedFor) {
			b.detachSession(s)
			changed = true
		}
	}
	live := b.oldLiveClaims(now)
	ids := make([]string, 0, len(b.claims))
	for id := range b.claims {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := b.claims[id]
		changed = b.tidy(now, c, live[c.ID]) || changed
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
	changed = b.pruneAlerts() || changed
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
	t.Parallel()
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

// oldSnapshotCopy is what Snapshot copied under the lock before it shared
// footprints and alerts with the board: every footprint, touch and alert.
func (b *Board) oldSnapshotCopy(now time.Time) (snapshot, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := snapshot{Format: snapshotFormat, Saved: now, Seq: b.seq, Version: b.version, Recent: slices.Clone(b.recent), Stats: map[string]*Stats{},
		Dropped: &droppedMarks{Repos: maps.Clone(b.dropped), All: b.droppedAll, Floor: b.droppedFloor}}
	for repo, st := range b.stats {
		c := *st
		s.Stats[repo] = &c
	}
	for _, c := range b.claims {
		s.Claims = append(s.Claims, c.oldClone())
	}
	for _, x := range b.sessions {
		s.Sessions = append(s.Sessions, x.clone())
	}
	if len(b.mail) > 0 {
		s.Mail = b.mailCopy()
	}
	return s, b.version
}

func (c *claim) oldClone() *claim {
	d := *c
	d.Intents = slices.Clone(c.Intents)
	d.Footprint = make(map[string]*touch, len(c.Footprint))
	for k, t := range c.Footprint {
		tt := *t
		d.Footprint[k] = &tt
	}
	d.Inbox = make([]InboxItem, len(c.Inbox))
	for i, it := range c.Inbox {
		it.Paths = slices.Clone(it.Paths)
		d.Inbox[i] = it
	}
	d.Alerted = maps.Clone(c.Alerted)
	d.Told = maps.Clone(c.Told)
	d.areaAt, d.sortedPaths, d.removed = nil, nil, false
	return &d
}

// oldSnapshot is Snapshot before it was streamed, with its claims and
// sessions in order: the copy marshalled whole.
func (b *Board) oldSnapshot(now time.Time) ([]byte, uint64, error) {
	s, version := b.oldSnapshotCopy(now)
	slices.SortFunc(s.Claims, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
	slices.SortFunc(s.Sessions, func(x, y *session) int { return strings.Compare(x.Key, y.Key) })
	data, err := json.Marshal(s)
	return data, version, err
}

// oldReadSnapshot is how Restore read a snapshot before it was streamed:
// the whole file in memory, unmarshalled at once.
func oldReadSnapshot(data []byte) (snapshot, error) {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if s.Format != snapshotFormat {
		return s, fmt.Errorf("snapshot format %d is not supported", s.Format)
	}
	return s, nil
}
