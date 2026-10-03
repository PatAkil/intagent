package board

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Agent names the kind of agent behind a session.
type Agent string

// Agents intagent has adapters for. Any other value is accepted and shown as is.
const (
	AgentClaudeCode Agent = "claude-code"
	AgentCodex      Agent = "codex"
	AgentCursor     Agent = "cursor"
	AgentCopilot    Agent = "copilot"
	AgentGemini     Agent = "gemini"
	AgentWatch      Agent = "watch"
	AgentCLI        Agent = "cli"
)

// Mode says whether an intent tolerates other writers.
type Mode string

const (
	// Shared intents announce a plan; others are told but not stopped.
	Shared Mode = "shared"
	// Exclusive intents ask others to stay out while the claim is active.
	Exclusive Mode = "exclusive"
)

// ParseMode accepts "", "shared" and "exclusive".
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", Shared:
		return Shared, nil
	case Exclusive:
		return Exclusive, nil
	}
	return "", fmt.Errorf("unknown mode %q: want shared or exclusive", s)
}

// Phase is the last lifecycle position a session reported.
type Phase string

// Phases a session reports through its hooks.
const (
	PhaseWorking Phase = "working" // a turn is in progress
	PhaseWaiting Phase = "waiting" // the turn ended; the agent waits for its person
	PhaseEnded   Phase = "ended"   // the session ended cleanly
)

// State is a session's liveness: its phase, adjusted for how long it has been silent.
type State string

// Liveness states, derived from a session's phase and silence.
const (
	StateWorking State = "working" // a turn is in progress and the agent reports in
	StateWaiting State = "waiting" // between turns, recently seen
	StateStalled State = "stalled" // working, but silent too long
	StateGone    State = "gone"    // silent for so long it is presumed dead
	StateEnded   State = "ended"   // ended cleanly
)

// Live reports whether a session in this state still holds its claim's intents.
func (s State) Live() bool { return s == StateWorking || s == StateWaiting }

// Severity ranks how much another claim matters to a path.
type Severity int

// Severities, from least to most serious.
const (
	// SeverityNone means no other claim matters.
	SeverityNone Severity = iota
	// Nearby means another claim works in the same area.
	Nearby
	// Overlap means another claim changed this path or plans to.
	Overlap
	// Block means an active claim holds an exclusive intent on this path.
	Block
)

var severityNames = [...]string{"none", "nearby", "overlap", "block"}

func (s Severity) String() string {
	if s < 0 || int(s) >= len(severityNames) {
		return fmt.Sprintf("severity(%d)", int(s))
	}
	return severityNames[s]
}

// MarshalText writes the severity's name.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads a severity's name.
func (s *Severity) UnmarshalText(b []byte) error {
	for i, n := range severityNames {
		if n == string(b) {
			*s = Severity(i)
			return nil
		}
	}
	return fmt.Errorf("unknown severity %q", b)
}

// Action is what the policy does about a severity.
type Action string

const (
	// Deny refuses the edit every time.
	Deny Action = "deny"
	// Ask hands the decision to the person running the agent.
	Ask Action = "ask"
	// Bump refuses the first attempt with an explanation and allows a retry.
	Bump Action = "bump"
	// Warn allows the edit and adds context once.
	Warn Action = "warn"
	// Off ignores the severity.
	Off Action = "off"
)

// ParseAction validates an action name.
func ParseAction(s string) (Action, error) {
	switch a := Action(s); a {
	case Deny, Ask, Bump, Warn, Off:
		return a, nil
	}
	return "", fmt.Errorf("unknown action %q: want deny, ask, bump, warn or off", s)
}

// Policy maps each severity to an action.
type Policy struct {
	Block   Action `json:"block"`
	Overlap Action `json:"overlap"`
	Nearby  Action `json:"nearby"`
}

// DefaultPolicy refuses edits under another active exclusive intent, makes an
// agent acknowledge an overlap before editing, and mentions nearby work.
func DefaultPolicy() Policy { return Policy{Block: Deny, Overlap: Bump, Nearby: Warn} }

func (p Policy) action(s Severity) Action {
	switch s {
	case Block:
		return p.Block
	case Overlap:
		return p.Overlap
	case Nearby:
		return p.Nearby
	}
	return Off
}

// Decision is the answer to an agent about to write.
type Decision string

// Decisions a hook can return.
const (
	Allow     Decision = "allow" // go ahead
	DecideAsk Decision = "ask"   // let the person decide
	Refuse    Decision = "deny"  // do not write; the reason says why
)

// Intent is a declared plan to change the paths a pattern covers.
type Intent struct {
	Pattern    string    `json:"pattern"`
	Mode       Mode      `json:"mode"`
	Summary    string    `json:"summary,omitempty"`
	DeclaredAt time.Time `json:"declared_at"`
}

// Touch records that a claim changed a file.
type Touch struct {
	Area    string    `json:"area,omitempty"`
	At      time.Time `json:"at"`
	Session string    `json:"session,omitempty"`
	// FromGit is true when the change was found by reconciling with git rather
	// than reported by a hook.
	FromGit bool `json:"from_git,omitempty"`
}

// InboxItem is something a claim's agents should hear at their next hook.
type InboxItem struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	FromClaim string    `json:"from_claim,omitempty"`
	From      string    `json:"from,omitempty"`
	Text      string    `json:"text"`
	Paths     []string  `json:"paths,omitempty"`
	// DeliveredTo holds the keys of sessions that have seen the item.
	DeliveredTo map[string]bool `json:"delivered_to,omitempty"`
}

// Claim is the unit of ownership: one member's work in one worktree.
type Claim struct {
	ID        string            `json:"id"`
	Repo      string            `json:"repo"`
	Member    string            `json:"member"`
	Host      string            `json:"host"`
	Worktree  string            `json:"worktree"`
	Branch    string            `json:"branch,omitempty"`
	Task      string            `json:"task,omitempty"`
	Intents   []Intent          `json:"intents,omitempty"`
	Footprint map[string]*Touch `json:"footprint,omitempty"`
	// FootprintTruncated is set when git reported more files than intagent keeps.
	FootprintTruncated bool        `json:"footprint_truncated,omitempty"`
	Inbox              []InboxItem `json:"inbox,omitempty"`
	// Alerted remembers which symmetric alerts this claim already received.
	Alerted   map[string]bool `json:"alerted,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func claimKey(repo, member, host, worktree string) string {
	b, _ := json.Marshal([]string{repo, member, host, worktree})
	return string(b)
}

func (c *Claim) key() string { return claimKey(c.Repo, c.Member, c.Host, c.Worktree) }

// Session is one agent run attached to a claim.
type Session struct {
	Key       string    `json:"key"`
	ID        string    `json:"id"`
	Member    string    `json:"member"`
	Agent     Agent     `json:"agent"`
	ClaimID   string    `json:"claim_id"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
	Phase     Phase     `json:"phase"`
	// Tool is the latest tool call in progress, if any; ToolSince is when the
	// session went into tools. Calls holds the running calls an agent gave
	// ids for, and InFlight counts those it gave none for.
	Tool      string          `json:"tool,omitempty"`
	ToolSince time.Time       `json:"tool_since,omitzero"`
	Calls     map[string]bool `json:"calls,omitempty"`
	InFlight  int             `json:"in_flight,omitempty"`
	// Acked holds the conflicts this session has already been told about.
	Acked map[string]bool `json:"acked,omitempty"`
	// Reported is the last derived state announced as an activity.
	Reported State `json:"reported,omitempty"`
	// Pending is context from before an edit, held for an agent that only
	// reads context after a tool has run.
	Pending string `json:"pending,omitempty"`
}

func sessionKey(member string, agent Agent, id string) string {
	b, _ := json.Marshal([]string{member, string(agent), id})
	return string(b)
}

// PathRef is a repo-relative path with the area it belongs to.
type PathRef struct {
	Path string `json:"path"`
	Area string `json:"area,omitempty"`
}

// Footprint is a claim's changed files as git sees them.
type Footprint struct {
	Files []PathRef `json:"files"`
	// Truncated is set when the client capped the list.
	Truncated bool `json:"truncated,omitempty"`
}

// Kind is a normalised lifecycle event.
type Kind string

// Event kinds, one per lifecycle moment the adapters recognise.
const (
	KindSessionStart Kind = "session_start"
	KindPrompt       Kind = "prompt"
	KindPreEdit      Kind = "pre_edit"
	KindPostEdit     Kind = "post_edit"
	KindToolStart    Kind = "tool_start"
	KindToolEnd      Kind = "tool_end"
	KindStop         Kind = "stop"
	KindSessionEnd   Kind = "session_end"
	KindHeartbeat    Kind = "heartbeat"
)

// Where identifies the worktree an event comes from.
type Where struct {
	Repo     string `json:"repo"`
	Host     string `json:"host"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch,omitempty"`
}

// HookEvent is one lifecycle event from an agent, already normalised by an
// adapter. Member is filled in by the server from the caller's token.
type HookEvent struct {
	Kind      Kind       `json:"kind"`
	Member    string     `json:"-"`
	Agent     Agent      `json:"agent"`
	SessionID string     `json:"session_id"`
	Where     Where      `json:"where"`
	Tool      string     `json:"tool,omitempty"`
	ToolUseID string     `json:"tool_use_id,omitempty"`
	Paths     []PathRef  `json:"paths,omitempty"`
	Prompt    string     `json:"prompt,omitempty"`
	Footprint *Footprint `json:"footprint,omitempty"`
	// NoAsk says the agent cannot put a question to its person before an
	// edit. An "ask" then refuses the first attempt, telling the agent to ask,
	// and lets the retry through, the way "bump" does.
	NoAsk bool `json:"no_ask,omitempty"`
	// LateContext says the agent ignores context given before a tool runs;
	// the board then holds a pre_edit's warnings until the next event that
	// can carry them, normally the edit's own post_edit.
	LateContext bool `json:"late_context,omitempty"`
}

// HookResult is the server's answer to a hook event.
type HookResult struct {
	Decision  Decision   `json:"decision"`
	Reason    string     `json:"reason,omitempty"`
	Context   string     `json:"context,omitempty"`
	ClaimID   string     `json:"claim_id,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

// Conflict is one other claim that matters to a path.
type Conflict struct {
	Path     string   `json:"path"`
	Severity Severity `json:"severity"`
	ClaimID  string   `json:"claim_id"`
	Member   string   `json:"member"`
	Branch   string   `json:"branch,omitempty"`
	Task     string   `json:"task,omitempty"`
	Why      string   `json:"why"`
	Pattern  string   `json:"pattern,omitempty"`
	// Area is the area of the path written, which a nearby conflict is about.
	Area      string    `json:"area,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	Active    bool      `json:"active"`
	SameClaim bool      `json:"same_claim,omitempty"`
}

// ActivityKind names what an activity records.
type ActivityKind string

// Activity kinds, as the feed, the dashboard and webhooks name them.
const (
	ActivityClaimOpened         ActivityKind = "claim.opened"
	ActivityClaimReleased       ActivityKind = "claim.released"
	ActivityClaimForgotten      ActivityKind = "claim.forgotten"
	ActivitySessionStarted      ActivityKind = "session.started"
	ActivitySessionEnded        ActivityKind = "session.ended"
	ActivitySessionRecovered    ActivityKind = "session.recovered"
	ActivitySessionStalled      ActivityKind = "session.stalled"
	ActivitySessionGone         ActivityKind = "session.gone"
	ActivityFileChanged         ActivityKind = "file.changed"
	ActivityFootprintReconciled ActivityKind = "footprint.reconciled"
	ActivityConflict            ActivityKind = "conflict"
	ActivityIntentDeclared      ActivityKind = "intent.declared"
	ActivityIntentReleased      ActivityKind = "intent.released"
	ActivityNoteSent            ActivityKind = "note.sent"
)

var activityKinds = []ActivityKind{
	ActivityClaimOpened, ActivityClaimReleased, ActivityClaimForgotten, ActivitySessionStarted, ActivitySessionEnded, ActivitySessionRecovered,
	ActivitySessionStalled, ActivitySessionGone, ActivityFileChanged, ActivityFootprintReconciled,
	ActivityConflict, ActivityIntentDeclared, ActivityIntentReleased, ActivityNoteSent,
}

// ParseActivityKind validates an activity kind's name.
func ParseActivityKind(s string) (ActivityKind, error) {
	if k := ActivityKind(s); slices.Contains(activityKinds, k) {
		return k, nil
	}
	names := make([]string, len(activityKinds))
	for i, k := range activityKinds {
		names[i] = string(k)
	}
	return "", fmt.Errorf("unknown activity kind %q: want one of %s", s, strings.Join(names, ", "))
}

// Activity is a record of something that happened, for the dashboard feed.
type Activity struct {
	Seq      uint64       `json:"seq"`
	At       time.Time    `json:"at"`
	Kind     ActivityKind `json:"kind"`
	Repo     string       `json:"repo"`
	Member   string       `json:"member,omitempty"`
	ClaimID  string       `json:"claim_id,omitempty"`
	Session  string       `json:"session,omitempty"`
	Agent    Agent        `json:"agent,omitempty"`
	Paths    []string     `json:"paths,omitempty"`
	Text     string       `json:"text,omitempty"`
	Severity Severity     `json:"severity,omitempty"`
	Decision Decision     `json:"decision,omitempty"`
	// Breach marks a change made without a check (through the shell) inside a
	// teammate's reservation, which the policy would have stopped.
	Breach bool `json:"breach,omitempty"`
}
