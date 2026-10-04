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
	return c
}

const (
	maxTaskLen   = 200
	maxNoteLen   = 600
	maxBranchLen = 120
	maxPaths     = 200
	maxIntents   = 50
	maxInbox     = 50
	inboxTTL     = 24 * time.Hour
	keepEndedFor = time.Hour
	noteWindow   = time.Minute
	idLen        = 8 // characters of an ID after its prefix
)

// Board holds every claim and session the server knows about.
type Board struct {
	mu       sync.Mutex
	notifyMu sync.Mutex // orders notifications; taken before mu is released
	cfg      Config
	claims   map[string]*claim
	byKey    map[string]string
	sessions map[string]*session
	recent   []Activity
	seq      uint64
	version  uint64
	notes    map[string][]time.Time
	stats    map[string]*Stats
	pending  []Activity
	notify   func([]Activity)
	newID    func(prefix string) string
	// unheard is set while Hook records an event whose answer nobody will
	// read: nothing is delivered in it.
	unheard bool

	// dropped is, by repository, the seq of the newest activity the feed has
	// let go of, and droppedAll the newest of all; droppedFloor covers what a
	// restart let go of, whose repositories are not known. A replay from an
	// older seq misses something.
	dropped      map[string]uint64
	droppedAll   uint64
	droppedFloor uint64
}

// Option configures a Board.
type Option func(*Board)

// WithNotify registers fn to receive new activities. It is called without the
// board's lock held, one call at a time, in the order activities happened; it
// must not block or call back into the board.
func WithNotify(fn func([]Activity)) Option { return func(b *Board) { b.notify = fn } }

// withIDs replaces the random ID generator, for tests.
func withIDs(fn func(prefix string) string) Option { return func(b *Board) { b.newID = fn } }

// New returns an empty board. Fields of cfg left unset take their defaults.
func New(cfg Config, opts ...Option) *Board {
	b := &Board{
		cfg:      cfg.WithDefaults(),
		claims:   map[string]*claim{},
		byKey:    map[string]string{},
		sessions: map[string]*session{},
		notes:    map[string][]time.Time{},
		stats:    map[string]*Stats{},
		dropped:  map[string]uint64{},
		newID:    randomID,
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

func (b *Board) lock() { b.mu.Lock() }

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

func cleanPaths(in []PathRef) ([]PathRef, error) {
	if len(in) > maxPaths {
		in = in[:maxPaths]
	}
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
	b.claims[c.ID] = c
	b.byKey[c.key()] = c.ID
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
			ClaimID:   c.ID,
			StartedAt: now,
			LastSeen:  now,
			Phase:     phaseWaiting,
		}
		b.sessions[key] = s
		b.record(Activity{At: now, Kind: ActivitySessionStarted, Repo: c.Repo, Member: ev.Member, ClaimID: c.ID, Session: ev.SessionID, Agent: ev.Agent})
	}
	s.ClaimID = c.ID
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

// liveClaims returns the IDs of claims with at least one live session.
func (b *Board) liveClaims(now time.Time) map[string]bool {
	live := map[string]bool{}
	for _, s := range b.sessions {
		if trace != nil {
			trace.sessionVisits++
		}
		if b.state(now, s).Live() {
			live[s.ClaimID] = true
		}
	}
	return live
}

func (b *Board) liveSessions(now time.Time) map[string]bool {
	live := map[string]bool{}
	for k, s := range b.sessions {
		if trace != nil {
			trace.sessionVisits++
		}
		if b.state(now, s).Live() {
			live[k] = true
		}
	}
	return live
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
	ev, err := ev.clean()
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
	b.changed()
	b.unheard = late
	defer func() { b.unheard = false }()

	c := b.claimFor(now, ev.Member, w)
	s := b.sessionFor(now, ev, c)
	checkedAfter := ev.Kind == KindPostEdit && ev.ToolUseID != "" && !s.Calls[ev.ToolUseID]
	if checkedAfter {
		b.unansweredEdit(now, c, s, ev)
	}
	was := b.state(now, s)
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
// its fields.
func (ev HookEvent) clean() (HookEvent, error) {
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
	ev.Paths, err = cleanPaths(ev.Paths)
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

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// reconcile replaces a claim's footprint with what git reports.
func (b *Board) reconcile(now time.Time, c *claim, s *session, fp *Footprint) {
	if fp == nil {
		return
	}
	files := cleanFootprint(fp.Files, b.cfg.MaxFootprint)
	next := make(map[string]*touch, len(files))
	var added []PathRef
	for _, f := range files {
		if t, ok := c.Footprint[f.Path]; ok {
			if f.Area != "" {
				t.Area = f.Area
			}
			next[f.Path] = t
			continue
		}
		next[f.Path] = &touch{Area: f.Area, At: now, Session: s.Key, FromGit: true}
		added = append(added, f)
	}
	removed := 0
	for p := range c.Footprint {
		if _, ok := next[p]; !ok {
			removed++
		}
	}
	c.Footprint = next
	c.FootprintTruncated = fp.Truncated || len(fp.Files) > len(files)
	if len(added) > 0 || removed > 0 {
		b.record(Activity{At: now, Kind: ActivityFootprintReconciled, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
			Text: fmt.Sprintf("%d changed files (+%d, -%d)", len(next), len(added), removed)})
	}
	if len(added) > 0 {
		b.alertOthers(now, c, added)
		b.reportUnchecked(now, c, s, added)
	}
}

func cleanFootprint(in []PathRef, limit int) []PathRef {
	out := make([]PathRef, 0, min(len(in), limit))
	seen := map[string]bool{}
	for _, f := range in {
		if len(out) >= limit {
			break
		}
		p, err := glob.CleanPath(f.Path)
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		area, _ := glob.CleanPath(f.Area)
		out = append(out, PathRef{Path: p, Area: area})
	}
	return out
}

// touch records files a hook saw being written.
func (b *Board) touch(now time.Time, c *claim, s *session, paths []PathRef) {
	if len(paths) == 0 {
		return
	}
	var fresh []PathRef
	for _, p := range paths {
		if t, ok := c.Footprint[p.Path]; ok {
			t.At, t.Session, t.FromGit = now, s.Key, false
			if p.Area != "" {
				t.Area = p.Area
			}
			continue
		}
		if len(c.Footprint) >= b.cfg.MaxFootprint {
			c.FootprintTruncated = true
			continue
		}
		c.Footprint[p.Path] = &touch{Area: p.Area, At: now, Session: s.Key}
		fresh = append(fresh, p)
	}
	b.record(Activity{At: now, Kind: ActivityFileChanged, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent, Paths: pathsOf(paths)})
	if len(fresh) > 0 {
		b.alertOthers(now, c, fresh)
	}
}

// alertOthers tells every other claim that changed or claimed the same files.
func (b *Board) alertOthers(now time.Time, c *claim, paths []PathRef) {
	for _, o := range b.claimsInRepo(c.Repo) {
		if o.ID == c.ID {
			continue
		}
		var hit []string
		for _, p := range paths {
			if _, ok := o.Footprint[p.Path]; !ok && !coveredByIntent(o, p.Path) {
				continue
			}
			k := "touch|" + c.ID + "|" + p.Path
			if o.Alerted[k] {
				continue
			}
			if o.Alerted == nil {
				o.Alerted = map[string]bool{}
			}
			o.Alerted[k] = true
			hit = append(hit, p.Path)
		}
		if len(hit) == 0 {
			continue
		}
		b.statsOf(c.Repo, now).Alerts++
		b.enqueue(now, o, InboxItem{
			Kind:      "overlap",
			FromClaim: c.ID,
			From:      c.Member,
			Paths:     hit,
			Text:      fmt.Sprintf("%s also changed %s%s.", who(c), listPaths(hit, 5), onBranch(c)),
		})
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
		cf, ok := blockerOf(p, holders)
		if !ok {
			continue
		}
		// Once per file, reservation and policy: a new reservation, or a
		// policy that now refuses what it only warned about, is news.
		if k := uncheckedKey(action, cf); !c.Alerted[k] {
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
	for _, o := range b.sessions {
		if trace != nil {
			trace.sessionVisits++
		}
		if o != s && o.ClaimID == c.ID && b.state(now, o).Live() {
			o.Pending = joinBlocks(o.Pending, text)
		}
	}
}

// reservationHolders lists the claims whose exclusive intents can block a
// change in c's worktree: the live ones in its repository, other than c,
// that hold one. They are in ID order.
func (b *Board) reservationHolders(now time.Time, c *claim) []*claim {
	var holders []*claim
	for _, o := range b.claims {
		if o.Repo == c.Repo && o.ID != c.ID && slices.ContainsFunc(o.Intents, func(in Intent) bool { return in.Mode == ModeExclusive }) {
			holders = append(holders, o)
		}
	}
	if len(holders) == 0 {
		return nil
	}
	live := b.liveClaims(now)
	holders = slices.DeleteFunc(holders, func(o *claim) bool { return !live[o.ID] })
	slices.SortFunc(holders, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
	return holders
}

// blockerOf is the block conflict conflictsFor lists first for p, given the
// claims that hold reservations: of each holder's first exclusive intent that
// covers p, the first in the order conflicts are sorted.
func blockerOf(p PathRef, holders []*claim) (Conflict, bool) {
	var best Conflict
	var by *Intent
	for _, o := range holders {
		for i := range o.Intents {
			in := &o.Intents[i]
			if in.Mode != ModeExclusive || !match(in.Pattern, p.Path) {
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
		if in.Mode == ModeExclusive && match(in.Pattern, cf.Path) && !t.At.Before(in.DeclaredAt) {
			return true
		}
	}
	return false
}

func coveredByIntent(c *claim, path string) bool {
	for _, in := range c.Intents {
		if match(in.Pattern, path) {
			return true
		}
	}
	return false
}

func (b *Board) claimsInRepo(repo string) []*claim {
	var out []*claim
	for _, c := range b.claims {
		if c.Repo == repo {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// releaseIfDone removes a claim that has nothing left to tell anyone.
func (b *Board) releaseIfDone(now time.Time, c *claim) {
	if len(c.Intents) == 0 && len(c.Footprint) == 0 && !b.liveClaims(now)[c.ID] {
		b.deleteClaim(now, c, ActivityClaimReleased)
	}
}

func (b *Board) deleteClaim(now time.Time, c *claim, kind ActivityKind) {
	delete(b.claims, c.ID)
	delete(b.byKey, c.key())
	b.record(Activity{At: now, Kind: kind, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Text: c.Branch})
}

// --- conflicts and decisions ---------------------------------------------

// conflictsFor lists the claims that matter to one path, most severe first.
func (b *Board) conflictsFor(now time.Time, self *claim, selfSession string, p PathRef, live map[string]bool, liveSess map[string]bool) []Conflict {
	repo := ""
	if self != nil {
		repo = self.Repo
	}
	if trace != nil {
		trace.conflictsFor++
	}
	var out []Conflict
	for _, o := range b.claims {
		if o.Repo != repo || (self != nil && o.ID == self.ID) {
			continue
		}
		if cf, ok := b.conflictWith(now, o, p, live[o.ID]); ok {
			out = append(out, cf)
		}
	}
	if self != nil {
		// Only a hook says which session wrote a file; a file git found was
		// written by someone in this worktree, perhaps the asking session.
		if t, ok := self.Footprint[p.Path]; ok && !t.FromGit && t.Session != "" && t.Session != selfSession && liveSess[t.Session] {
			out = append(out, Conflict{
				Path: p.Path, Area: p.Area, Severity: SeverityOverlap, ClaimID: self.ID, Member: self.Member, Branch: self.Branch, Task: self.Task,
				Why: "another live session in this same worktree changed this file", Since: t.At, Active: true, SameClaim: true,
			})
		}
	}
	sortConflicts(out)
	return out
}

func (b *Board) conflictWith(now time.Time, o *claim, p PathRef, active bool) (Conflict, bool) {
	if trace != nil {
		trace.conflictWith++
	}
	best := Conflict{Path: p.Path, Area: p.Area, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: active}
	consider := func(sev Severity, why, pattern string, since time.Time) {
		if sev > best.Severity {
			best.Severity, best.Why, best.Pattern, best.Since = sev, why, pattern, since
		}
	}
	for _, in := range o.Intents {
		if !match(in.Pattern, p.Path) {
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
		if since, ok := workedInArea(o, p.Area); ok {
			consider(SeverityNearby, "is working in the same area "+p.Area, "", since)
		}
	}
	return best, best.Severity > SeverityNone
}

// intentWhy says why an intent makes a conflict.
func intentWhy(in Intent) string {
	why := fmt.Sprintf("declared %s intent %s", in.Mode, in.Pattern)
	if in.Summary != "" {
		why += ": " + quote(in.Summary)
	}
	return why
}

// workedInArea reports whether a claim changed or declared anything in area.
func workedInArea(c *claim, area string) (time.Time, bool) {
	if trace != nil {
		trace.workedInArea++
		trace.areaVisits += len(c.Footprint)
	}
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
		if match(area, dir) || match(dir, area) {
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
// if it runs into a collision this session has not been told about. It also
// returns the one-time answers the check spent, the bumps it showed and the
// questions an agent that cannot ask is told to put, which a refusal the
// agent never hears gives back.
func (b *Board) decide(now time.Time, c *claim, s *session, paths []PathRef, noAsk bool) (HookResult, []string) {
	if s.Acked == nil {
		s.Acked = map[string]bool{}
	}
	v := b.judge(now, c, s, paths, noAsk)
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
	named := map[string]bool{top.ClaimID: true}
	for _, cf := range fresh {
		if !named[cf.ClaimID] {
			named[cf.ClaimID] = true
			also = append(also, fmt.Sprintf("%s on %s: %s", cf.Member, cf.Path, cf.Why))
		}
	}
	b.record(Activity{At: now, Kind: ActivityConflict, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
		Paths: pathsOf(paths), Severity: top.Severity, Decision: d, Also: also,
		Text: fmt.Sprintf("%s → %s (%s)", top.Path, top.Member, top.Why)})
}

// judge applies the policy to the conflicts on every path being written. A
// bump counts as acknowledged as soon as it is met: its retry goes through.
func (b *Board) judge(now time.Time, c *claim, s *session, paths []PathRef, noAsk bool) verdict {
	live, liveSess := b.liveClaims(now), b.liveSessions(now)
	var v verdict
	for _, p := range paths {
		for _, cf := range b.conflictsFor(now, c, s.Key, p, live, liveSess) {
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
	res := HookResult{Decision: DecisionAllow, Conflicts: v.all}
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
	if over := len(keep) - maxInbox; over > 0 {
		keep = keep[over:]
	}
	c.Inbox = keep
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
}

// Declare records intents. An exclusive intent that overlaps another active
// claim's exclusive intent is rejected; the caller may declare it shared.
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
	b.changed()
	c := b.claimFor(now, r.Member, w)
	live := b.liveClaims(now)
	res := DeclareResult{ClaimID: c.ID}

	for _, pat := range patterns {
		if mode == ModeExclusive {
			if against, ok := b.exclusiveClash(c, pat, live); ok {
				res.Rejected = append(res.Rejected, Rejection{
					Pattern: pat,
					Reason: fmt.Sprintf("%s's agent holds %s exclusively, and a shared intent of yours would not change "+
						"that: work elsewhere, ask them with a note, or tell your user", against.Member, against.Pattern),
					Against: against,
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
	res.Text = renderDeclare(now, res, b.cfg.Policy)
	return res, nil
}

func (b *Board) exclusiveClash(self *claim, pattern string, live map[string]bool) (Conflict, bool) {
	for _, o := range b.claimsInRepo(self.Repo) {
		if o.ID == self.ID || !live[o.ID] {
			continue
		}
		for _, in := range o.Intents {
			if in.Mode == ModeExclusive && overlap(in.Pattern, pattern) {
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
func (b *Board) intentOverlaps(c *claim, in Intent, live map[string]bool) []Conflict {
	var out []Conflict
	for _, o := range b.claimsInRepo(c.Repo) {
		if o.ID == c.ID {
			continue
		}
		cf := Conflict{Path: in.Pattern, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: live[o.ID]}
		for _, oi := range o.Intents {
			if overlap(oi.Pattern, in.Pattern) {
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
				if match(in.Pattern, p) {
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

func (b *Board) tellIntent(now time.Time, c *claim, accepted []Intent, overlaps []Conflict) {
	notified := map[string]bool{}
	for _, cf := range overlaps {
		if notified[cf.ClaimID] {
			continue
		}
		o := b.claims[cf.ClaimID]
		if o == nil {
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

// Check lists conflicts for paths without changing anything.
func (b *Board) Check(now time.Time, r CheckRequest) ([]Conflict, error) {
	w, err := cleanWhere(r.Where)
	if err != nil {
		return nil, err
	}
	paths, err := cleanPaths(r.Paths)
	if err != nil {
		return nil, err
	}
	b.lock()
	defer b.unlock()
	self := b.findClaim(r.Member, w)
	if self == nil {
		self = &claim{Repo: w.Repo, Member: r.Member}
	}
	live, liveSess := b.liveClaims(now), b.liveSessions(now)
	var out []Conflict
	for _, p := range paths {
		out = append(out, b.conflictsFor(now, self, "", p, live, liveSess)...)
	}
	return out, nil
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
}

// NoteResult lists the claims a note was queued for.
type NoteResult struct {
	Delivered []string `json:"delivered"`
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
	targets, path := b.resolve(w.Repo, to, self)
	if len(targets) == 0 {
		return NoteResult{}, fmt.Errorf("%w: %q", ErrNoTarget, to)
	}
	b.changed()
	b.notes[r.Member] = append(recent, now)
	from := &claim{Member: r.Member}
	if self != nil {
		from = self
	}
	var res NoteResult
	for _, t := range targets {
		sender := who(from)
		if r.ByPerson {
			sender = from.Member
		}
		b.enqueue(now, t, InboxItem{Kind: "note", FromClaim: from.ID, From: r.Member, Text: fmt.Sprintf("Note from %s: %s", sender, quote(text))})
		res.Delivered = append(res.Delivered, t.ID)
	}
	b.statsOf(w.Repo, now).Notes += len(targets)
	note := Activity{At: now, Kind: ActivityNoteSent, Repo: w.Repo, Member: r.Member, ClaimID: from.ID}
	if path != "" {
		note.Paths, note.Text = []string{path}, fmt.Sprintf("to whoever works on %s: %s", path, text)
	} else {
		note.Text = fmt.Sprintf("to %s: %s", targets[0].Member, text)
	}
	b.record(note)
	return res, nil
}

// resolve finds a note's recipients: a claim by ID, a member's claims, or the
// claims that changed or reserved a path, which it also returns.
func (b *Board) resolve(repo, to string, self *claim) ([]*claim, string) {
	if c, ok := b.claims[to]; ok && c.Repo == repo {
		return []*claim{c}, ""
	}
	var out []*claim
	for _, c := range b.claimsInRepo(repo) {
		if self != nil && c.ID == self.ID {
			continue
		}
		if c.Member == to {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return out, ""
	}
	p, err := glob.CleanPath(to)
	if err != nil {
		return nil, ""
	}
	for _, c := range b.claimsInRepo(repo) {
		if self != nil && c.ID == self.ID {
			continue
		}
		if _, ok := c.Footprint[p]; ok || coveredByIntent(c, p) {
			out = append(out, c)
		}
	}
	return out, p
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
			delete(b.sessions, k)
			changed = true
			continue
		}
		held[s.ClaimID] = true
		if st.Live() {
			live[s.ClaimID] = true
		}
	}
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
		case len(c.Intents) == 0 && len(c.Footprint) == 0 && !held[c.ID]:
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
