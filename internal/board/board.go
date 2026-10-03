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
	ErrNotFound    = errors.New("not found")
)

// Config holds the board's timings and policy.
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

const (
	maxTaskLen    = 200
	maxNoteLen    = 600
	maxBranchLen  = 120
	maxPaths      = 200
	maxIntents    = 50
	maxInbox      = 50
	inboxTTL      = 24 * time.Hour
	keepEndedFor  = time.Hour
	noteWindow    = time.Minute
	idAlphabetLen = 8
)

// Board holds every claim and session the server knows about.
type Board struct {
	mu       sync.Mutex
	notifyMu sync.Mutex // orders notifications; taken before mu is released
	cfg      Config
	claims   map[string]*Claim
	byKey    map[string]string
	sessions map[string]*Session
	recent   []Activity
	seq      uint64
	version  uint64
	notes    map[string][]time.Time
	stats    map[string]*Stats
	pending  []Activity
	notify   func([]Activity)
	newID    func(prefix string) string
}

// Option configures a Board.
type Option func(*Board)

// WithNotify registers fn to receive new activities. It is called without the
// board's lock held, one call at a time, in the order activities happened; it
// must not block or call back into the board.
func WithNotify(fn func([]Activity)) Option { return func(b *Board) { b.notify = fn } }

// WithIDs replaces the random ID generator, for tests.
func WithIDs(fn func(prefix string) string) Option { return func(b *Board) { b.newID = fn } }

// New returns an empty board.
func New(cfg Config, opts ...Option) *Board {
	b := &Board{
		cfg:      cfg,
		claims:   map[string]*Claim{},
		byKey:    map[string]string{},
		sessions: map[string]*Session{},
		notes:    map[string][]time.Time{},
		stats:    map[string]*Stats{},
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
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))[:idAlphabetLen]
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
		b.recent = append(b.recent[:0:0], b.recent[over:]...)
	}
	b.pending = append(b.pending, a)
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

func (b *Board) findClaim(member string, w Where) *Claim {
	if id, ok := b.byKey[claimKey(w.Repo, member, w.Host, w.Worktree)]; ok {
		return b.claims[id]
	}
	return nil
}

func (b *Board) claimFor(now time.Time, member string, w Where) *Claim {
	if c := b.findClaim(member, w); c != nil {
		if w.Branch != "" {
			c.Branch = w.Branch
		}
		return c
	}
	c := &Claim{
		ID:        b.newID("c_"),
		Repo:      w.Repo,
		Member:    member,
		Host:      w.Host,
		Worktree:  w.Worktree,
		Branch:    w.Branch,
		Footprint: map[string]*Touch{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	b.claims[c.ID] = c
	b.byKey[c.key()] = c.ID
	b.record(Activity{At: now, Kind: "claim.opened", Repo: c.Repo, Member: member, ClaimID: c.ID, Text: c.Branch})
	return c
}

func (b *Board) sessionFor(now time.Time, ev HookEvent, c *Claim) *Session {
	key := sessionKey(ev.Member, ev.Agent, ev.SessionID)
	s, ok := b.sessions[key]
	if !ok {
		s = &Session{
			Key:       key,
			ID:        ev.SessionID,
			Member:    ev.Member,
			Agent:     ev.Agent,
			ClaimID:   c.ID,
			StartedAt: now,
			LastSeen:  now,
			Phase:     PhaseWaiting,
		}
		b.sessions[key] = s
		b.record(Activity{At: now, Kind: "session.started", Repo: c.Repo, Member: ev.Member, ClaimID: c.ID, Session: ev.SessionID, Agent: ev.Agent})
	}
	s.ClaimID = c.ID
	return s
}

// state derives a session's liveness.
func (b *Board) state(now time.Time, s *Session) State {
	if s.Phase == PhaseEnded {
		return StateEnded
	}
	silent := now.Sub(s.LastSeen)
	if silent > b.cfg.IdleAfter {
		return StateGone
	}
	if s.Phase == PhaseWorking {
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
		if b.state(now, s).Live() {
			live[s.ClaimID] = true
		}
	}
	return live
}

func (b *Board) liveSessions(now time.Time) map[string]bool {
	live := map[string]bool{}
	for k, s := range b.sessions {
		if b.state(now, s).Live() {
			live[k] = true
		}
	}
	return live
}

// --- hook events ----------------------------------------------------------

// Hook applies one lifecycle event and returns what the agent should be told.
// It never refuses on error: callers should allow the agent to continue.
func (b *Board) Hook(now time.Time, ev HookEvent) (HookResult, error) {
	allow := HookResult{Decision: Allow}
	w, err := cleanWhere(ev.Where)
	if err != nil {
		return allow, err
	}
	ev.Where = w
	ev.SessionID = Clean(ev.SessionID, 200)
	ev.Agent = Agent(ident(string(ev.Agent), 40))
	ev.Tool = ident(ev.Tool, 60)
	ev.ToolUseID = Clean(ev.ToolUseID, 200)
	if ev.Member == "" || ev.SessionID == "" || ev.Agent == "" {
		return allow, fmt.Errorf("%w: member, agent and session_id are required", ErrInvalid)
	}
	if !knownKinds[ev.Kind] {
		// Before anything is created: an event this board does not know (from
		// a newer client, say) must not bring a dormant claim back to life.
		return allow, fmt.Errorf("%w: unknown event kind %q", ErrInvalid, ev.Kind)
	}
	paths, err := cleanPaths(ev.Paths)
	if err != nil {
		return allow, err
	}
	ev.Paths = paths

	b.lock()
	defer b.unlock()
	b.changed()

	c := b.claimFor(now, ev.Member, w)
	s := b.sessionFor(now, ev, c)
	was := b.state(now, s)
	res := HookResult{Decision: Allow, ClaimID: c.ID}

	switch ev.Kind {
	case KindSessionStart:
		if s.Phase == PhaseEnded {
			s.Phase = PhaseWaiting
		}
		clearTools(s)
		b.reconcile(now, c, s, ev.Footprint)
		res.Context = joinBlocks(b.renderStart(now, c), b.deliver(now, c, s))
	case KindPrompt:
		s.Phase = PhaseWorking
		clearTools(s)
		b.taskFromPrompt(c, s, ev.Prompt)
		res.Context = b.deliver(now, c, s)
	case KindToolStart:
		startTool(now, s, ev.Tool, ev.ToolUseID)
	case KindPreEdit:
		startTool(now, s, ev.Tool, ev.ToolUseID)
		res = b.decide(now, c, s, ev.Paths, ev.NoAsk)
		res.ClaimID = c.ID
		if res.Decision == Refuse || (res.Decision == DecideAsk && ev.NoAsk) {
			// The edit does not run, so no tool end will follow it. (An ask that
			// the person answers runs, or not, and the agent reports either.)
			endTool(s, ev.ToolUseID)
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
		s.Phase = PhaseWaiting
		clearTools(s)
		b.reconcile(now, c, s, ev.Footprint)
	case KindSessionEnd:
		s.Phase = PhaseEnded
		clearTools(s)
		b.reconcile(now, c, s, ev.Footprint)
		b.record(Activity{At: now, Kind: "session.ended", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent})
	case KindHeartbeat:
		if s.Phase == PhaseEnded {
			s.Phase = PhaseWaiting
		}
		b.reconcile(now, c, s, ev.Footprint)
		res.Context = b.deliver(now, c, s)
	default:
		return allow, fmt.Errorf("%w: unknown event kind %q", ErrInvalid, ev.Kind)
	}

	s.LastSeen = now
	c.UpdatedAt = now
	if is := b.state(now, s); is != was && (was == StateStalled || was == StateGone) {
		b.record(Activity{At: now, Kind: "session.recovered", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent})
	}
	s.Reported = b.state(now, s)
	if ev.Kind == KindSessionEnd {
		b.releaseIfDone(now, c)
	}
	return res, nil
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
func startTool(now time.Time, s *Session, tool, id string) {
	s.Phase = PhaseWorking
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
func endTool(s *Session, id string) {
	s.Phase = PhaseWorking
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

func inTool(s *Session) bool { return s.InFlight > 0 || len(s.Calls) > 0 }

// clearTools forgets the tools in flight, at the boundaries where none can be:
// a prompt, a stop, the session's start and end. An end event that never came
// cannot keep a session inside a tool past them.
func clearTools(s *Session) {
	s.InFlight, s.Calls, s.Tool, s.ToolSince = 0, nil, "", time.Time{}
}

func (b *Board) taskFromPrompt(c *Claim, s *Session, prompt string) {
	prompt = Clean(firstLine(prompt), maxTaskLen)
	if prompt == "" || s.Acked["task:prompted"] {
		return
	}
	if s.Acked == nil {
		s.Acked = map[string]bool{}
	}
	s.Acked["task:prompted"] = true
	if c.Task == "" || !c.taskFromIntent() {
		c.Task = prompt
	}
}

// taskFromIntent reports whether the task came from a declared intent, which
// a prompt must not overwrite.
func (c *Claim) taskFromIntent() bool {
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
func (b *Board) reconcile(now time.Time, c *Claim, s *Session, fp *Footprint) {
	if fp == nil {
		return
	}
	files := cleanFootprint(fp.Files, b.cfg.MaxFootprint)
	next := make(map[string]*Touch, len(files))
	var added []PathRef
	for _, f := range files {
		if t, ok := c.Footprint[f.Path]; ok {
			if f.Area != "" {
				t.Area = f.Area
			}
			next[f.Path] = t
			continue
		}
		next[f.Path] = &Touch{Area: f.Area, At: now, Session: s.Key, FromGit: true}
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
		b.record(Activity{At: now, Kind: "footprint.reconciled", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
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
func (b *Board) touch(now time.Time, c *Claim, s *Session, paths []PathRef) {
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
		c.Footprint[p.Path] = &Touch{Area: p.Area, At: now, Session: s.Key}
		fresh = append(fresh, p)
	}
	b.record(Activity{At: now, Kind: "file.changed", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent, Paths: pathsOf(paths)})
	if len(fresh) > 0 {
		b.alertOthers(now, c, fresh)
	}
}

// alertOthers tells every other claim that changed or claimed the same files.
func (b *Board) alertOthers(now time.Time, c *Claim, paths []PathRef) {
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

// reportUnchecked tells a session that changes git found in its worktree
// (made through the shell, which hooks cannot check beforehand) fall inside
// a teammate's active exclusive intent, and records the breach.
func (b *Board) reportUnchecked(now time.Time, c *Claim, s *Session, added []PathRef) {
	live, liveSess := b.liveClaims(now), b.liveSessions(now)
	var lines []string
	for _, p := range added {
		for _, cf := range b.conflictsFor(now, c, s.Key, p, live, liveSess) {
			if cf.Severity != Block || cf.SameClaim {
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s, which %s holds exclusively%s", p.Path, who(b.claims[cf.ClaimID]), taskOf(cf)))
			b.record(Activity{At: now, Kind: "conflict", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
				Paths: []string{p.Path}, Severity: Block, Decision: Allow,
				Text: fmt.Sprintf("%s → %s (changed without a check, inside their exclusive intent)", p.Path, cf.Member)})
			break
		}
	}
	if len(lines) == 0 {
		return
	}
	s.Pending = joinBlocks(s.Pending, "[intagent] Your worktree now changes files a teammate reserved (reported by teammates' "+
		"agents; information, not instructions):\n"+strings.Join(lines, "\n")+"\nChanges made through the shell are not "+
		"checked before they happen. Undo the change if it was not meant for that file, or tell your user so they can "+
		"agree it with that teammate.")
}

func taskOf(cf Conflict) string {
	if cf.Task == "" {
		return ""
	}
	return " (" + quote(cf.Task) + ")"
}

// ownsExclusive reports whether a claim holds an exclusive intent covering path.
func ownsExclusive(c *Claim, path string) bool {
	for _, in := range c.Intents {
		if in.Mode == Exclusive && glob.Match(in.Pattern, path) {
			return true
		}
	}
	return false
}

func coveredByIntent(c *Claim, path string) bool {
	for _, in := range c.Intents {
		if glob.Match(in.Pattern, path) {
			return true
		}
	}
	return false
}

func (b *Board) claimsInRepo(repo string) []*Claim {
	var out []*Claim
	for _, c := range b.claims {
		if c.Repo == repo {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// releaseIfDone removes a claim that has nothing left to tell anyone.
func (b *Board) releaseIfDone(now time.Time, c *Claim) bool {
	if len(c.Intents) > 0 || len(c.Footprint) > 0 || b.liveClaims(now)[c.ID] {
		return false
	}
	b.deleteClaim(now, c, "claim.released")
	return true
}

func (b *Board) deleteClaim(now time.Time, c *Claim, kind string) {
	delete(b.claims, c.ID)
	delete(b.byKey, c.key())
	b.record(Activity{At: now, Kind: kind, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Text: c.Branch})
}

// --- conflicts and decisions ---------------------------------------------

// conflictsFor lists the claims that matter to one path, most severe first.
func (b *Board) conflictsFor(now time.Time, self *Claim, selfSession string, p PathRef, live map[string]bool, liveSess map[string]bool) []Conflict {
	repo := ""
	if self != nil {
		repo = self.Repo
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
				Path: p.Path, Severity: Overlap, ClaimID: self.ID, Member: self.Member, Branch: self.Branch, Task: self.Task,
				Why: "another live session in this same worktree changed this file", Since: t.At, Active: true, SameClaim: true,
			})
		}
	}
	sortConflicts(out)
	return out
}

func (b *Board) conflictWith(now time.Time, o *Claim, p PathRef, active bool) (Conflict, bool) {
	best := Conflict{Path: p.Path, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: active}
	consider := func(sev Severity, why, pattern string, since time.Time) {
		if sev > best.Severity {
			best.Severity, best.Why, best.Pattern, best.Since = sev, why, pattern, since
		}
	}
	for _, in := range o.Intents {
		if !glob.Match(in.Pattern, p.Path) {
			continue
		}
		sev := Overlap
		if in.Mode == Exclusive && active {
			sev = Block
		}
		why := fmt.Sprintf("declared %s intent %s", in.Mode, in.Pattern)
		if in.Summary != "" {
			why += ": " + quote(in.Summary)
		}
		consider(sev, why, in.Pattern, in.DeclaredAt)
	}
	if t, ok := o.Footprint[p.Path]; ok {
		sev := Overlap
		why := "has unmerged changes to this file"
		if !active && now.Sub(o.UpdatedAt) > b.cfg.DormantFor {
			sev = Nearby
			why = "had unmerged changes to this file (claim dormant for " + ago(now, o.UpdatedAt) + ")"
		}
		consider(sev, why, "", t.At)
	}
	if best.Severity < Nearby && p.Area != "" {
		if since, ok := workedInArea(o, p.Area); ok {
			consider(Nearby, "is working in the same area "+p.Area, "", since)
		}
	}
	return best, best.Severity > SeverityNone
}

// workedInArea reports whether a claim changed or declared anything in area.
func workedInArea(c *Claim, area string) (time.Time, bool) {
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

func sortConflicts(cs []Conflict) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Severity != cs[j].Severity {
			return cs[i].Severity > cs[j].Severity
		}
		if cs[i].Active != cs[j].Active {
			return cs[i].Active
		}
		return cs[i].Since.After(cs[j].Since)
	})
}

func ackKey(c Conflict) string {
	subject := c.Path
	if c.Severity == Nearby {
		subject = "area"
	}
	return c.Severity.String() + "|" + c.ClaimID + "|" + subject
}

// decide applies the policy to the conflicts on every path being written.
func (b *Board) decide(now time.Time, c *Claim, s *Session, paths []PathRef, noAsk bool) HookResult {
	live, liveSess := b.liveClaims(now), b.liveSessions(now)
	var refused, asked, warned, all []Conflict
	var askKeys, warnKeys []string
	if s.Acked == nil {
		s.Acked = map[string]bool{}
	}
	for _, p := range paths {
		for _, cf := range b.conflictsFor(now, c, s.Key, p, live, liveSess) {
			all = append(all, cf)
			key := ackKey(cf)
			if cf.Severity == Nearby {
				key = "nearby|" + cf.ClaimID + "|" + p.Area
			}
			action := b.cfg.Policy.action(cf.Severity)
			if cf.Severity == Overlap && action != Off && ownsExclusive(c, p.Path) {
				// Inside its own reservation an agent is told about others'
				// changes, not stopped by them.
				action = Warn
			}
			switch {
			case action == Deny:
				refused = append(refused, cf)
			case action == Ask && !noAsk:
				asked = append(asked, cf)
			case action == Ask && !s.Acked[key]:
				asked = append(asked, cf)
				askKeys = append(askKeys, key)
			case action == Bump && !s.Acked[key]:
				s.Acked[key] = true
				refused = append(refused, cf)
			case action == Warn && !s.Acked[key]:
				warned = append(warned, cf)
				warnKeys = append(warnKeys, key)
			}
		}
	}
	res := HookResult{Decision: Allow, Conflicts: all}
	switch {
	case len(refused) > 0:
		res.Decision = Refuse
		res.Reason = b.renderRefusal(now, refused, b.cfg.Policy)
	case len(asked) > 0:
		res.Decision = DecideAsk
		res.Reason = b.renderRefusal(now, asked, b.cfg.Policy)
		// An agent that cannot ask was told to ask its person; its retry is the
		// answer. Not when a refusal of the same edit hid the question.
		for _, k := range askKeys {
			s.Acked[k] = true
		}
	case len(warned) > 0:
		res.Context = b.renderWarnings(now, warned)
		// A warning counts as told only once it is shown, not when a refusal
		// of the same edit hid it.
		for _, k := range warnKeys {
			s.Acked[k] = true
		}
	}
	acted := refused
	if len(acted) == 0 {
		acted = asked
	}
	if len(acted) == 0 {
		acted = warned
	}
	// A retry that meets the same conflicts again is checked, but it is not
	// a new collision: counting it, or announcing it, would inflate both.
	fresh := false
	for _, cf := range acted {
		if k := "told|" + ackKey(cf); !s.Acked[k] {
			s.Acked[k] = true
			fresh = true
		}
	}
	b.count(now, c.Repo, all, refused, asked, warned, fresh)
	if fresh {
		top := mostSevere(acted)
		b.record(Activity{At: now, Kind: "conflict", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
			Paths: pathsOf(paths), Severity: top.Severity, Decision: res.Decision,
			Text: fmt.Sprintf("%s → %s (%s)", top.Path, top.Member, top.Why)})
	}
	return res
}

// count adds one checked edit to the repository's stats, under its most
// severe conflict and what was done about it.
func (b *Board) count(now time.Time, repo string, all, refused, asked, warned []Conflict, fresh bool) {
	st := b.statsOf(repo, now)
	st.Checks++
	if !fresh && len(refused)+len(asked)+len(warned) > 0 {
		return
	}
	if len(all) > 0 {
		switch mostSevere(all).Severity {
		case Block:
			st.Blocks++
		case Overlap:
			st.Overlaps++
		case Nearby:
			st.Nearby++
		}
	}
	hard := false
	for _, cf := range refused {
		if b.cfg.Policy.action(cf.Severity) == Deny {
			hard = true
		}
	}
	switch {
	case hard:
		st.Refused++
	case len(refused) > 0:
		st.Bumped++
	case len(asked) > 0:
		st.Asked++
	case len(warned) > 0:
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

func (b *Board) enqueue(now time.Time, c *Claim, it InboxItem) {
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
// from before an edit, then the inbox items it has not seen.
func (b *Board) deliver(now time.Time, c *Claim, s *Session) string {
	pending := s.Pending
	s.Pending = ""
	return joinBlocks(pending, b.deliverInbox(now, c, s))
}

func (b *Board) deliverInbox(now time.Time, c *Claim, s *Session) string {
	var items []InboxItem
	for i := range c.Inbox {
		it := &c.Inbox[i]
		if now.Sub(it.At) >= inboxTTL || it.DeliveredTo[s.Key] {
			continue
		}
		if len(items) == b.cfg.InboxPerHook {
			break
		}
		if it.DeliveredTo == nil {
			it.DeliveredTo = map[string]bool{}
		}
		it.DeliveredTo[s.Key] = true
		items = append(items, *it)
	}
	return b.renderInbox(now, items)
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
		if mode == Exclusive {
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
		b.record(Activity{At: now, Kind: "intent.declared", Repo: c.Repo, Member: c.Member, ClaimID: c.ID,
			Paths: intentPatterns(res.Accepted), Text: fmt.Sprintf("%s: %s", mode, summary)})
	}
	res.Text = renderDeclare(now, res, b.cfg.Policy)
	return res, nil
}

func (b *Board) exclusiveClash(self *Claim, pattern string, live map[string]bool) (Conflict, bool) {
	for _, o := range b.claimsInRepo(self.Repo) {
		if o.ID == self.ID || !live[o.ID] {
			continue
		}
		for _, in := range o.Intents {
			if in.Mode == Exclusive && glob.Overlap(in.Pattern, pattern) {
				return Conflict{Path: pattern, Severity: Block, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task,
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
func (b *Board) intentOverlaps(c *Claim, in Intent, live map[string]bool) []Conflict {
	var out []Conflict
	for _, o := range b.claimsInRepo(c.Repo) {
		if o.ID == c.ID {
			continue
		}
		cf := Conflict{Path: in.Pattern, ClaimID: o.ID, Member: o.Member, Branch: o.Branch, Task: o.Task, Active: live[o.ID]}
		for _, oi := range o.Intents {
			if glob.Overlap(oi.Pattern, in.Pattern) {
				sev := Overlap
				if oi.Mode == Exclusive && cf.Active {
					sev = Block
				}
				if sev > cf.Severity {
					cf.Severity, cf.Pattern, cf.Since = sev, oi.Pattern, oi.DeclaredAt
					cf.Why = fmt.Sprintf("declared %s intent %s", oi.Mode, oi.Pattern)
				}
			}
		}
		if cf.Severity < Overlap {
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
				cf.Severity, cf.Since = Overlap, since
				cf.Why = "has unmerged changes to " + listPaths(hit, 3)
			}
		}
		if cf.Severity > SeverityNone {
			out = append(out, cf)
		}
	}
	return out
}

func (b *Board) tellIntent(now time.Time, c *Claim, accepted []Intent, overlaps []Conflict) {
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
		b.record(Activity{At: now, Kind: "intent.released", Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Paths: gone})
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
		self = &Claim{Repo: w.Repo, Member: r.Member}
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
	targets := b.resolve(w.Repo, to, self)
	if len(targets) == 0 {
		return NoteResult{}, fmt.Errorf("%w: %q", ErrNoTarget, to)
	}
	b.changed()
	b.notes[r.Member] = append(recent, now)
	from := &Claim{Member: r.Member}
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
	b.record(Activity{At: now, Kind: "note.sent", Repo: w.Repo, Member: r.Member, ClaimID: from.ID, Text: fmt.Sprintf("to %s: %s", to, text)})
	return res, nil
}

func (b *Board) resolve(repo, to string, self *Claim) []*Claim {
	if c, ok := b.claims[to]; ok && c.Repo == repo {
		return []*Claim{c}
	}
	var out []*Claim
	for _, c := range b.claimsInRepo(repo) {
		if self != nil && c.ID == self.ID {
			continue
		}
		if c.Member == to {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return out
	}
	p, err := glob.CleanPath(to)
	if err != nil {
		return nil
	}
	for _, c := range b.claimsInRepo(repo) {
		if self != nil && c.ID == self.ID {
			continue
		}
		if _, ok := c.Footprint[p]; ok || coveredByIntent(c, p) {
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
				b.record(Activity{At: now, Kind: "session." + string(st), Repo: repo, Member: member, ClaimID: s.ClaimID, Session: s.ID, Agent: s.Agent, Text: text})
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
		case len(c.Intents) == 0 && len(c.Footprint) == 0 && !b.hasSessions(c.ID):
			b.deleteClaim(now, c, "claim.released")
			changed = true
		case now.Sub(c.UpdatedAt) > b.cfg.ForgetAfter:
			b.deleteClaim(now, c, "claim.forgotten")
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

func (b *Board) hasSessions(claimID string) bool {
	for _, s := range b.sessions {
		if s.ClaimID == claimID {
			return true
		}
	}
	return false
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
