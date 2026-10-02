package board

import (
	"sort"
	"time"
)

// SessionView is a session as the dashboard and CLI show it.
type SessionView struct {
	ID        string    `json:"id"`
	Agent     Agent     `json:"agent"`
	State     State     `json:"state"`
	Tool      string    `json:"tool,omitempty"`
	ToolSince time.Time `json:"tool_since,omitzero"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// FileView is one changed file.
type FileView struct {
	Path    string    `json:"path"`
	Area    string    `json:"area,omitempty"`
	At      time.Time `json:"at"`
	FromGit bool      `json:"from_git,omitempty"`
}

// ClaimView is a claim as the dashboard and CLI show it.
type ClaimView struct {
	ID        string        `json:"id"`
	Member    string        `json:"member"`
	Host      string        `json:"host"`
	Worktree  string        `json:"worktree"`
	Branch    string        `json:"branch,omitempty"`
	Task      string        `json:"task,omitempty"`
	Active    bool          `json:"active"`
	Intents   []Intent      `json:"intents"`
	Files     []FileView    `json:"files"`
	Truncated bool          `json:"truncated,omitempty"`
	Sessions  []SessionView `json:"sessions"`
	Pending   int           `json:"pending_inbox"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// View is everything known about one repository.
type View struct {
	Repo     string      `json:"repo"`
	At       time.Time   `json:"at"`
	Claims   []ClaimView `json:"claims"`
	Recent   []Activity  `json:"recent"`
	Stats    Stats       `json:"stats"`
	Policy   Policy      `json:"policy"`
	LastSeq  uint64      `json:"last_seq"`
	Members  []string    `json:"members"`
	Sessions int         `json:"live_sessions"`
}

// RepoSummary is one line of the repository list.
type RepoSummary struct {
	Repo         string    `json:"repo"`
	Claims       int       `json:"claims"`
	ActiveClaims int       `json:"active_claims"`
	LiveSessions int       `json:"live_sessions"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// View returns a repository's claims, most recently active first.
func (b *Board) View(now time.Time, repo string) View {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	for _, c := range b.claimsInRepo(repo) {
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
		for _, p := range sortedFiles(c) {
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

// Repos lists every repository with claims, most recently active first.
func (b *Board) Repos(now time.Time) []RepoSummary {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := b.liveClaims(now)
	byRepo := map[string]*RepoSummary{}
	for _, c := range b.claims {
		r := byRepo[c.Repo]
		if r == nil {
			r = &RepoSummary{Repo: c.Repo}
			byRepo[c.Repo] = r
		}
		r.Claims++
		if live[c.ID] {
			r.ActiveClaims++
		}
		if c.UpdatedAt.After(r.UpdatedAt) {
			r.UpdatedAt = c.UpdatedAt
		}
	}
	for _, s := range b.sessions {
		if c := b.claims[s.ClaimID]; c != nil && b.state(now, s).Live() {
			byRepo[c.Repo].LiveSessions++
		}
	}
	out := make([]RepoSummary, 0, len(byRepo))
	for _, r := range byRepo {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Since returns activities after seq for a repository ("" for all).
func (b *Board) Since(repo string, seq uint64) []Activity {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Activity
	for _, a := range b.recent {
		if a.Seq > seq && (repo == "" || a.Repo == repo) {
			out = append(out, a)
		}
	}
	return out
}

// Text renders a view for a person or an agent reading a terminal.
func (v View) Text() string { return renderView(v) }
