// Package board is intagent's domain: who is working where, on what, and what
// another agent should hear before it writes a file.
//
// A Board is safe for concurrent use. Every method takes the current time
// instead of reading a clock, so behaviour over time is easy to test.
package board

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patakil/intagent/internal/glob"
)

// Errors returned by Board methods.
var (
	ErrInvalid     = errors.New("invalid request")
	ErrRateLimited = errors.New("too many notes; try again in a minute")
	ErrNoTarget    = errors.New("nobody by that name, claim or path has work in this repository right now")
	// ErrAbandoned reports a hook the board did not answer because its agent
	// had stopped waiting (HookEvent.Late); the agent went ahead unchecked.
	ErrAbandoned = errors.New("the agent stopped waiting for the answer")
)

// Config holds the board's timings and policy. A zero or negative field, and
// an empty policy action, takes its value from DefaultConfig.
type Config struct {
	// StallAfter is how long a working session may be silent before it counts as stalled.
	StallAfter time.Duration `json:"stall_after"`
	// ToolStallAfter replaces StallAfter while a tool call is in progress.
	ToolStallAfter time.Duration `json:"tool_stall_after"`
	// IdleAfter is how long any session may be silent before it counts as gone.
	IdleAfter time.Duration `json:"idle_after"`
	// DormantFor is how long a dormant claim's changed files still count as an
	// overlap; after that they only count as nearby.
	DormantFor time.Duration `json:"dormant_for"`
	// ForgetAfter removes claims with no live session and no activity.
	ForgetAfter time.Duration `json:"forget_after"`
	// Policy maps severities to actions.
	Policy Policy `json:"policy"`
	// NotesPerMinute caps the notes one member can send.
	NotesPerMinute int `json:"notes_per_minute"`
	// MaxFootprint caps the files kept per claim.
	MaxFootprint int `json:"max_footprint"`
	// InboxPerHook caps the inbox items delivered in one hook response.
	InboxPerHook int `json:"inbox_per_hook"`
	// KeepActivities is the length of the recent-activity feed.
	KeepActivities int `json:"keep_activities"`
	// MaxSessions caps the sessions the board keeps (bounds.go).
	MaxSessions int `json:"max_sessions"`
	// MaxDormantClaims caps the claims with no live session the board keeps.
	MaxDormantClaims int `json:"max_dormant_claims"`
	// MaxFootprintBytes and MemberFootprintBytes cap what one claim's
	// footprint, and all of one member's, may hold (bounds.go).
	MaxFootprintBytes    int `json:"max_footprint_bytes"`
	MemberFootprintBytes int `json:"member_footprint_bytes"`
}

// DefaultConfig returns the timings described in docs/design.md.
func DefaultConfig() Config {
	return Config{
		StallAfter:     10 * time.Minute,
		ToolStallAfter: 45 * time.Minute,
		IdleAfter:      2 * time.Hour,
		DormantFor:     24 * time.Hour,
		ForgetAfter:    7 * 24 * time.Hour,
		Policy:         DefaultPolicy(),
		NotesPerMinute: 20,
		MaxFootprint:   2000,
		InboxPerHook:   5,
		KeepActivities: 300,
		// A thousand sessions and a week's dormant claims are the scale
		// intagent is meant for; these are twenty times that.
		MaxSessions:          20_000,
		MaxDormantClaims:     20_000,
		MaxFootprintBytes:    defaultFootprintBytes,
		MemberFootprintBytes: defaultMemberFootprintBytes,
	}
}

// WithDefaults returns c with the fields left unset taken from DefaultConfig.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	orDuration := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	orInt := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	orAction := func(v *Action, def Action) {
		if *v == "" {
			*v = def
		}
	}
	orDuration(&c.StallAfter, d.StallAfter)
	orDuration(&c.ToolStallAfter, d.ToolStallAfter)
	orDuration(&c.IdleAfter, d.IdleAfter)
	orDuration(&c.DormantFor, d.DormantFor)
	orDuration(&c.ForgetAfter, d.ForgetAfter)
	orAction(&c.Policy.Block, d.Policy.Block)
	orAction(&c.Policy.Overlap, d.Policy.Overlap)
	orAction(&c.Policy.Nearby, d.Policy.Nearby)
	orInt(&c.NotesPerMinute, d.NotesPerMinute)
	orInt(&c.MaxFootprint, d.MaxFootprint)
	orInt(&c.InboxPerHook, d.InboxPerHook)
	orInt(&c.KeepActivities, d.KeepActivities)
	orInt(&c.MaxSessions, d.MaxSessions)
	orInt(&c.MaxDormantClaims, d.MaxDormantClaims)
	orInt(&c.MaxFootprintBytes, d.MaxFootprintBytes)
	orInt(&c.MemberFootprintBytes, d.MemberFootprintBytes)
	return c
}

const (
	maxTaskLen   = 200
	maxPrompt    = 8 << 10 // the part of a prompt's first line the board reads
	maxNoteLen   = 600
	maxBranchLen = 120
	maxIntents   = 50
	maxInbox     = 50
	inboxTTL     = 24 * time.Hour
	keepEndedFor = time.Hour
	noteWindow   = time.Minute
	idLen        = 8 // characters of an ID after its prefix
)

// maxFootprintDirs bounds the directories a footprint names in place of the
// files it leaves out (Footprint.Dirs), as intagent's client bounds them.
const maxFootprintDirs = 200

// maxShownPaths bounds the paths an activity lists, since the feed, the
// snapshot and every stream carry them; MorePaths counts the rest.
const maxShownPaths = 200

// maxCheckPaths bounds the paths of one edit, or one check, compared with
// teammates' work. Each path costs a look at each of the repository's
// claims, under the board's lock: on the target board's busy repository (300
// claims, typical or mixed footprints, 70% of them with intents) an edit of
// 200 files holds it for 14 ms, and one of 2000 would hold it for 105 to
// 120 ms and spend 40% of what one call may match (maxGlobWork); one
// member's checks, at the 5 a second the server allows, would then hold it
// for more than half of every second. Answers are bounded (boundConflicts),
// but that cost is why it stays below MaxFootprint.
// Past it, an edit's agent is told what was not checked, and a check's
// answer counts and names what it did not check. It may exceed
// maxShownPaths: an edit's activity lists the paths that decided its answer
// first.
const maxCheckPaths = 200

// Board holds every claim and session the server knows about.
type Board struct {
	mu       sync.Mutex
	notifyMu sync.Mutex // orders notifications; taken before mu is released
	cfg      Config
	claims   map[string]*claim
	byKey    map[string]string
	sessions map[string]*session
	// byRepo and claimSessions index claims and sessions (index.go).
	byRepo        map[string]*repoIndex
	claimSessions map[string]map[string]*session
	memberBytes   map[string]int
	recent        []Activity
	seq           uint64
	version       uint64
	notes         map[string][]time.Time
	stats         map[string]*Stats
	statsAt       map[string]time.Time // when each repository was last counted in
	pending       []Activity
	notify        func([]Activity)
	newID         func(prefix string) string
	// unheard is set while Hook records an event whose answer nobody will
	// read: nothing is delivered in it.
	unheard bool
	// work is the matching left to the call that holds the lock (lock).
	work glob.Work
	log  *slog.Logger
	// viewCopied, when a test sets it, runs once View has let go of the lock.
	viewCopied func()

	// dropped is, by repository, the seq of the newest activity the feed has
	// let go of, and droppedAll the newest of all; droppedFloor covers what a
	// restart let go of, whose repositories are not known. A replay from an
	// older seq misses something.
	dropped      map[string]uint64
	droppedAll   uint64
	droppedFloor uint64

	// unpruned holds the repositories a claim was removed from since the
	// last sweep, whose claims may remember alerts of it (pruneAlerts).
	unpruned map[string]bool
	// mail holds the notes waiting for members, by repository and member
	// (mail.go).
	mail map[string]*mailbox
}

// Option configures a Board.
type Option func(*Board)

// WithNotify registers fn to receive new activities. It is called without the
// board's lock held, one call at a time, in the order activities happened; it
// must not block or call back into the board.
func WithNotify(fn func([]Activity)) Option { return func(b *Board) { b.notify = fn } }

// WithLogger has the board log what it drops from a snapshot it restores.
func WithLogger(l *slog.Logger) Option {
	return func(b *Board) {
		if l != nil {
			b.log = l
		}
	}
}

// withIDs replaces the random ID generator, for tests.
func withIDs(fn func(prefix string) string) Option { return func(b *Board) { b.newID = fn } }

// New returns an empty board. Fields of cfg left unset take their defaults.
func New(cfg Config, opts ...Option) *Board {
	b := &Board{
		cfg:           cfg.WithDefaults(),
		claims:        map[string]*claim{},
		byKey:         map[string]string{},
		sessions:      map[string]*session{},
		byRepo:        map[string]*repoIndex{},
		claimSessions: map[string]map[string]*session{},
		memberBytes:   map[string]int{},
		notes:         map[string][]time.Time{},
		stats:         map[string]*Stats{},
		statsAt:       map[string]time.Time{},
		dropped:       map[string]uint64{},
		unpruned:      map[string]bool{},
		mail:          map[string]*mailbox{},
		newID:         randomID,
		log:           slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func randomID(prefix string) string {
	var buf [5]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))[:idLen]
}

// Config returns the board's configuration.
func (b *Board) Config() Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// Version increases with every change, so a persister can tell when to save.
func (b *Board) Version() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

// maxGlobWork bounds the matching of paths and patterns one call may do
// under the board's lock, in glob.Work's units. On the target board an edit
// of 200 files uses about a seventh of it, and a worktree arriving with 2000
// changed files or a declaration of 50 patterns about a fortieth; the
// costliest patterns glob.CleanPattern accepts use it up in 50 to 85 ms.
// Past the bound, the call compares no more (comparing), lets through what
// it did not compare, and says so (partialNote).
const maxGlobWork = 2_000_000

// lock takes the board's lock for a call that may match paths and patterns,
// with its own bound on that work.
func (b *Board) lock() {
	b.mu.Lock()
	b.work = glob.NewWork(maxGlobWork)
}

// match is glob.Match, counted and charged to the work the call holding
// the board's lock may do.
func (b *Board) match(pattern, name string) bool {
	if trace != nil {
		trace.globMatch++
		if b.work.Short() {
			trace.spentMatches++
		}
	}
	return b.work.Match(pattern, name)
}

// overlap is glob.Overlap, counted and charged as match is.
func (b *Board) overlap(x, y string) bool {
	if trace != nil {
		trace.globOverlap++
		if b.work.Short() {
			trace.spentMatches++
		}
	}
	return b.work.Overlap(x, y)
}

// countPartial counts, in repo's stats, a call that stopped matching for
// want of work.
func (b *Board) countPartial(now time.Time, repo string) {
	if b.work.Short() {
		b.statsOf(repo, now).Partial++
	}
}

// comparing yields a repository's claims in ID order, as claimsIn does, until
// the call runs out of matching: past that, the call compares no more claims,
// and lets through what it did not compare. The order makes a call stop at
// the same claim every time.
func (b *Board) comparing(repo string) iter.Seq[*claim] {
	return func(yield func(*claim) bool) {
		for c := range b.claimsIn(repo) {
			if b.work.Short() || !yield(c) {
				return
			}
		}
	}
}

// unlock releases the lock and then hands pending activities to the notifier.
// The notifier's own lock is taken before the board's is released, so two
// writers cannot hand over their activities out of order: a stream that skips
// what it has seen would otherwise lose the earlier ones for good.
func (b *Board) unlock() {
	acts := b.pending
	b.pending = nil
	if len(acts) == 0 || b.notify == nil {
		b.mu.Unlock()
		return
	}
	b.notifyMu.Lock()
	b.mu.Unlock()
	defer b.notifyMu.Unlock()
	b.notify(acts)
}

func (b *Board) record(a Activity) {
	b.seq++
	a.Seq = b.seq
	if over := len(a.Paths) - maxShownPaths; over > 0 {
		a.Paths, a.MorePaths = slices.Clone(a.Paths[:maxShownPaths]), a.MorePaths+over
	}
	b.recent = append(b.recent, a)
	if over := len(b.recent) - b.cfg.KeepActivities; over > 0 {
		// Slice past the oldest rather than copy the feed: append moves it to
		// a new array only when it runs out of room, about once in
		// KeepActivities records. Clear what is dropped, so the old array
		// does not keep it alive meanwhile; letGo notes its seqs first.
		b.letGo(b.recent[:over])
		clear(b.recent[:over])
		b.recent = b.recent[over:]
	}
	b.pending = append(b.pending, a)
}

// maxDroppedRepos bounds the repositories the board remembers dropping
// activities of; past it they are folded into one floor, which can only make
// a replay say it misses something when it does not.
const maxDroppedRepos = 1000

// letGo notes the activities the feed lets go of, so a replay can tell when
// it misses one.
func (b *Board) letGo(acts []Activity) {
	for _, a := range acts {
		b.dropped[a.Repo] = a.Seq
		b.droppedAll = a.Seq
	}
	if len(b.dropped) > maxDroppedRepos {
		b.droppedFloor = b.droppedAll
		clear(b.dropped)
	}
}

func (b *Board) changed() { b.version++ }

// --- validation -----------------------------------------------------------

func cleanWhere(w Where) (Where, error) {
	w.Repo = RepoID(w.Repo)
	w.Host = Clean(w.Host, 100)
	w.Worktree = Clean(w.Worktree, 500)
	w.Branch = ident(w.Branch, maxBranchLen)
	if w.Repo == "" || w.Host == "" || w.Worktree == "" {
		return w, fmt.Errorf("%w: repo, host and worktree are required", ErrInvalid)
	}
	return w, nil
}

// cleanPaths keeps the valid, distinct paths among the first limit.
func cleanPaths(in []PathRef, limit int) ([]PathRef, error) {
	in = in[:min(len(in), limit)]
	out := make([]PathRef, 0, len(in))
	seen := map[string]bool{}
	var first error
	for _, p := range in {
		c, err := glob.CleanPath(p.Path)
		if err != nil {
			// One path intagent cannot show (a control character, say) must not
			// let the rest of the edit through unchecked.
			if first == nil {
				first = err
			}
			continue
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		area := ""
		if p.Area != "" {
			if a, err := glob.CleanPath(p.Area); err == nil {
				area = a
			}
		}
		out = append(out, PathRef{Path: c, Area: area})
	}
	if len(out) == 0 && first != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, first)
	}
	return out, nil
}

// --- lookups --------------------------------------------------------------

func (b *Board) findClaim(member string, w Where) *claim {
	if id, ok := b.byKey[claimKey(w.Repo, member, w.Host, w.Worktree)]; ok {
		return b.claims[id]
	}
	return nil
}

func (b *Board) claimFor(now time.Time, member string, w Where) *claim {
	if c := b.findClaim(member, w); c != nil {
		if w.Branch != "" {
			c.Branch = w.Branch
		}
		return c
	}
	c := &claim{
		ID:        b.newID("c_"),
		Repo:      w.Repo,
		Member:    member,
		Host:      w.Host,
		Worktree:  w.Worktree,
		Branch:    w.Branch,
		Footprint: map[string]*touch{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	b.addClaim(c)
	b.record(Activity{At: now, Kind: ActivityClaimOpened, Repo: c.Repo, Member: member, ClaimID: c.ID, Text: c.Branch})
	return c
}

func (b *Board) sessionFor(now time.Time, ev HookEvent, c *claim) *session {
	key := sessionKey(ev.Member, ev.Agent, ev.SessionID)
	s, ok := b.sessions[key]
	if !ok {
		s = &session{
			Key:       key,
			ID:        ev.SessionID,
			Member:    ev.Member,
			Agent:     ev.Agent,
			StartedAt: now,
			LastSeen:  now,
			Phase:     phaseWaiting,
		}
		b.record(Activity{At: now, Kind: ActivitySessionStarted, Repo: c.Repo, Member: ev.Member, ClaimID: c.ID, Session: ev.SessionID, Agent: ev.Agent})
	}
	if !ok || s.ClaimID != c.ID {
		b.attachSession(s, c.ID)
		b.trimSessions(now, c, s)
	}
	return s
}

// state derives a session's liveness.
func (b *Board) state(now time.Time, s *session) State {
	if s.Phase == phaseEnded {
		return StateEnded
	}
	silent := now.Sub(s.LastSeen)
	if silent > b.cfg.IdleAfter {
		return StateGone
	}
	if s.Phase == phaseWorking {
		limit := b.cfg.StallAfter
		if s.Tool != "" {
			limit = b.cfg.ToolStallAfter
		}
		if silent > limit {
			return StateStalled
		}
		return StateWorking
	}
	return StateWaiting
}

// --- hook events ----------------------------------------------------------

// Hook applies one lifecycle event and returns what the agent should be told.
// It never refuses on error: callers should allow the agent to continue.
//
// An event whose agent has stopped waiting (ev.Late) is not answered: an
// edit about to be made returns ErrAbandoned having changed nothing, and any
// other event is recorded without delivering anything.
func (b *Board) Hook(now time.Time, ev HookEvent) (HookResult, error) {
	allow := HookResult{Decision: DecisionAllow}
	named := len(ev.Paths)
	ev, err := ev.clean(b.cfg.MaxFootprint) // the config never changes after New
	if err != nil {
		return allow, err
	}
	w := ev.Where

	b.lock()
	defer b.unlock()
	// Asked once the lock is held: a hook queued behind other work can reach
	// the board after its agent went ahead without the answer.
	late := ev.Late != nil && ev.Late()
	if late && ev.Kind == KindPreEdit {
		return b.abandon(now, ev), ErrAbandoned
	}
	if b.full(ev) {
		return b.unstored(now, ev), nil
	}
	b.changed()
	b.unheard = late
	defer func() { b.unheard = false }()
	defer b.countPartial(now, w.Repo)

	c := b.claimFor(now, ev.Member, w)
	s := b.sessionFor(now, ev, c)
	checkedAfter := ev.Kind == KindPostEdit && ev.ToolUseID != "" && !s.Calls[ev.ToolUseID]
	if checkedAfter {
		b.unansweredEdit(now, c, s, ev)
	}
	// What the session was last announced as, not what the clock makes of
	// it: a session back before a sweep announced it stalled has nothing to
	// recover from.
	was := s.Reported
	res := HookResult{Decision: DecisionAllow, ClaimID: c.ID, CheckedAfter: checkedAfter}

	switch ev.Kind {
	case KindSessionStart:
		if s.Phase == phaseEnded {
			s.Phase = phaseWaiting
		}
		clearTools(s)
		b.reconcile(now, c, s, ev.Footprint)
		res.Context = joinBlocks(b.renderStart(now, c), b.deliver(now, c, s))
	case KindPrompt:
		s.Phase = phaseWorking
		clearTools(s)
		b.taskFromPrompt(c, s, ev.Prompt)
		res.Context = b.deliver(now, c, s)
	case KindToolStart:
		startTool(now, s, ev.Tool, ev.ToolUseID)
	case KindPreEdit:
		startTool(now, s, ev.Tool, ev.ToolUseID)
		var spent []string
		res, spent = b.decide(now, c, s, ev.Paths, ev.NoAsk)
		res.ClaimID = c.ID
		if len(ev.Paths) > maxCheckPaths || named > b.cfg.MaxFootprint {
			res.Context = joinBlocks(res.Context, fmt.Sprintf("[intagent] This edit names %d files; intagent checked only "+
				"the first %d against teammates' work. Check the others with the intagent check_paths tool, %d at a time, "+
				"or tell your user.", named, min(len(ev.Paths), maxCheckPaths), maxCheckPaths))
		}
		if b.work.Short() {
			res.Context = joinBlocks(res.Context, prefix+" "+partialNote+" It let this edit through on what it had not "+
				"compared: tell your user if the edit may touch a teammate's work.")
		}
		if res.Decision == DecisionRefuse || (res.Decision == DecisionAsk && ev.NoAsk) {
			// The edit does not run, so no tool end will follow it. (An ask that
			// the person answers runs, or not, and the agent reports either.)
			refuseTool(s, ev.ToolUseID, spent)
		} else if ev.LateContext && res.Context != "" {
			s.Pending, res.Context = joinBlocks(s.Pending, res.Context), ""
		}
	case KindPostEdit:
		endTool(s, ev.ToolUseID)
		b.touch(now, c, s, ev.Paths)
		res.Context = b.deliver(now, c, s)
	case KindToolEnd:
		endTool(s, ev.ToolUseID)
		// A shell command may have written files without saying which.
		b.reconcile(now, c, s, ev.Footprint)
		res.Context = b.deliver(now, c, s)
	case KindStop:
		s.Phase = phaseWaiting
		clearTools(s)
		b.reconcile(now, c, s, ev.Footprint)
	case KindSessionEnd:
		s.Phase = phaseEnded
		clearTools(s)
		b.reconcile(now, c, s, ev.Footprint)
		b.record(Activity{At: now, Kind: ActivitySessionEnded, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent})
	case KindHeartbeat:
		if s.Phase == phaseEnded {
			s.Phase = phaseWaiting
		}
		b.reconcile(now, c, s, ev.Footprint)
		res.Context = b.deliver(now, c, s)
	default:
		return allow, fmt.Errorf("%w: unknown event kind %q", ErrInvalid, ev.Kind)
	}

	res.Partial = b.work.Short()
	s.LastSeen = now
	c.UpdatedAt = now
	if is := b.state(now, s); is != was && (was == StateStalled || was == StateGone) {
		b.record(Activity{At: now, Kind: ActivitySessionRecovered, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent})
	}
	s.Reported = b.state(now, s)
	if ev.Kind == KindSessionEnd {
		b.releaseIfDone(now, c)
	}
	return res, nil
}

// clean validates an event, before anything is created for it, and bounds
// its fields. It runs before the board's lock is taken, so that how much a
// client sends does not decide how long others wait for the lock.
func (ev HookEvent) clean(maxFootprint int) (HookEvent, error) {
	w, err := cleanWhere(ev.Where)
	if err != nil {
		return ev, err
	}
	ev.Where = w
	ev.SessionID = Clean(ev.SessionID, 200)
	ev.Agent = Agent(ident(string(ev.Agent), 40))
	ev.Tool = ident(ev.Tool, 60)
	ev.ToolUseID = Clean(ev.ToolUseID, 200)
	if ev.Member == "" || ev.SessionID == "" || ev.Agent == "" {
		return ev, fmt.Errorf("%w: member, agent and session_id are required", ErrInvalid)
	}
	if !knownKinds[ev.Kind] {
		// An event this board does not know (from a newer client, say) must
		// not bring a dormant claim back to life.
		return ev, fmt.Errorf("%w: unknown event kind %q", ErrInvalid, ev.Kind)
	}
	if fp := ev.Footprint; fp != nil {
		// A copy, so that the caller's is left as it was, with every field
		// clean does not know of kept.
		cut := *fp
		cut.Files = cleanFootprint(fp.Files, maxFootprint)
		if fp.Dirs != nil {
			cut.Dirs = cleanFootprint(fp.Dirs, maxFootprintDirs)
		}
		cut.Truncated = fp.Truncated || len(fp.Files) > len(cut.Files) || len(fp.Dirs) > len(cut.Dirs)
		cut.AgeMS = min(max(fp.AgeMS, 0), maxScanAge.Milliseconds())
		ev.Footprint = &cut
	}
	ev.Prompt = PromptLine(ev.Prompt)
	// A post_edit's paths join the claim's files, as many as it keeps; a
	// pre_edit's first maxCheckPaths are checked.
	ev.Paths, err = cleanPaths(ev.Paths, maxFootprint)
	return ev, err
}

var knownKinds = map[Kind]bool{
	KindSessionStart: true, KindPrompt: true, KindPreEdit: true, KindPostEdit: true, KindToolStart: true,
	KindToolEnd: true, KindStop: true, KindSessionEnd: true, KindHeartbeat: true,
}

// startTool records a tool call starting. Agents run tools in parallel, so
// the session counts them: it is inside a tool, with the longer stall
// threshold, until the last one ends.
//
// Calls are tracked by the id the agent gives them, so an end reported twice
// (a refused call that the agent also reports as failed) or never cannot
// shift the count; calls without an id are counted.
func startTool(now time.Time, s *session, tool, id string) {
	s.Phase = phaseWorking
	if !inTool(s) {
		s.ToolSince = now
	}
	if id == "" {
		s.InFlight++
	} else {
		if s.Calls == nil {
			s.Calls = map[string]bool{}
		}
		s.Calls[id] = true
	}
	s.Tool = tool
}

// endTool records a tool call ending.
func endTool(s *session, id string) {
	s.Phase = phaseWorking
	switch {
	case id != "":
		delete(s.Calls, id)
	case s.InFlight > 0:
		s.InFlight--
	}
	if !inTool(s) {
		clearTools(s)
	}
}

func inTool(s *session) bool { return s.InFlight > 0 || len(s.Calls) > 0 }

// clearTools forgets the tools in flight, at the boundaries where none can be:
// a prompt, a stop, the session's start and end. An end event that never came
// cannot keep a session inside a tool past them.
func clearTools(s *session) {
	s.InFlight, s.Calls, s.Tool, s.ToolSince = 0, nil, "", time.Time{}
}

func (b *Board) taskFromPrompt(c *claim, s *session, prompt string) {
	prompt = Clean(firstLine(prompt), maxTaskLen)
	if prompt == "" || s.Acked[ackPrompted] {
		return
	}
	if s.Acked == nil {
		s.Acked = map[string]bool{}
	}
	s.Acked[ackPrompted] = true
	if c.Task == "" || !c.taskFromIntent() {
		c.Task = prompt
	}
}

// taskFromIntent reports whether the task came from a declared intent, which
// a prompt must not overwrite.
func (c *claim) taskFromIntent() bool {
	for _, in := range c.Intents {
		if in.Summary != "" && in.Summary == c.Task {
			return true
		}
	}
	return false
}

// PromptLine is the part of a prompt the board reads, of which a task is the
// first maxTaskLen characters: its first line, up to 8 KB. Clients send no
// more, so that a prompt with a log pasted into it stays a small request.
func PromptLine(prompt string) string {
	if prompt = firstLine(prompt); len(prompt) > maxPrompt {
		prompt = prompt[:maxPrompt]
	}
	return prompt
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// reconcile replaces a claim's footprint with what git reports, as
// HookEvent.clean left it, but never loses for it a change a hook reported
// that the report may not show: one made after the scan began, or one the
// list, cut short, leaves out.
func (b *Board) reconcile(now time.Time, c *claim, s *session, fp *Footprint) {
	if fp == nil {
		return
	}
	next, paths, added, cut := b.reconciled(now, c, fp)
	moved := false // a file's area changed
	for p, t := range next {
		if old, ok := c.Footprint[p]; ok && old != t {
			moved = true
			break
		}
	}
	// The files are distinct, so those kept are the ones not added.
	removed := len(c.Footprint) - (len(next) - len(added))
	if len(added) > 0 || removed > 0 || moved {
		b.setFootprint(c, next, paths)
	}
	// The directories, in what room the budgets leave once the files are kept.
	dirs, short := addedDirs(now, c.Dirs, fp.Dirs, b.footprintRoom(c, false)-(c.fpBytes-dirsCost(c.Dirs)))
	b.setDirs(c, dirs)
	c.FootprintTruncated = fp.Truncated || cut || short
	if len(added) > 0 || removed > 0 {
		b.record(Activity{At: now, Kind: ActivityFootprintReconciled, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
			Text: fmt.Sprintf("%d changed files (+%d, -%d)", len(next), len(added), removed)})
	}
	if len(added) > 0 {
		// Breaches first: they are what this session must hear, and the
		// alerts to teammates, which compare the files with every claim in
		// the repository, must not use up the matching the call may do
		// before they are found.
		b.reportUnchecked(now, c, s, added)
		b.alertOthers(now, c, added)
		b.tellShort(s)
	}
}

// addedDirs is the directories a scan says its worktree added whole, each
// with when the board first heard of it (in had), in a new map: a snapshot
// may share the one before. They count against the footprint's byte budgets
// as files do (footprintCost), so it keeps those that fit in room bytes, in
// the order the client sent them, and reports whether it left any out.
func addedDirs(now time.Time, had map[string]*touch, dirs []PathRef, room int) (map[string]*touch, bool) {
	if len(dirs) == 0 {
		return nil, false
	}
	out := make(map[string]*touch, len(dirs))
	for _, d := range dirs {
		t := had[d.Path]
		if t == nil || t.Area != d.Area {
			t = &touch{Area: d.Area, At: now, FromGit: true}
		}
		if room -= footprintCost(d.Path, t); room < 0 {
			return out, true
		}
		out[d.Path] = t
	}
	return out, false
}

// dirAbove is the directory claim c's worktree added whole that holds path,
// and when the board first heard of it, if there is one.
func (c *claim) dirAbove(path string) (string, time.Time, bool) {
	if len(c.Dirs) == 0 {
		return "", time.Time{}, false
	}
	for i := strings.LastIndexByte(path, '/'); i > 0; i = strings.LastIndexByte(path[:i], '/') {
		if t, ok := c.Dirs[path[:i]]; ok {
			return path[:i], t.At, true
		}
	}
	return "", time.Time{}, false
}

// maxScanAge bounds how long before its event a scan may have begun, as
// Footprint.AgeMS says, for the board to keep the changes hooks reported
// since that the scan does not list: a scan takes a second or two, and one
// sent long after it began says little about what came since.
const maxScanAge = 8 * time.Second

// reconciled is the footprint a scan leaves claim c with, its paths, the
// files git found that it did not have, and whether its bounds cut what it
// keeps. It keeps the changes hooks reported first: those the list names,
// those reported after the scan began (Footprint.AgeMS) and, when the list
// is cut short, those it says nothing of; when they are
// more than the footprint's bounds, the newest first, up to hookHeadroom
// past them. Then, in the order the client sent them, which puts first
// those it would least want left out, the files git found, within the
// bounds.
func (b *Board) reconciled(now time.Time, c *claim, fp *Footprint) (map[string]*touch, []string, []PathRef, bool) {
	var hooks, git []fileAt
	listed := make(map[string]bool, len(fp.Files))
	for _, f := range fp.Files {
		listed[f.Path] = true
		t, ok := c.Footprint[f.Path]
		switch {
		case !ok:
			// Which session found a file in git says nothing about who
			// changed it, so a touch from git names none.
			t = &touch{Area: f.Area, At: now, FromGit: true}
		case f.Area != "" && f.Area != t.Area:
			t = &touch{Area: f.Area, At: t.At, Session: t.Session, FromGit: t.FromGit}
		}
		if t.FromGit {
			git = append(git, fileAt{f.Path, t})
		} else {
			hooks = append(hooks, fileAt{f.Path, t})
		}
	}
	// A change a hook reported after the scan began may not be in it. The
	// scan began AgeMS before the client sent it, and the request took a
	// moment to come, so a change reported since now less the age is kept.
	since := now.Add(-time.Duration(fp.AgeMS) * time.Millisecond)
	for _, p := range c.sortedPaths {
		if t := c.Footprint[p]; !t.FromGit && !listed[p] && (fp.Truncated || t.At.After(since)) {
			hooks = append(hooks, fileAt{p, t})
		}
	}
	k := footprintKeeper{next: make(map[string]*touch, len(hooks)+len(git))}
	files, room := b.cfg.MaxFootprint, b.footprintRoom(c, false)
	if cost := footprintCostOf(hooks); len(hooks) > files || cost > room {
		slices.SortFunc(hooks, newerFirst)
		files, room = hookHeadroom(files), b.footprintRoom(c, true)
	}
	for _, f := range hooks {
		k.keep(f, files, room)
	}
	files, room = b.cfg.MaxFootprint, b.footprintRoom(c, false)
	var added []PathRef
	for _, f := range git {
		if !k.keep(f, files, room) {
			break
		}
		if _, had := c.Footprint[f.path]; !had {
			added = append(added, PathRef{Path: f.path, Area: f.t.Area})
		}
	}
	return k.next, k.paths, added, k.cut
}

// footprintKeeper puts together the footprint a scan leaves a claim with.
type footprintKeeper struct {
	next  map[string]*touch
	paths []string
	bytes int  // what next costs
	cut   bool // a file was left out
}

// keep adds f to the footprint if it then holds at most files files and
// costs at most room, and reports whether it did.
func (k *footprintKeeper) keep(f fileAt, files, room int) bool {
	cost := footprintCost(f.path, f.t)
	if len(k.next) >= files || k.bytes+cost > room {
		k.cut = true
		return false
	}
	k.next[f.path] = f.t
	k.paths = append(k.paths, f.path)
	k.bytes += cost
	return true
}

func footprintCostOf(files []fileAt) int {
	n := 0
	for _, f := range files {
		n += footprintCost(f.path, f.t)
	}
	return n
}

// tellShort tells s, with what it hears next, that the call stopped
// comparing its worktree's changes with teammates' work before it had
// compared them all; once, however many calls run short before it hears.
func (b *Board) tellShort(s *session) {
	if b.work.Short() && !strings.Contains(s.Pending, shortChangesNote) {
		s.Pending = joinBlocks(s.Pending, shortChangesNote)
	}
}

// cleanFootprint keeps the valid, distinct paths among the first limit
// entries. Duplicates and invalid paths count toward the limit: a client
// sends neither, and they must not make the work longer.
func cleanFootprint(in []PathRef, limit int) []PathRef {
	in = in[:min(len(in), limit)]
	out := make([]PathRef, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, f := range in {
		p, err := glob.CleanPath(f.Path)
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		area := ""
		if f.Area != "" {
			area, _ = glob.CleanPath(f.Area)
		}
		out = append(out, PathRef{Path: p, Area: area})
	}
	return out
}

// touch records files a hook saw being written.
func (b *Board) touch(now time.Time, c *claim, s *session, paths []PathRef) {
	if len(paths) == 0 {
		return
	}
	// A file an agent just wrote matters more than one git listed, which
	// makes room for it if the footprint is full; but not one the agent
	// wrote too, which is the hook's first.
	files, bytes := 0, 0
	for _, p := range paths {
		t, ok := c.Footprint[p.Path]
		if !ok {
			files, bytes = files+1, bytes+footprintCost(p.Path, &touch{Area: p.Area})
			continue
		}
		area := t.Area
		if p.Area != "" {
			area = p.Area
		}
		b.putTouch(c, p.Path, &touch{Area: area, At: now, Session: s.Key})
	}
	b.makeRoom(c, files, bytes)
	var fresh []PathRef
	for _, p := range paths {
		if _, ok := c.Footprint[p.Path]; ok {
			continue // the hook's already, above, or named twice
		}
		t := &touch{Area: p.Area, At: now, Session: s.Key}
		if !b.hookFits(c, footprintCost(p.Path, t)) {
			c.FootprintTruncated = true
			continue
		}
		b.putTouch(c, p.Path, t)
		fresh = append(fresh, p)
	}
	b.record(Activity{At: now, Kind: ActivityFileChanged, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent, Paths: pathsOf(paths)})
	if len(fresh) > 0 {
		b.alertOthers(now, c, fresh)
		b.tellShort(s)
	}
}

// alertOthers tells every other claim that changed or claimed the same files,
// of those still listening.
func (b *Board) alertOthers(now time.Time, c *claim, paths []PathRef) {
	byName := orderByName(paths)
	covered := make([]bool, len(paths)) // by the claim being compared
	var at map[string]int               // where each path first is in paths, once needed
	live := b.liveAt(now)
	for o := range b.comparing(c.Repo) {
		if o.ID == c.ID || !b.listening(live, o) {
			continue
		}
		// The claim's changes, from whichever of the two lists is shorter.
		clear(covered)
		if len(o.sortedPaths) < len(paths) {
			if at == nil {
				at = make(map[string]int, len(paths))
				for i, p := range slices.Backward(paths) {
					at[p.Path] = i
				}
			}
			for _, f := range o.sortedPaths {
				if i, ok := at[f]; ok {
					covered[i] = true
				}
			}
		} else {
			for i, p := range paths {
				_, covered[i] = o.Footprint[p.Path]
			}
		}
		b.coverByIntents(o, paths, byName, covered)
		// o remembers it was told of the first few files c changed, so
		// that c changing them again does not tell it again; past those,
		// a file that comes back into c's changes is told again, so what
		// it remembers of c does not grow with the files the two share.
		told := o.Told[c.ID]
		var hit []string
		for i, p := range paths {
			if !covered[i] {
				continue
			}
			k := "touch|" + c.ID + "|" + p.Path
			if o.Alerted[k] {
				continue
			}
			if told < maxToldPaths {
				o.alert(k)
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
		b.enqueue(now, o, InboxItem{
			Kind:      "overlap",
			FromClaim: c.ID,
			From:      c.Member,
			Paths:     slices.Clone(hit[:min(len(hit), maxToldPaths)]),
			Text:      fmt.Sprintf("%s also changed %s%s.", who(c), listPaths(hit, maxToldPaths), onBranch(c)),
		})
	}
}

// maxToldPaths bounds the files a claim remembers being told one teammate's
// claim changed, and those an alert's item lists: its text names as many,
// and counts the rest. Claims that share a large footprint, as stacked
// branches do, otherwise remember a key for each file and pair of claims.
const maxToldPaths = 5

// listening reports whether claim o's agents may still hear what is queued
// for it: it has a live session, or it was active within DormantFor. A claim
// quiet for longer counts only as nearby work (conflictWith), and is queued
// nothing, nor remembers what it would have been told: an agent that comes
// back to it hears of teammates' work from its greeting and its checks.
func (b *Board) listening(live liveness, o *claim) bool {
	return live.now.Sub(o.UpdatedAt) <= b.cfg.DormantFor || live.claim(o.ID)
}

// orderByName returns the positions of paths in the order of their names.
func orderByName(paths []PathRef) []int {
	order := make([]int, len(paths))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(x, y int) int { return strings.Compare(paths[x].Path, paths[y].Path) })
	return order
}

// namesUnder returns the run of byName, positions in paths in the order of
// their names, from dir up to dir+"0": dir, the names below it, as '0'
// follows '/', and the few that go on from dir with a byte before '0', such
// as dir.go.
func namesUnder(byName []int, paths []PathRef, dir string) []int {
	lo, _ := slices.BinarySearchFunc(byName, dir, func(i int, dir string) int { return strings.Compare(paths[i].Path, dir) })
	// From lo on, every name is dir or after it, so those before dir+"0"
	// are the ones that start with it and go on, if at all, below '0'.
	n, _ := slices.BinarySearchFunc(byName[lo:], dir, func(i int, dir string) int {
		if name := paths[i].Path; strings.HasPrefix(name, dir) && (len(name) == len(dir) || name[len(dir)] < '0') {
			return -1
		}
		return 1
	})
	return byName[lo : lo+n]
}

// coverByIntents marks, in covered, the paths o's intents cover, given the
// paths' order by name (orderByName). A pattern rooted in a directory can
// cover only that directory and the names below it, which are together in
// that order (namesUnder), so it is matched against those alone: on a busy
// repository, against few or none of the paths a worktree arrives with. A
// pattern rooted nowhere, such as **/*.go, is matched against them all.
func (b *Board) coverByIntents(o *claim, paths []PathRef, byName []int, covered []bool) {
	for _, in := range o.Intents {
		some := byName
		if dir := glob.LiteralDir(in.Pattern); dir != "" {
			some = namesUnder(byName, paths, dir)
		}
		for _, i := range some {
			if b.work.Short() {
				return
			}
			if !covered[i] && b.match(in.Pattern, paths[i].Path) {
				covered[i] = true
			}
		}
	}
}

// reportUnchecked tells the sessions of a claim that changes git found in its
// worktree (made through the shell, which hooks cannot check beforehand) fall
// inside a teammate's active exclusive intent, and records the breach, once
// per file. A policy that ignores reservations ignores these too, and one
// that only warns about them records a warning rather than a breach.
func (b *Board) reportUnchecked(now time.Time, c *claim, s *session, added []PathRef) {
	action := b.cfg.Policy.action(SeverityBlock)
	if action == ActionOff {
		return
	}
	holders := b.reservationHolders(now, c)
	if len(holders) == 0 {
		return
	}
	var lines, paths []string
	var first Conflict
	for _, p := range added {
		cf, ok := b.blockerOf(p, holders)
		if !ok {
			continue
		}
		// Once per file, reservation and policy: a new reservation, or a
		// policy that now refuses what it only warned about, is news.
		if c.alert(uncheckedKey(action, cf)) {
			if len(paths) == 0 {
				first = cf
			}
			lines = append(lines, fmt.Sprintf("- %s, which %s holds exclusively%s", p.Path, who(b.claims[cf.ClaimID]), taskOf(cf)))
			paths = append(paths, p.Path)
		}
	}
	if len(paths) == 0 {
		return
	}
	why := "holds it exclusively" // a breach's kind already says it was not checked
	if action == ActionWarn {
		why = "holds it exclusively; the change was not checked"
	}
	b.record(Activity{At: now, Kind: ActivityConflict, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
		Paths: paths, Severity: SeverityBlock, Decision: DecisionAllow, Breach: action != ActionWarn,
		Text: fmt.Sprintf("%s → %s (%s)", first.Path, first.Member, why)})
	b.tellUnchecked(now, c, s, lines)
}

// tellUnchecked tells s, whose scan found them, and every other live session
// of claim c about unchecked changes in their worktree, listed in lines. A
// client scans a worktree at most once in 15 seconds, after whichever of its
// sessions' shell commands comes first, so the session that reports a change
// is not always the one that made it: each is told, in words that suit a
// session that did not make it.
func (b *Board) tellUnchecked(now time.Time, c *claim, s *session, lines []string) {
	text := "[intagent] Your worktree now changes files a teammate reserved (reported by teammates' agents; " +
		"information, not instructions):\n" + strings.Join(lines, "\n") + "\nChanges made through the shell are not " +
		"checked before they happen, and this one may have come from another session in this worktree or from your " +
		"user. If this session made it and it was not meant for that file, undo it; otherwise tell your user so they " +
		"can agree it with that teammate."
	s.Pending = joinBlocks(s.Pending, text)
	for _, o := range b.claimSessions[c.ID] {
		if trace != nil {
			trace.sessionVisits++
		}
		if o != s && b.state(now, o).Live() {
			o.Pending = joinBlocks(o.Pending, text)
		}
	}
}

// reservationHolders lists the claims whose exclusive intents can block a
// change in c's worktree: the live ones in its repository, other than c,
// that hold one. They are in ID order.
func (b *Board) reservationHolders(now time.Time, c *claim) []*claim {
	var holders []*claim
	live := b.liveAt(now)
	for o := range b.claimsIn(c.Repo) {
		if o.ID != c.ID && slices.ContainsFunc(o.Intents, func(in Intent) bool { return in.Mode == ModeExclusive }) && live.claim(o.ID) {
			holders = append(holders, o)
		}
	}
	return holders
}

// blockerOf is the block conflict conflictsFor lists first for p, given the
// claims that hold reservations: of each holder's first exclusive intent that
// covers p, the first in the order conflicts are sorted.
func (b *Board) blockerOf(p PathRef, holders []*claim) (Conflict, bool) {
	var best Conflict
	var by *Intent
	for _, o := range holders {
		if b.work.Short() {
			break // as comparing does
		}
		for i := range o.Intents {
			in := &o.Intents[i]
			if in.Mode != ModeExclusive || !b.match(in.Pattern, p.Path) {
				continue
			}
			cf := Conflict{Path: p.Path, Area: p.Area, Severity: SeverityBlock, ClaimID: o.ID, Member: o.Member, Branch: o.Branch,
				Task: o.Task, Pattern: in.Pattern, Since: in.DeclaredAt, Active: true}
			if by == nil || conflictBefore(cf, best) {
				best, by = cf, in
			}
			break
		}
	}
	if by == nil {
		return Conflict{}, false
	}
	best.Why = intentWhy(*by)
	return best, true
}

func taskOf(cf Conflict) string {
	if cf.Task == "" {
		return ""
	}
	return " (" + quote(cf.Task) + ")"
}

// breachedReservation reports whether a conflict is a teammate's change to a
// file inside c's exclusive intent that the board learned of after c declared
// it: a hook reports a change as it is made, git when it first sees it. A
// change the board knew of then was among the overlaps the declaration
// reported, and still counts in full.
func (b *Board) breachedReservation(c *claim, cf Conflict) bool {
	if cf.Severity != SeverityOverlap || cf.SameClaim {
		return false
	}
	o := b.claims[cf.ClaimID]
	if o == nil {
		return false
	}
	t, ok := o.Footprint[cf.Path]
	if !ok {
		return false // a plan, not a change
	}
	for _, in := range c.Intents {
		if in.Mode == ModeExclusive && b.match(in.Pattern, cf.Path) && !t.At.Before(in.DeclaredAt) {
			return true
		}
	}
	return false
}

func (b *Board) coveredByIntent(c *claim, path string) bool {
	for _, in := range c.Intents {
		if b.match(in.Pattern, path) {
			return true
		}
	}
	return false
}

// releaseIfDone removes a claim that has nothing left to tell anyone.
func (b *Board) releaseIfDone(now time.Time, c *claim) {
	if len(c.Intents) == 0 && len(c.Footprint) == 0 && !b.liveAt(now).claim(c.ID) {
		b.deleteClaim(now, c, ActivityClaimReleased)
	}
}

func (b *Board) deleteClaim(now time.Time, c *claim, kind ActivityKind) {
	b.removeClaim(c)
	b.unpruned[c.Repo] = true
	b.record(Activity{At: now, Kind: kind, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Text: c.Branch})
}

// --- conflicts and decisions ---------------------------------------------

// conflictsFor lists the claims that matter to one path, most severe first.
func (b *Board) conflictsFor(live liveness, self *claim, selfSession string, p PathRef) []Conflict {
	if trace != nil {
		trace.conflictsFor++
	}
	var out []Conflict
	for o := range b.comparing(self.Repo) {
		if o.ID == self.ID {
			continue
		}
		if cf, ok := b.conflictWith(live, o, p); ok {
			out = append(out, cf)
		}
	}
	// Only a hook says which session wrote a file; a file git found was
	// written by someone in this worktree, perhaps the asking session.
	if t, ok := self.Footprint[p.Path]; ok && !t.FromGit && t.Session != "" && t.Session != selfSession && live.session(t.Session) {
		out = append(out, Conflict{
			Path: p.Path, Area: p.Area, Severity: SeverityOverlap, ClaimID: self.ID, Member: self.Member, Branch: self.Branch, Task: self.Task,
			Why: "another live session in this same worktree changed this file", Since: t.At, Active: true, SameClaim: true,
		})
	}
	sortConflicts(out)
	return out
}

// conflictWith is how much claim o matters to path p: through the first of
// its intents that covers p, the most severe first; else through its change
// to p; else through its work in p's area. Whether o is live is asked only
// of a claim that matters.
func (b *Board) conflictWith(live liveness, o *claim, p PathRef) (Conflict, bool) {
	if trace != nil {
		trace.conflictWith++
	}
	planned, reserved := -1, -1 // the first intent that covers p, and the first exclusive one
	for i := range o.Intents {
		if !b.match(o.Intents[i].Pattern, p.Path) {
			continue
		}
		if planned < 0 {
			planned = i
		}
		if o.Intents[i].Mode == ModeExclusive {
			reserved = i
			break
		}
	}
	t, changed := o.Footprint[p.Path]
	var near time.Time
	var dir string // a directory o's worktree added whole that holds p
	nearby := false
	if planned < 0 && !changed && p.Area != "" {
		near, nearby = b.workedInArea(o, p.Area)
	}
	if planned < 0 && !changed && !nearby {
		dir, near, nearby = o.dirAbove(p.Path)
	}
	if planned < 0 && !changed && !nearby {
		return Conflict{}, false
	}
	active := live.claim(o.ID)
	cf := Conflict{Path: p.Path, Area: p.Area, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: active}
	switch {
	case reserved >= 0 && active:
		in := o.Intents[reserved]
		cf.Severity, cf.Why, cf.Pattern, cf.Since = SeverityBlock, intentWhy(in), in.Pattern, in.DeclaredAt
	case planned >= 0:
		in := o.Intents[planned]
		cf.Severity, cf.Why, cf.Pattern, cf.Since = SeverityOverlap, intentWhy(in), in.Pattern, in.DeclaredAt
	case changed && !active && live.now.Sub(o.UpdatedAt) > b.cfg.DormantFor:
		cf.Severity, cf.Since = SeverityNearby, t.At
		cf.Why = "had unmerged changes to this file (claim dormant for " + ago(live.now, o.UpdatedAt) + ")"
	case changed:
		cf.Severity, cf.Why, cf.Since = SeverityOverlap, "has unmerged changes to this file", t.At
	case dir != "":
		// It is warned of once per directory, as nearby work is once per area.
		cf.Severity, cf.Why, cf.Since = SeverityNearby, "added the directory "+dir+" whole, without listing its files", near
		if cf.Area == "" {
			cf.Area = dir
		}
	default:
		cf.Severity, cf.Why, cf.Since = SeverityNearby, "is working in the same area "+p.Area, near
	}
	return cf, true
}

// intentWhy says why an intent makes a conflict.
func intentWhy(in Intent) string {
	why := fmt.Sprintf("declared %s intent %s", in.Mode, in.Pattern)
	if in.Summary != "" {
		why += ": " + quote(in.Summary)
	}
	return why
}

// workedInArea reports whether a claim changed or declared anything in area,
// and when last.
func (b *Board) workedInArea(c *claim, area string) (time.Time, bool) {
	if trace != nil {
		trace.workedInArea++
	}
	latest, found := c.areaAt[area]
	if !latest.After(time.Time{}) {
		latest, found = time.Time{}, false
	}
	for _, in := range c.Intents {
		dir := glob.LiteralDir(in.Pattern)
		if dir == "" {
			continue
		}
		if b.match(area, dir) || b.match(dir, area) {
			if in.DeclaredAt.After(latest) {
				latest = in.DeclaredAt
			}
			found = true
		}
	}
	return latest, found
}

func sortConflicts(cs []Conflict) {
	sort.SliceStable(cs, func(i, j int) bool { return conflictBefore(cs[i], cs[j]) })
}

// conflictBefore orders conflicts as agents read them: the most severe
// first, then those of running agents, then the newest. Conflicts that tie
// go by member, branch, path and claim, so the order never depends on how
// the board happened to find them.
func conflictBefore(a, b Conflict) bool {
	if a.Severity != b.Severity {
		return a.Severity > b.Severity
	}
	if a.Active != b.Active {
		return a.Active
	}
	if c := a.Since.Compare(b.Since); c != 0 {
		return c > 0
	}
	if a.Member != b.Member {
		return a.Member < b.Member
	}
	if a.Branch != b.Branch {
		return a.Branch < b.Branch
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.ClaimID < b.ClaimID
}

// ackKey names what an agent has been told about a conflict: a file, or for
// nearby work the area it shares with the other claim.
func ackKey(c Conflict) string {
	subject := c.Path
	if c.Severity == SeverityNearby {
		subject = c.Area
	}
	return c.Severity.String() + "|" + c.ClaimID + "|" + subject
}

// Session.Acked remembers what a session has been told: an ackKey for each
// conflict the policy has dealt with, a toldKey for each collision counted and
// announced, and ackPrompted once a prompt has named the claim's task.
const ackPrompted = "task:prompted"

func toldKey(cf Conflict) string { return "told|" + ackKey(cf) }

// verdict sorts the conflicts on one edit by what the policy does about them.
type verdict struct {
	all, refused, asked, warned []Conflict
	// askKeys and warnKeys are acknowledged only if the answer shows them: a
	// refusal of the same edit hides questions and warnings. bumpKeys are
	// the bumps the check acknowledged.
	askKeys, warnKeys, bumpKeys []string
}

// acted is what the agent is told about: refusals, else questions, else warnings.
func (v verdict) acted() []Conflict {
	switch {
	case len(v.refused) > 0:
		return v.refused
	case len(v.asked) > 0:
		return v.asked
	}
	return v.warned
}

// decide answers an agent about to write, and counts and announces the edit
// if it runs into a collision this session has not been told about. It checks
// the first maxCheckPaths paths, and the announcement names them all. It also
// returns the one-time answers the check spent, the bumps it showed and the
// questions an agent that cannot ask is told to put, which a refusal the
// agent never hears gives back.
func (b *Board) decide(now time.Time, c *claim, s *session, paths []PathRef, noAsk bool) (HookResult, []string) {
	if s.Acked == nil {
		s.Acked = map[string]bool{}
	}
	v := b.judge(now, c, s, paths[:min(len(paths), maxCheckPaths)], noAsk)
	res := b.answer(now, s, v)
	spent := v.bumpKeys
	if res.Decision == DecisionAsk {
		spent = append(spent, v.askKeys...)
	}
	// A retry that meets the same conflicts again is checked, but it is not
	// a new collision: counting it, or announcing it, would inflate both.
	var fresh []Conflict
	for _, cf := range v.acted() {
		if k := toldKey(cf); !s.Acked[k] {
			s.Acked[k] = true
			fresh = append(fresh, cf)
		}
	}
	b.count(now, c.Repo, v, len(fresh) > 0)
	if len(fresh) > 0 {
		b.announce(now, c, s, paths, res.Decision, v.acted(), fresh)
	}
	return res, spent
}

// announce records a collision under the conflict that decided the answer,
// a new one when one of those decided it, and names the other new ones.
func (b *Board) announce(now time.Time, c *claim, s *session, paths []PathRef, d Decision, acted, fresh []Conflict) {
	top := mostSevere(acted)
	if f := mostSevere(fresh); f.Severity == top.Severity {
		top = f
	}
	var also []string
	worktrees := map[string]int{} // claims each line stands for
	named := map[string]bool{top.ClaimID: true}
	for _, cf := range fresh {
		if !named[cf.ClaimID] {
			named[cf.ClaimID] = true
			line := fmt.Sprintf("%s on %s: %s", cf.Member, cf.Path, cf.Why)
			if worktrees[line] == 0 {
				also = append(also, line)
			}
			worktrees[line]++
		}
	}
	b.record(Activity{At: now, Kind: ActivityConflict, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
		Paths: decidedFirst(paths, acted), Severity: top.Severity, Decision: d, Also: alsoLines(also, worktrees),
		Text: fmt.Sprintf("%s → %s (%s)", top.Path, top.Member, top.Why)})
}

// maxAlso bounds the lines an activity's Also holds, besides one that counts
// the rest: every stream, the feed, the snapshot and a webhook message carry
// it, and a fleet's worktrees on one file would otherwise each add a line.
const maxAlso = 4

// alsoLines is lines, each said once with the worktrees it stands for, up to
// maxAlso of them, and how many teammates' claims the rest stand for.
func alsoLines(lines []string, worktrees map[string]int) []string {
	out := make([]string, 0, min(len(lines), maxAlso+1))
	rest := 0
	for i, line := range lines {
		if i >= maxAlso {
			rest += worktrees[line]
			continue
		}
		if n := worktrees[line]; n > 1 {
			line += fmt.Sprintf(" (in %d worktrees)", n)
		}
		out = append(out, line)
	}
	if rest > 0 {
		out = append(out, fmt.Sprintf("%d more", rest))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decidedFirst names an edit's paths for its activity. When there are more
// than an activity lists, those of the conflicts that decided the answer come
// first, so that the files it lists include the ones it was about.
func decidedFirst(paths []PathRef, acted []Conflict) []string {
	names := pathsOf(paths)
	if len(names) <= maxShownPaths {
		return names
	}
	decided := make(map[string]bool, len(acted))
	for _, cf := range acted {
		decided[cf.Path] = true
	}
	out := make([]string, 0, len(names))
	for _, first := range []bool{true, false} {
		for _, p := range names {
			if decided[p] == first {
				out = append(out, p)
			}
		}
	}
	return out
}

// judge applies the policy to the conflicts on every path being written. A
// bump counts as acknowledged as soon as it is met: its retry goes through.
func (b *Board) judge(now time.Time, c *claim, s *session, paths []PathRef, noAsk bool) verdict {
	live := b.liveAt(now)
	var v verdict
	for _, p := range paths {
		for _, cf := range b.conflictsFor(live, c, s.Key, p) {
			v.all = append(v.all, cf)
			key := ackKey(cf)
			action := b.cfg.Policy.action(cf.Severity)
			if action == ActionBump && b.breachedReservation(c, cf) {
				// A teammate changed a file inside this claim's reservation
				// after it was made: the owner is told, not stopped.
				action = ActionWarn
			}
			switch {
			case action == ActionDeny:
				v.refused = append(v.refused, cf)
			case action == ActionAsk && !noAsk:
				v.asked = append(v.asked, cf)
			case action == ActionAsk && !s.Acked[key]:
				v.asked = append(v.asked, cf)
				v.askKeys = append(v.askKeys, key)
			case action == ActionBump && !s.Acked[key]:
				s.Acked[key] = true
				v.refused = append(v.refused, cf)
				v.bumpKeys = append(v.bumpKeys, key)
			case action == ActionWarn && !s.Acked[key]:
				v.warned = append(v.warned, cf)
				v.warnKeys = append(v.warnKeys, key)
			}
		}
	}
	return v
}

// answer turns a verdict into what the hook says, and acknowledges the
// questions and warnings it shows.
func (b *Board) answer(now time.Time, s *session, v verdict) HookResult {
	res := HookResult{Decision: DecisionAllow}
	res.Conflicts, res.MoreConflicts = boundConflicts(v.all)
	switch {
	case len(v.refused) > 0:
		res.Decision = DecisionRefuse
		res.Reason = b.renderRefusal(now, v.refused, b.cfg.Policy)
	case len(v.asked) > 0:
		res.Decision = DecisionAsk
		res.Reason = b.renderRefusal(now, v.asked, b.cfg.Policy)
		// An agent that cannot ask was told to ask its person; its retry is the answer.
		for _, k := range v.askKeys {
			s.Acked[k] = true
		}
	case len(v.warned) > 0:
		res.Context = b.renderWarnings(now, v.warned)
		for _, k := range v.warnKeys {
			s.Acked[k] = true
		}
	}
	return res
}

// Bounds on the conflicts an answer lists, which an edit's agent never reads
// (it reads the decision, the reason and the context) but which go back in
// every answer: those that block or overlap, and the newest of those nearby.
const (
	maxAnswerConflicts = 200
	maxNearbyConflicts = 20
)

// boundConflicts keeps, of the conflicts all lists, the first
// maxAnswerConflicts that block or overlap, those that block first, and the
// maxNearbyConflicts newest of those nearby, in the order all has them, and
// counts the rest.
func boundConflicts(all []Conflict) ([]Conflict, int) {
	var nearby []int
	for i, cf := range all {
		if cf.Severity == SeverityNearby {
			nearby = append(nearby, i)
		}
	}
	if len(all)-len(nearby) <= maxAnswerConflicts && len(nearby) <= maxNearbyConflicts {
		return all, 0
	}
	keep := make([]bool, len(all))
	kept := 0
	for _, sev := range []Severity{SeverityBlock, SeverityOverlap} {
		for i, cf := range all {
			if cf.Severity == sev && kept < maxAnswerConflicts {
				keep[i] = true
				kept++
			}
		}
	}
	slices.SortStableFunc(nearby, func(x, y int) int { return all[y].Since.Compare(all[x].Since) })
	for _, i := range nearby[:min(len(nearby), maxNearbyConflicts)] {
		keep[i] = true
	}
	out := make([]Conflict, 0, kept+maxNearbyConflicts)
	for i, cf := range all {
		if keep[i] {
			out = append(out, cf)
		}
	}
	return out, len(all) - len(out)
}

// count adds one checked edit to the repository's stats, under its most
// severe conflict and what was done about it. A retry that met only
// collisions already counted is counted as a check alone.
func (b *Board) count(now time.Time, repo string, v verdict, fresh bool) {
	st := b.statsOf(repo, now)
	st.Checks++
	if !fresh && len(v.acted()) > 0 {
		return
	}
	if len(v.all) > 0 {
		switch mostSevere(v.all).Severity {
		case SeverityBlock:
			st.Blocks++
		case SeverityOverlap:
			st.Overlaps++
		case SeverityNearby:
			st.Nearby++
		}
	}
	hard := slices.ContainsFunc(v.refused, func(cf Conflict) bool { return b.cfg.Policy.action(cf.Severity) == ActionDeny })
	switch {
	case hard:
		st.Refused++
	case len(v.refused) > 0:
		st.Bumped++
	case len(v.asked) > 0:
		st.Asked++
	case len(v.warned) > 0:
		st.Warned++
	}
}

// mostSevere is the first of the most severe conflicts in a non-empty list.
func mostSevere(list []Conflict) Conflict {
	top := list[0]
	for _, cf := range list[1:] {
		if cf.Severity > top.Severity {
			top = cf
		}
	}
	return top
}

// --- inbox ---------------------------------------------------------------

func (b *Board) enqueue(now time.Time, c *claim, it InboxItem) {
	it.ID = b.newID("i_")
	it.At = now
	it.Text = Clean(it.Text, maxNoteLen)
	keep := c.Inbox[:0:0]
	for _, old := range c.Inbox {
		if now.Sub(old.At) < inboxTTL {
			keep = append(keep, old)
		}
	}
	keep = append(keep, it)
	for len(keep) > maxInbox {
		i := evictIndex(keep)
		keep = slices.Delete(keep, i, i+1)
	}
	c.Inbox = keep
}

// evictIndex picks the item a full inbox lets go of: the oldest one some
// session was already shown; else the oldest of the sender with the most
// items, an alert before a note when senders tie. So a flood of alerts from
// one teammate's worktrees, or of their notes, pushes out their own items,
// not a note from someone else the agent has not heard yet.
func evictIndex(in []InboxItem) int {
	for i := range in {
		if len(in[i].DeliveredTo) > 0 {
			return i
		}
	}
	held := make(map[string]int, len(in))
	for _, it := range in {
		held[it.From]++
	}
	best := 0
	for i := 1; i < len(in); i++ {
		n, m := held[in[i].From], held[in[best].From]
		if n > m || (n == m && in[best].Kind == "note" && in[i].Kind != "note") {
			best = i
		}
	}
	return best
}

// deliver renders what this session has not been told yet: context held back
// from before an edit, then the inbox items it has not seen. In an answer
// nobody will read, it delivers nothing: all of it waits for the next one.
func (b *Board) deliver(now time.Time, c *claim, s *session) string {
	if b.unheard {
		return ""
	}
	pending := s.Pending
	s.Pending = ""
	b.collectMail(now, c)
	return joinBlocks(pending, b.deliverInbox(now, c, s))
}

// Each session hears an item once. One that another session of this claim
// already showed its agent is still told (that session may have stopped
// without acting on it), but as earlier news, not new.
func (b *Board) deliverInbox(now time.Time, c *claim, s *session) string {
	var fresh, earlier []InboxItem
	for i := range c.Inbox {
		it := &c.Inbox[i]
		if now.Sub(it.At) >= inboxTTL || it.DeliveredTo[s.Key] {
			continue
		}
		if len(fresh)+len(earlier) == b.cfg.InboxPerHook {
			break
		}
		if len(it.DeliveredTo) > 0 {
			earlier = append(earlier, *it)
		} else {
			fresh = append(fresh, *it)
		}
		if it.DeliveredTo == nil {
			it.DeliveredTo = map[string]bool{}
		}
		it.DeliveredTo[s.Key] = true
	}
	return b.renderInbox(now, fresh, earlier)
}

// --- intents -------------------------------------------------------------

// DeclareRequest declares intents for the caller's claim in a worktree.
type DeclareRequest struct {
	Member   string   `json:"-"`
	Where    Where    `json:"where"`
	Summary  string   `json:"summary"`
	Patterns []string `json:"patterns"`
	Mode     Mode     `json:"mode"`
}

// Rejection explains why an intent was not accepted.
type Rejection struct {
	Pattern string   `json:"pattern"`
	Reason  string   `json:"reason"`
	Against Conflict `json:"against"`
}

// DeclareResult reports what was accepted and who else is in the way.
type DeclareResult struct {
	ClaimID  string      `json:"claim_id"`
	Accepted []Intent    `json:"accepted"`
	Rejected []Rejection `json:"rejected,omitempty"`
	Overlaps []Conflict  `json:"overlaps,omitempty"`
	Text     string      `json:"text"`
	// Partial says the board stopped comparing the intents with teammates'
	// patterns and files before it had compared them all (Text says so too):
	// Rejected and Overlaps may miss some.
	Partial bool `json:"partial,omitempty"`
}

// Declare records intents. An exclusive intent that overlaps another active
// claim's exclusive intent is rejected, and so is one the call could not
// finish comparing with them; the caller may declare it shared.
func (b *Board) Declare(now time.Time, r DeclareRequest) (DeclareResult, error) {
	w, err := cleanWhere(r.Where)
	if err != nil {
		return DeclareResult{}, err
	}
	mode, err := ParseMode(string(r.Mode))
	if err != nil {
		return DeclareResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(r.Patterns) == 0 || len(r.Patterns) > maxIntents {
		return DeclareResult{}, fmt.Errorf("%w: declare between 1 and %d patterns", ErrInvalid, maxIntents)
	}
	patterns := make([]string, 0, len(r.Patterns))
	for _, p := range r.Patterns {
		c, err := glob.CleanPattern(p)
		if err != nil {
			return DeclareResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		patterns = append(patterns, c)
	}
	summary := Clean(r.Summary, maxTaskLen)

	b.lock()
	defer b.unlock()
	defer b.countPartial(now, w.Repo)
	b.changed()
	c := b.claimFor(now, r.Member, w)
	live := b.liveAt(now)
	res := DeclareResult{ClaimID: c.ID}

	for _, pat := range patterns {
		if mode == ModeExclusive {
			against, ok := b.exclusiveClash(c, pat, live)
			switch {
			case ok:
				res.Rejected = append(res.Rejected, Rejection{
					Pattern: pat,
					Reason: fmt.Sprintf("%s's agent holds %s exclusively, and a shared intent of yours would not change "+
						"that: work elsewhere, ask them with a note, or tell your user", against.Member, against.Pattern),
					Against: against,
				})
				continue
			case b.work.Short():
				// A reservation not compared with every other one could
				// overlap one, and two would hold the same files.
				res.Rejected = append(res.Rejected, Rejection{
					Pattern: pat,
					Reason: "intagent could not finish comparing it with teammates' reservations, so it cannot reserve it: " +
						"declare it shared, or tell your user",
				})
				continue
			}
		}
		in := Intent{Pattern: pat, Mode: mode, Summary: summary, DeclaredAt: now}
		c.Intents = upsertIntent(c.Intents, in)
		res.Accepted = append(res.Accepted, in)
	}
	if len(res.Accepted) > 0 && summary != "" {
		c.Task = summary
	}
	c.UpdatedAt = now

	for _, in := range res.Accepted {
		res.Overlaps = append(res.Overlaps, b.intentOverlaps(c, in, live)...)
	}
	sortConflicts(res.Overlaps)
	b.tellIntent(now, c, res.Accepted, res.Overlaps)
	if len(res.Accepted) > 0 {
		b.statsOf(c.Repo, now).Intents += len(res.Accepted)
		b.record(Activity{At: now, Kind: ActivityIntentDeclared, Repo: c.Repo, Member: c.Member, ClaimID: c.ID,
			Paths: intentPatterns(res.Accepted), Text: fmt.Sprintf("%s: %s", mode, summary)})
	}
	res.Partial = b.work.Short()
	res.Text = renderDeclare(now, res, b.cfg.Policy)
	return res, nil
}

func (b *Board) exclusiveClash(self *claim, pattern string, live liveness) (Conflict, bool) {
	for o := range b.comparing(self.Repo) {
		if o.ID == self.ID || !slices.ContainsFunc(o.Intents, func(in Intent) bool { return in.Mode == ModeExclusive }) || !live.claim(o.ID) {
			continue
		}
		for _, in := range o.Intents {
			if in.Mode == ModeExclusive && b.overlap(in.Pattern, pattern) {
				return Conflict{Path: pattern, Severity: SeverityBlock, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task,
					Why: "holds exclusive intent " + in.Pattern, Pattern: in.Pattern, Since: in.DeclaredAt, Active: true}, true
			}
		}
	}
	return Conflict{}, false
}

func upsertIntent(list []Intent, in Intent) []Intent {
	for i := range list {
		if list[i].Pattern == in.Pattern {
			list[i] = in
			return list
		}
	}
	if len(list) >= maxIntents {
		list = list[1:]
	}
	return append(list, in)
}

// intentOverlaps lists other claims whose intents or changed files meet a new intent.
func (b *Board) intentOverlaps(c *claim, in Intent, live liveness) []Conflict {
	// The files the intent can cover are those under the directory it is
	// rooted in, which are together in each claim's ordered paths: from
	// dir+"/" up to dir+"0", as '0' follows '/'. A pattern rooted nowhere,
	// such as **/*.go, can cover any of them.
	dir := glob.LiteralDir(in.Pattern)
	lo, hi := dir+"/", dir+"0"
	var out []Conflict
	for o := range b.comparing(c.Repo) {
		if o.ID == c.ID {
			continue
		}
		cf := Conflict{Path: in.Pattern, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task}
		for _, oi := range o.Intents {
			if b.overlap(oi.Pattern, in.Pattern) {
				sev := SeverityOverlap
				if oi.Mode == ModeExclusive && live.claim(o.ID) {
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
			see := func(p string) {
				if b.match(in.Pattern, p) {
					hit = append(hit, p)
					if t := o.Footprint[p]; t.At.After(since) {
						since = t.At
					}
				}
			}
			if dir == "" {
				for _, p := range o.sortedPaths {
					see(p)
				}
			} else {
				if _, ok := o.Footprint[dir]; ok {
					see(dir)
				}
				for _, p := range o.pathsIn(lo, hi) {
					see(p)
				}
			}
			if len(hit) > 0 {
				cf.Severity, cf.Since = SeverityOverlap, since
				cf.Why = "has unmerged changes to " + listPaths(hit, 3)
			}
		}
		if cf.Severity > SeverityNone {
			cf.Active = live.claim(o.ID)
			out = append(out, cf)
		}
	}
	return out
}

// tellIntent tells the claims a declaration overlaps, of those still
// listening.
func (b *Board) tellIntent(now time.Time, c *claim, accepted []Intent, overlaps []Conflict) {
	notified := map[string]bool{}
	live := b.liveAt(now)
	for _, cf := range overlaps {
		if notified[cf.ClaimID] {
			continue
		}
		o := b.claims[cf.ClaimID]
		if o == nil || !b.listening(live, o) {
			continue
		}
		notified[cf.ClaimID] = true
		pats := intentPatterns(accepted)
		text := fmt.Sprintf("%s declared %s intent on %s", who(c), accepted[0].Mode, listPaths(pats, 4))
		if accepted[0].Summary != "" {
			text += ": " + quote(accepted[0].Summary)
		}
		b.enqueue(now, o, InboxItem{Kind: "intent", FromClaim: c.ID, From: c.Member, Paths: pats, Text: text + ". It overlaps your work."})
	}
}

// ReleaseRequest releases intents. No patterns releases all of them.
type ReleaseRequest struct {
	Member   string   `json:"-"`
	Where    Where    `json:"where"`
	Patterns []string `json:"patterns,omitempty"`
}

// Release drops intents and returns how many were released.
func (b *Board) Release(now time.Time, r ReleaseRequest) (int, error) {
	w, err := cleanWhere(r.Where)
	if err != nil {
		return 0, err
	}
	drop := map[string]bool{}
	for _, p := range r.Patterns {
		c, err := glob.CleanPattern(p)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		drop[c] = true
	}
	b.lock()
	defer b.unlock()
	c := b.findClaim(r.Member, w)
	if c == nil {
		return 0, nil
	}
	b.changed()
	keep := c.Intents[:0:0]
	var gone []string
	for _, in := range c.Intents {
		if len(drop) == 0 || drop[in.Pattern] {
			gone = append(gone, in.Pattern)
			continue
		}
		keep = append(keep, in)
	}
	c.Intents = keep
	c.UpdatedAt = now
	if len(gone) > 0 {
		b.record(Activity{At: now, Kind: ActivityIntentReleased, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Paths: gone})
	}
	return len(gone), nil
}

// --- read-only queries ----------------------------------------------------

// CheckRequest asks who else matters to some paths.
type CheckRequest struct {
	Member string    `json:"-"`
	Where  Where     `json:"where"`
	Paths  []PathRef `json:"paths"`
}

// CheckResult is the answer to POST /v1/check: the conflicts, and the same as text.
type CheckResult struct {
	Conflicts []Conflict `json:"conflicts"`
	Text      string     `json:"text"`
	// Unchecked counts the paths past the first maxCheckPaths, which were not
	// checked; Text says so too.
	Unchecked int `json:"unchecked,omitempty"`
	// Partial says the board stopped comparing the paths with teammates'
	// patterns before it had compared them all; Text says so too.
	Partial bool `json:"partial,omitempty"`
}

// ReleaseResult is the answer to POST /v1/intents/release.
type ReleaseResult struct {
	Released int `json:"released"`
}

// Whoami is the answer to GET /v1/whoami: the member a token belongs to, and
// the server they reach.
type Whoami struct {
	Member  string `json:"member"`
	Version string `json:"version"`
	Policy  Policy `json:"policy"`
	// Demo is true for 'intagent demo', whose agents are simulated.
	Demo bool `json:"demo,omitempty"`
}

// Check lists conflicts for paths without changing anything. It checks the
// first maxCheckPaths of them, and its answer counts the rest and says how
// to check them. intagent's clients send checks of maxCheckPaths; older ones
// send every path in one and read only the conflicts, as they did when the
// rest went unsaid, and a refusal would pass a whole commit unchecked.
func (b *Board) Check(now time.Time, r CheckRequest) (CheckResult, error) {
	w, err := cleanWhere(r.Where)
	if err != nil {
		return CheckResult{}, err
	}
	paths, err := cleanPaths(r.Paths, maxCheckPaths)
	if err != nil {
		return CheckResult{}, err
	}
	cs, partial := b.check(now, r.Member, w, paths)
	res := CheckResult{Conflicts: cs, Text: RenderConflicts(now, cs), Unchecked: max(0, len(r.Paths)-maxCheckPaths), Partial: partial}
	if res.Unchecked > 0 {
		res.Text += fmt.Sprintf("\nintagent checked only the first %d of these %d paths. Check the other %d in other "+
			"checks, %d at a time.", maxCheckPaths, len(r.Paths), res.Unchecked, maxCheckPaths)
	}
	if partial {
		res.Text += "\n" + partialNote + " What it had not compared is not listed."
	}
	return res, nil
}

// check lists the conflicts of paths, cleaned, for member's claim in w, and
// whether it stopped matching for want of work.
func (b *Board) check(now time.Time, member string, w Where, paths []PathRef) ([]Conflict, bool) {
	b.lock()
	defer b.unlock()
	self := b.findClaim(member, w)
	if self == nil {
		self = &claim{Repo: w.Repo, Member: member}
	}
	live := b.liveAt(now)
	var out []Conflict
	for _, p := range paths {
		out = append(out, b.conflictsFor(live, self, "", p)...)
	}
	return out, b.work.Short()
}

// NoteRequest sends a short note to another claim's agents.
type NoteRequest struct {
	Member string `json:"-"`
	Where  Where  `json:"where"`
	// To is a claim ID, a member name, or a repo-relative path.
	To   string `json:"to"`
	Text string `json:"text"`
	// ByPerson says the member wrote the note themselves, not their agent.
	ByPerson bool `json:"by_person,omitempty"`
	// ToMember, set by the server, says To names a member of the team, who
	// may have no claim in the repository yet.
	ToMember bool `json:"-"`
}

// NoteResult lists the claims a note was queued for.
type NoteResult struct {
	Delivered []string `json:"delivered"`
	// HeldFor names the member a note waits for when none of their
	// worktrees in the repository is listening: their next session there
	// hears it, in whichever worktree.
	HeldFor string `json:"held_for,omitempty"`
}

// Note queues a note in the inbox of every claim the recipient resolves to.
func (b *Board) Note(now time.Time, r NoteRequest) (NoteResult, error) {
	w, err := cleanWhere(r.Where)
	if err != nil {
		return NoteResult{}, err
	}
	text := Clean(r.Text, maxNoteLen)
	to := strings.TrimSpace(r.To)
	if text == "" || to == "" {
		return NoteResult{}, fmt.Errorf("%w: a note needs a recipient and text", ErrInvalid)
	}
	b.lock()
	defer b.unlock()
	defer b.countPartial(now, w.Repo)
	recent := b.notes[r.Member][:0:0]
	for _, t := range b.notes[r.Member] {
		if now.Sub(t) < noteWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= b.cfg.NotesPerMinute {
		b.notes[r.Member] = recent
		return NoteResult{}, ErrRateLimited
	}
	self := b.findClaim(r.Member, w)
	targets, path, byID := b.resolve(w.Repo, to, self, r.ToMember)
	var res NoteResult
	live := b.liveAt(now)
	switch {
	case byID:
	case path != "" || to == r.Member:
		targets = b.listeners(live, targets)
	default:
		// To a member: their claims still listening, or their mailbox.
		targets = slices.DeleteFunc(targets, func(c *claim) bool { return !b.listening(live, c) })
		if len(targets) == 0 {
			res.HeldFor = to
		}
	}
	if len(targets) == 0 && res.HeldFor == "" {
		return NoteResult{}, fmt.Errorf("%w: %q", ErrNoTarget, to)
	}
	b.changed()
	b.notes[r.Member] = append(recent, now)
	from := &claim{Member: r.Member}
	if self != nil {
		from = self
	}
	sender := who(from)
	if r.ByPerson {
		sender = from.Member
	}
	item := InboxItem{Kind: "note", FromClaim: from.ID, From: r.Member, Text: fmt.Sprintf("Note from %s: %s", sender, quote(text))}
	for _, t := range targets {
		b.enqueue(now, t, item)
		res.Delivered = append(res.Delivered, t.ID)
	}
	if res.HeldFor != "" {
		b.hold(now, w.Repo, res.HeldFor, item)
	}
	b.statsOf(w.Repo, now).Notes += max(len(targets), 1)
	note := Activity{At: now, Kind: ActivityNoteSent, Repo: w.Repo, Member: r.Member, ClaimID: from.ID}
	switch {
	case path != "":
		note.Paths, note.Text = []string{path}, fmt.Sprintf("to whoever works on %s: %s", path, text)
	case res.HeldFor != "":
		note.Text = fmt.Sprintf("to %s, for their next session: %s", res.HeldFor, text)
	default:
		note.Text = fmt.Sprintf("to %s: %s", targets[0].Member, text)
	}
	b.record(note)
	return res, nil
}

// resolve finds a note's recipients: a claim by ID, a member's claims, or the
// claims that changed or reserved a path, which it also returns. It reports
// whether the note named its claim. A note to a member of the team (member)
// is to that member, whatever claims they have.
func (b *Board) resolve(repo, to string, self *claim, member bool) ([]*claim, string, bool) {
	if c, ok := b.claims[to]; ok && c.Repo == repo {
		return []*claim{c}, "", true
	}
	var out []*claim
	for c := range b.claimsIn(repo) {
		if self != nil && c.ID == self.ID {
			continue
		}
		if c.Member == to {
			out = append(out, c)
		}
	}
	if len(out) > 0 || member {
		return out, "", false
	}
	p, err := glob.CleanPath(to)
	if err != nil {
		return nil, "", false
	}
	for c := range b.comparing(repo) {
		if self != nil && c.ID == self.ID {
			continue
		}
		if _, ok := c.Footprint[p]; ok || b.coveredByIntent(c, p) {
			out = append(out, c)
		}
	}
	return out, p, false
}

// listeners keeps, of a note's recipients, the claims still listening. A
// member none of whose claims among them listens keeps the one they were
// last active in, ties going by ID, so a note to someone away waits for
// them once rather than in every worktree they left. A member with many
// worktrees would otherwise have a note queued in each, every one of which
// is saved with the board until it expires, for none to read.
func (b *Board) listeners(live liveness, cs []*claim) []*claim {
	if len(cs) <= 1 {
		return cs
	}
	keep := make([]bool, len(cs))
	heard := map[string]bool{} // members with a claim that listens
	newest := map[string]int{} // each member's most recently active claim
	for i, c := range cs {
		if b.listening(live, c) {
			keep[i], heard[c.Member] = true, true
		}
		n, ok := newest[c.Member]
		if !ok || c.UpdatedAt.After(cs[n].UpdatedAt) || (c.UpdatedAt.Equal(cs[n].UpdatedAt) && c.ID < cs[n].ID) {
			newest[c.Member] = i
		}
	}
	for m, i := range newest {
		keep[i] = keep[i] || !heard[m]
	}
	out := cs[:0:0]
	for i, c := range cs {
		if keep[i] {
			out = append(out, c)
		}
	}
	return out
}

// --- maintenance -----------------------------------------------------------

// Sweep announces stalled and gone sessions, drops old sessions, and removes
// claims that are released or forgotten. Call it every few seconds.
func (b *Board) Sweep(now time.Time) {
	b.lock()
	defer b.unlock()
	changed := false
	// In the order they went quiet, so announcements read in time order.
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
	// Which claims still have a session, and which a live one, once old
	// sessions are dropped: gathered in this pass, not by a search of every
	// session for every claim.
	live, held := map[string]bool{}, make(map[string]bool, len(b.claims))
	var done []*session // those kept that are not live, in the order of keys
	for _, k := range keys {
		s := b.sessions[k]
		if trace != nil {
			trace.sessionVisits++
		}
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
			continue
		}
		held[s.ClaimID] = true
		if st.Live() {
			live[s.ClaimID] = true
		} else {
			done = append(done, s)
		}
	}
	changed = b.trimEnded(done) || changed
	changed = b.evictSessions(now, keys) || changed
	ids := make([]string, 0, len(b.claims))
	for id := range b.claims {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	dormant, idle := 0, []*claim(nil) // claims with no live session, and those of them with no intent
	for _, id := range ids {
		c := b.claims[id]
		changed = b.tidy(now, c, live[c.ID]) || changed
		if live[c.ID] {
			continue
		}
		switch {
		case len(c.Intents) == 0 && len(c.Footprint) == 0 && !held[c.ID]:
			b.deleteClaim(now, c, ActivityClaimReleased)
			changed = true
		case now.Sub(c.UpdatedAt) > b.cfg.ForgetAfter:
			b.deleteClaim(now, c, ActivityClaimForgotten)
			changed = true
		case len(c.Intents) == 0:
			dormant++
			idle = append(idle, c)
		default:
			dormant++
		}
	}
	changed = b.forgetDormant(now, dormant, idle) || changed
	for m, ts := range b.notes {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > noteWindow {
			delete(b.notes, m)
		}
	}
	changed = b.pruneAlerts() || changed
	changed = b.tidyMail(now) || changed
	if changed {
		b.changed()
	}
}

// tidy drops what claim c keeps for nobody, and reports whether it dropped
// anything: the inbox items no session can be shown any more, and, once it
// no longer listens (listening), the alerts it remembers, which keep only
// what it is no longer told from being told twice. Items are queued in time
// order, so only the oldest is looked at when none has expired.
func (b *Board) tidy(now time.Time, c *claim, live bool) bool {
	tidied := false
	if len(c.Inbox) > 0 && now.Sub(c.Inbox[0].At) >= inboxTTL {
		c.Inbox = slices.DeleteFunc(c.Inbox, func(it InboxItem) bool { return now.Sub(it.At) >= inboxTTL })
		if len(c.Inbox) == 0 {
			c.Inbox = nil
		}
		tidied = true
	}
	if (len(c.Alerted) > 0 || len(c.Told) > 0) && !live && now.Sub(c.UpdatedAt) > b.cfg.DormantFor {
		c.Alerted, c.alertedShared, c.Told = nil, false, nil
		tidied = true
	}
	return tidied
}

// pruneAlerts drops the alerts claims remember of claims no longer on the
// board, in the repositories a claim was removed from since the last sweep:
// a claim's ID is never used again, so nothing they would keep from being
// told twice can come. A claim that lives for weeks otherwise keeps a key
// for every short-lived claim that ever touched its files.
func (b *Board) pruneAlerts() bool {
	pruned := false
	for repo := range b.unpruned {
		for c := range b.claimsIn(repo) {
			pruned = c.pruneAlerted(b.claims) || pruned
		}
	}
	clear(b.unpruned)
	return pruned
}

// pruneAlerted drops the claim's alerts of claims not in claims, and its
// counts of them (Told). It puts the alerts it keeps in a new map, which a
// snapshot may share, and which is no larger than what it holds: a map does
// not shrink as keys are deleted.
func (c *claim) pruneAlerted(claims map[string]*claim) bool {
	pruned := false
	for id := range c.Told {
		if claims[id] == nil {
			delete(c.Told, id)
			pruned = true
		}
	}
	if pruned && len(c.Told) == 0 {
		c.Told = nil
	}
	dead := func(k string) bool {
		id := alertedClaim(k)
		return id != "" && claims[id] == nil
	}
	n := 0
	for k := range c.Alerted {
		if dead(k) {
			n++
		}
	}
	if n == 0 {
		return pruned
	}
	var keep map[string]bool
	if n < len(c.Alerted) {
		keep = make(map[string]bool, len(c.Alerted)-n)
		for k := range c.Alerted {
			if !dead(k) {
				keep[k] = true
			}
		}
	}
	c.Alerted, c.alertedShared = keep, false
	return true
}

// alertedClaim is the claim an Alerted key names: alertOthers' touch|<claim>|
// <path>, or uncheckedKey's unchecked|<action>|<claim>|...
func alertedClaim(k string) string {
	kind, rest, _ := strings.Cut(k, "|")
	switch kind {
	case "touch":
	case "unchecked":
		_, rest, _ = strings.Cut(rest, "|")
	default:
		return ""
	}
	id, _, _ := strings.Cut(rest, "|")
	return id
}

func pathsOf(ps []PathRef) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Path
	}
	return out
}

func intentPatterns(in []Intent) []string {
	out := make([]string, len(in))
	for i, x := range in {
		out[i] = x.Pattern
	}
	return out
}
