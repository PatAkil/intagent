package board

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	update         = flag.Bool("update", false, "rewrite the golden files in testdata")
	transcriptFile = flag.String("transcript", "", "write TestBoardTranscript's full transcript to this file")
)

// TestBoardTranscript pins everything the board says, step by step, over a
// seeded workload: every hook kind, edits of several files, footprints that
// grow, shrink and change areas, intents declared and released, notes,
// checks, sweeps, clock jumps past the stall, idle, dormant and forget
// thresholds, and the board saved and restored. A change meant to make the
// board faster must leave the transcript byte for byte as it was; a change to
// what agents are told shows up as a diff to read before running the test
// with -update.
//
// The full transcript (every answer, conflict, activity and view, about 15
// MB) is too large to keep, so the golden file holds a digest: a line per ten
// steps naming what each did and how it was answered, and a hash of their
// full text. To see what changed, write the full transcript before and after
// the change with -transcript=FILE and diff the two.
//
// The clock moves on at every step, so no two claims change a file or declare
// an intent at the same instant: what the board answers then does not depend
// on the order Go happens to iterate its maps.
func TestBoardTranscript(t *testing.T) {
	var digest, full bytes.Buffer
	for _, sec := range transcriptSections {
		runTranscript(&digest, &full, sec, false)
	}
	writeTranscript(t, full.Bytes())
	compareGolden(t, "testdata/transcript.golden", digest.Bytes())
}

func writeTranscript(t *testing.T, full []byte) {
	t.Helper()
	if *transcriptFile == "" {
		return
	}
	if err := os.WriteFile(*transcriptFile, full, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBoardTranscriptWithTies runs the workload on a clock that moves in
// whole seconds and often not at all, so claims change files, declare
// intents and are seen at the same instant, and pins what the board says
// then. It runs the workload three times: an answer that depends on the
// order Go iterates a map comes out differently between runs.
func TestBoardTranscriptWithTies(t *testing.T) {
	sec := transcriptSection{name: "default policy, a clock that often stands still", seed: 4, steps: 2000, cfg: func(*Config) {}}
	var first []byte
	for run := range 3 {
		var digest, full bytes.Buffer
		runTranscript(&digest, &full, sec, true)
		if run == 0 {
			first = full.Bytes()
			if *transcriptFile != "" {
				if err := os.WriteFile(*transcriptFile+".ties", first, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			compareGolden(t, "testdata/transcript-ties.golden", digest.Bytes())
			continue
		}
		if !bytes.Equal(full.Bytes(), first) {
			g, w := strings.Split(full.String(), "\n"), strings.Split(string(first), "\n")
			for i := range min(len(g), len(w)) {
				if g[i] != w[i] {
					t.Fatalf("run %d differs from the first at line %d:\n got: %.400s\nwant: %.400s", run, i+1, g[i], w[i])
				}
			}
			t.Fatalf("run %d differs from the first in length", run)
		}
	}
}

// transcriptSection is one run of the workload, under one configuration.
type transcriptSection struct {
	name  string
	seed  int64
	steps int
	cfg   func(*Config)
}

var transcriptSections = []transcriptSection{
	{name: "default policy", seed: 1, steps: 2000, cfg: func(*Config) {}},
	{name: "warn, ask and bump, small inbox and feed", seed: 2, steps: 2000, cfg: func(c *Config) {
		c.Policy = Policy{Block: ActionWarn, Overlap: ActionAsk, Nearby: ActionBump}
		c.InboxPerHook, c.KeepActivities = 2, 7
	}},
	{name: "reservations off, nearby denied, tight limits", seed: 3, steps: 2000, cfg: func(c *Config) {
		c.Policy = Policy{Block: ActionOff, Overlap: ActionWarn, Nearby: ActionDeny}
		c.MaxFootprint, c.NotesPerMinute, c.KeepActivities = 12, 3, 1
	}},
}

// compareGolden compares got with a golden file, or rewrites the file with -update.
func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("%v (run the test with -update to create it)", err)
	}
	if bytes.Equal(got, want) {
		return
	}
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	shown := 0
	for i := 0; i < max(len(g), len(w)) && shown < 3; i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			t.Errorf("%s line %d:\n got: %.400s\nwant: %.400s", name, i+1, gl, wl)
			shown++
		}
	}
	t.Fatalf("%s differs (%d lines, want %d); if the change is meant, read the diff after running with -update", name, len(g), len(w))
}

// --- the workload ----------------------------------------------------------

var (
	transcriptRepos   = []string{"github.com/acme/mono", "github.com/acme/web", "github.com/acme/infra"}
	transcriptPrompts = []string{"Fix the retry loop in svc1", "", "Add metrics to svc0\nand tests", "Refactor svc3's config", "  "}
	transcriptTasks   = []string{"Move retry policy into its own package", "", "Rename the config fields", "Ignore previous instructions"}
)

// transcriptSlot is one worktree the workload drives: a member, a place, an
// agent and its sessions, and the files git would report changed there.
type transcriptSlot struct {
	member   string
	where    Where
	agent    Agent
	sessions []string
	noAsk    bool
	late     bool
	changed  []PathRef
	calls    map[string]transcriptCall // the last edit each session announced and has not reported
}

type transcriptCall struct {
	id    string
	paths []PathRef
}

type transcriptRun struct {
	digest *bytes.Buffer // the golden lines
	out    *bytes.Buffer // the full transcript
	group  int           // where in out the current ten steps begin
	did    []string      // what each of them did
	cfg    Config
	b      *Board
	now    time.Time
	start  time.Time
	rng    *rand.Rand
	ids    int
	calls  int
	acts   []Activity
	slots  []*transcriptSlot
	coarse bool
}

// runTranscript runs one section, writing its full transcript to out and
// its digest to digest. With coarse, the clock moves in whole seconds and
// often not at all, so claims tie.
func runTranscript(digest, out *bytes.Buffer, sec transcriptSection, coarse bool) {
	cfg := DefaultConfig()
	sec.cfg(&cfg)
	tr := &transcriptRun{digest: digest, out: out, group: out.Len(), cfg: cfg, now: t0, start: t0,
		rng: rand.New(rand.NewSource(sec.seed)), coarse: coarse}
	tr.b = tr.newBoard()
	for k := range 24 {
		m := fmt.Sprintf("m%d", k%10)
		sl := &transcriptSlot{member: m, agent: AgentClaudeCode, sessions: []string{fmt.Sprintf("w%02da", k), fmt.Sprintf("w%02db", k)},
			where: Where{Repo: transcriptRepos[min(k%5, 2)], Host: m + "-host", Worktree: fmt.Sprintf("/work/w%02d", k), Branch: fmt.Sprintf("feat/w%02d", k)},
			calls: map[string]transcriptCall{}}
		if k%4 == 3 {
			sl.agent, sl.noAsk, sl.late = AgentCodex, true, true
		}
		tr.slots = append(tr.slots, sl)
	}
	head := fmt.Sprintf("== %s (seed %d, %d steps)\n", sec.name, sec.seed, sec.steps)
	digest.WriteString(head)
	out.WriteString(head)
	tr.group = out.Len()
	for i := range sec.steps {
		tr.step(i)
		if i%10 == 9 {
			tr.digestGroup(i - 9)
		}
	}
	tr.view(sec.steps)
	tr.did = append(tr.did, "view")
	tr.digestGroup(sec.steps - sec.steps%10)
}

// digestGroup writes the golden line for the steps since the last one.
func (tr *transcriptRun) digestGroup(first int) {
	sum := sha256.Sum256(tr.out.Bytes()[tr.group:])
	fmt.Fprintf(tr.digest, "#%04d %x %s\n", first, sum[:6], strings.Join(tr.did, " "))
	tr.group, tr.did = tr.out.Len(), tr.did[:0]
}

func (tr *transcriptRun) newBoard() *Board {
	return New(tr.cfg,
		withIDs(func(prefix string) string { tr.ids++; return fmt.Sprintf("%s%04d", prefix, tr.ids) }),
		WithNotify(func(a []Activity) { tr.acts = append(tr.acts, a...) }))
}

func (tr *transcriptRun) step(i int) {
	tr.advance()
	switch r := tr.rng.Intn(1000); {
	case r < 70:
		tr.declare(i)
	case r < 100:
		tr.release(i)
	case r < 130:
		tr.note(i)
	case r < 180:
		tr.check(i)
	case r < 215:
		tr.line(i, "sweep")
		tr.b.Sweep(tr.now)
		tr.did = append(tr.did, "sweep")
	case r < 225:
		tr.view(i)
		tr.did = append(tr.did, "view")
	case r < 228:
		tr.restore(i)
	default:
		tr.hook(i)
	}
	for _, a := range tr.acts {
		tr.out.WriteString("  act " + jsonOf(a) + "\n")
	}
	tr.acts = nil
}

// advance moves the clock on, now and then past one of the board's thresholds.
func (tr *transcriptRun) advance() {
	if tr.coarse {
		tr.now = tr.now.Add(time.Duration(tr.rng.Intn(3)/2) * time.Second)
	} else {
		tr.now = tr.now.Add(time.Millisecond + time.Duration(tr.rng.Int63n(int64(20*time.Second))))
	}
	if tr.rng.Intn(100) == 0 {
		jumps := []time.Duration{11 * time.Minute, 46 * time.Minute, 2*time.Hour + time.Minute, 3*time.Hour + 5*time.Minute,
			25 * time.Hour, 7*24*time.Hour + time.Hour}
		tr.now = tr.now.Add(jumps[tr.rng.Intn(len(jumps))])
	}
}

func (tr *transcriptRun) line(i int, format string, args ...any) {
	fmt.Fprintf(tr.out, "#%04d %s %s\n", i, tr.now.Sub(tr.start), fmt.Sprintf(format, args...))
}

// outcome names a step for the digest: what it was, and how many things it
// found or changed, or that it failed.
func outcome(op string, err error, n int) string {
	if err != nil {
		return op + "!"
	}
	return fmt.Sprintf("%s:%d", op, n)
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// slot picks a worktree; the first few are busier.
func (tr *transcriptRun) slot() *transcriptSlot {
	if tr.rng.Intn(3) == 0 {
		return tr.slots[tr.rng.Intn(6)]
	}
	return tr.slots[tr.rng.Intn(len(tr.slots))]
}

// file picks a file, from hot areas more often.
func (tr *transcriptRun) file() PathRef {
	a := tr.rng.Intn(6)
	if tr.rng.Intn(2) == 0 {
		a = tr.rng.Intn(2)
	}
	pkg, f := tr.rng.Intn(3), tr.rng.Intn(6)
	if tr.rng.Intn(20) == 0 {
		return PathRef{Path: fmt.Sprintf("README%d.md", f)}
	}
	path := fmt.Sprintf("svc%d/pkg%d/f%d.go", a, pkg, f)
	return PathRef{Path: path, Area: tr.area(path)}
}

// area is a file's area as a client reports it: usually its service,
// sometimes its package (which gained a marker), sometimes none.
func (tr *transcriptRun) area(path string) string {
	dirs := strings.Split(path, "/")
	switch r := tr.rng.Intn(10); {
	case len(dirs) < 3 || r == 0:
		return ""
	case r == 1:
		return dirs[0] + "/" + dirs[1]
	}
	return dirs[0]
}

func (tr *transcriptRun) files(n int) []PathRef {
	out := make([]PathRef, n)
	for i := range out {
		out[i] = tr.file()
	}
	return out
}

// footprint changes what git finds in a worktree (a merge drops files, a
// shell command adds some) and reports it.
func (tr *transcriptRun) footprint(sl *transcriptSlot) *Footprint {
	if tr.rng.Intn(3) == 0 {
		sl.changed = slices.DeleteFunc(sl.changed, func(PathRef) bool { return tr.rng.Intn(3) == 0 })
	}
	if tr.rng.Intn(3) == 0 {
		sl.changed = append(sl.changed, tr.files(1+tr.rng.Intn(5))...)
	}
	fp := &Footprint{Files: slices.Clone(sl.changed)}
	for i := range fp.Files {
		if tr.rng.Intn(8) == 0 {
			fp.Files[i].Area = tr.area(fp.Files[i].Path) // the same file, its area seen anew
		}
	}
	switch tr.rng.Intn(20) {
	case 0:
		fp.Truncated = true
	case 1:
		fp.Files = append(fp.Files, PathRef{Path: "../outside.go"})
	}
	return fp
}

func (tr *transcriptRun) callID() string {
	if tr.rng.Intn(5) == 0 {
		return ""
	}
	tr.calls++
	return fmt.Sprintf("t%d", tr.calls)
}

var transcriptKinds = []struct {
	kind   Kind
	weight int
}{
	{KindSessionStart, 6}, {KindPrompt, 8}, {KindPreEdit, 26}, {KindPostEdit, 20}, {KindToolStart, 8}, {KindToolEnd, 8},
	{KindStop, 6}, {KindSessionEnd, 4}, {KindHeartbeat, 4},
}

func (tr *transcriptRun) kind() Kind {
	n := 0
	for _, k := range transcriptKinds {
		n += k.weight
	}
	r := tr.rng.Intn(n)
	for _, k := range transcriptKinds {
		if r < k.weight {
			return k.kind
		}
		r -= k.weight
	}
	panic("unreachable")
}

func (tr *transcriptRun) hook(i int) {
	sl := tr.slot()
	sid := sl.sessions[tr.rng.Intn(len(sl.sessions))]
	ev := HookEvent{Kind: tr.kind(), Member: sl.member, Agent: sl.agent, SessionID: sid, Where: sl.where, NoAsk: sl.noAsk, LateContext: sl.late}
	switch ev.Kind {
	case KindPreEdit:
		n := 1
		if tr.rng.Intn(4) == 0 {
			n += tr.rng.Intn(4) // an edit of several files
		}
		ev.Tool, ev.ToolUseID, ev.Paths = "Edit", tr.callID(), tr.files(n)
		sl.calls[sid] = transcriptCall{id: ev.ToolUseID, paths: ev.Paths}
	case KindPostEdit:
		ev.Tool = "Edit"
		if c, ok := sl.calls[sid]; ok && tr.rng.Intn(10) < 7 {
			ev.ToolUseID, ev.Paths = c.id, c.paths
			delete(sl.calls, sid)
		} else {
			ev.ToolUseID, ev.Paths = tr.callID(), tr.files(1+tr.rng.Intn(2))
		}
		sl.changed = append(sl.changed, ev.Paths...)
	case KindToolStart, KindToolEnd:
		ev.Tool, ev.ToolUseID = "Bash", tr.callID()
		if ev.Kind == KindToolEnd && tr.rng.Intn(2) == 0 {
			ev.Footprint = tr.footprint(sl)
		}
	case KindPrompt:
		ev.Prompt = transcriptPrompts[tr.rng.Intn(len(transcriptPrompts))]
	default:
		if tr.rng.Intn(5) < 3 {
			ev.Footprint = tr.footprint(sl)
		}
	}
	switch tr.rng.Intn(100) {
	case 0:
		ev.Kind = "subagent_stop" // from a newer client
	case 1:
		ev.Paths = append(ev.Paths, PathRef{Path: "svc1/bad\nname.go"})
	case 2:
		ev.Where.Branch = fmt.Sprintf("feat/%s-v2", sl.member)
	}
	res, err := tr.b.Hook(tr.now, ev)
	pending := ""
	if s := tr.b.sessions[sessionKey(ev.Member, ev.Agent, ev.SessionID)]; s != nil {
		pending = s.Pending
	}
	files := 0
	if ev.Footprint != nil {
		files = len(ev.Footprint.Files)
	}
	tr.line(i, "hook %s %s/%s paths=%s footprint=%d -> err=%v decision=%s claim=%s reason=%q context=%q pending=%q conflicts=%s",
		ev.Kind, sl.where.Worktree, sid, jsonOf(ev.Paths), files, err, res.Decision, res.ClaimID, res.Reason, res.Context, pending, jsonOf(res.Conflicts))
	did := string(ev.Kind)
	switch {
	case err != nil:
		did += "!"
	case res.Decision != DecisionAllow:
		did += ":" + string(res.Decision)
	case res.Context != "":
		did += ":told"
	}
	tr.did = append(tr.did, did)
}

func (tr *transcriptRun) pattern() string {
	a, pkg, f := tr.rng.Intn(6), tr.rng.Intn(3), tr.rng.Intn(6)
	switch tr.rng.Intn(9) {
	case 0:
		return fmt.Sprintf("svc%d/**", a)
	case 1:
		return fmt.Sprintf("svc%d/pkg%d", a, pkg)
	case 2:
		return fmt.Sprintf("svc%d/pkg%d/f%d.go", a, pkg, f)
	case 3:
		return fmt.Sprintf("svc%d/*/f%d*.go", a, f)
	case 4:
		return fmt.Sprintf("**/f%d.go", f)
	case 5:
		return fmt.Sprintf("svc%d/pkg[01]/**", a)
	case 6:
		return "*.md"
	case 7:
		return fmt.Sprintf("svc%d/pkg%d/", a, pkg)
	}
	if tr.rng.Intn(4) == 0 {
		return "svc1/**x" // invalid
	}
	return fmt.Sprintf("svc%d/pkg%d/f%d.go", a, pkg, f)
}

func (tr *transcriptRun) declare(i int) {
	sl := tr.slot()
	pats := make([]string, 1+tr.rng.Intn(3))
	for k := range pats {
		pats[k] = tr.pattern()
	}
	mode := ModeShared
	if tr.rng.Intn(5) < 2 {
		mode = ModeExclusive
	}
	res, err := tr.b.Declare(tr.now, DeclareRequest{Member: sl.member, Where: sl.where, Mode: mode, Patterns: pats,
		Summary: transcriptTasks[tr.rng.Intn(len(transcriptTasks))]})
	tr.line(i, "declare %s %s %s -> err=%v %s", sl.where.Worktree, mode, jsonOf(pats), err, jsonOf(res))
	tr.did = append(tr.did, outcome("declare", err, len(res.Overlaps)))
}

func (tr *transcriptRun) release(i int) {
	sl := tr.slot()
	var pats []string
	if tr.rng.Intn(2) == 0 {
		pats = []string{tr.pattern()}
	}
	n, err := tr.b.Release(tr.now, ReleaseRequest{Member: sl.member, Where: sl.where, Patterns: pats})
	tr.line(i, "release %s %s -> err=%v released=%d", sl.where.Worktree, jsonOf(pats), err, n)
	tr.did = append(tr.did, outcome("release", err, n))
}

func (tr *transcriptRun) note(i int) {
	sl := tr.slot()
	var to string
	switch tr.rng.Intn(4) {
	case 0:
		to = tr.slots[tr.rng.Intn(len(tr.slots))].member
	case 1:
		ids := make([]string, 0, len(tr.b.claims))
		for id := range tr.b.claims {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		if to = "c_none"; len(ids) > 0 {
			to = ids[tr.rng.Intn(len(ids))]
		}
	case 2:
		to = tr.file().Path
	default:
		to = "nobody"
	}
	// Sometimes a burst to a teammate in the repository, which a tight limit
	// cuts short.
	burst := 1
	if tr.rng.Intn(4) == 0 {
		burst = 4
		for _, c := range tr.b.claimsInRepo(sl.where.Repo) {
			if c.Member != sl.member {
				to = c.Member
				break
			}
		}
	}
	byPerson := tr.rng.Intn(3) == 0
	for range burst {
		res, err := tr.b.Note(tr.now, NoteRequest{Member: sl.member, Where: sl.where, To: to, Text: "Heads up about " + to, ByPerson: byPerson})
		tr.line(i, "note %s to=%s -> err=%v %s", sl.where.Worktree, to, err, jsonOf(res))
		tr.did = append(tr.did, outcome("note", err, len(res.Delivered)))
	}
}

func (tr *transcriptRun) check(i int) {
	sl := tr.slot()
	paths := tr.files(1 + tr.rng.Intn(3))
	cs, err := tr.b.Check(tr.now, CheckRequest{Member: sl.member, Where: sl.where, Paths: paths})
	tr.line(i, "check %s %s -> err=%v %s text=%q", sl.where.Worktree, jsonOf(paths), err, jsonOf(cs), RenderConflicts(tr.now, cs))
	tr.did = append(tr.did, outcome("check", err, len(cs)))
}

// view writes what the dashboard and the CLI show: every repository's view,
// and the list of repositories.
func (tr *transcriptRun) view(i int) {
	for _, r := range transcriptRepos {
		v := tr.b.View(tr.now, r)
		tr.line(i, "view %s %s", r, jsonOf(v))
		tr.line(i, "view text %q", v.Text())
	}
	tr.line(i, "repos %s", jsonOf(tr.b.Repos(tr.now)))
}

// restore saves the board and restores it into a new one, as a restart does.
func (tr *transcriptRun) restore(i int) {
	data, _, err := tr.b.Snapshot(tr.now)
	if err != nil {
		panic(err)
	}
	tr.b = tr.newBoard()
	err = tr.b.Restore(data)
	tr.line(i, "restore -> err=%v claims=%d sessions=%d", err, len(tr.b.claims), len(tr.b.sessions))
	tr.did = append(tr.did, outcome("restore", err, len(tr.b.claims)))
}
