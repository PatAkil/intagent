package board

import (
	"slices"
	"sort"
	"strings"
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
	ID       string     `json:"id"`
	Member   string     `json:"member"`
	Host     string     `json:"host"`
	Worktree string     `json:"worktree"`
	Branch   string     `json:"branch,omitempty"`
	Task     string     `json:"task,omitempty"`
	Active   bool       `json:"active"`
	Intents  []Intent   `json:"intents"`
	Files    []FileView `json:"files"`
	// FileCount counts the claim's changed files; Files lists at most
	// maxViewFiles of them, the newest, and the contested ones besides.
	FileCount int `json:"file_count"`
	// Truncated says git reported more changed files than the board keeps.
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
	// Epoch names the server process that answered, set by the server. When it
	// changes, the server restarted and event numbers may have started over.
	Epoch string `json:"epoch,omitempty"`
}

// RepoSummary is one line of the repository list.
type RepoSummary struct {
	Repo         string    `json:"repo"`
	Claims       int       `json:"claims"`
	ActiveClaims int       `json:"active_claims"`
	LiveSessions int       `json:"live_sessions"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Epoch is View.Epoch, for readers of the list.
	Epoch string `json:"epoch,omitempty"`
}

// View returns a repository's claims, most recently active first. It copies
// what it shows while it holds the board's lock, and orders and caps it once
// it has let go: on a large repository, ordering every claim's files was most
// of the time hooks waited behind a view.
func (b *Board) View(now time.Time, repo string) View {
	repo = RepoID(repo)
	v, claims := b.viewCopy(now, repo)
	if b.viewCopied != nil {
		b.viewCopied()
	}
	changedBy := map[string]int{} // how many claims changed each file
	for _, c := range claims {
		for _, f := range c.files {
			changedBy[f.path]++
		}
	}
	members := map[string]bool{}
	for _, c := range claims {
		members[c.view.Member] = true
		cv := c.view
		// A session is listed under each claim it keeps live, and counted
		// under the one it reports from, as Repos counts it: a session that
		// moved to another repository is not counted in this one.
		for _, s := range c.sessions {
			if s.here && s.view.State.Live() {
				v.Sessions++
			}
		}
		cv.Sessions = sessionViews(c.sessions)
		// Files are sorted with their touches beside them: a comparison that
		// looked both up in the footprint cost most of a dashboard's view.
		files := c.files
		slices.SortFunc(files, newerFirst)
		cv.FileCount = len(files)
		if len(files) > maxViewFiles {
			// A view goes to every dashboard on every refresh; the newest files
			// are the ones anyone acts on, and contested ones are hot spots.
			files = capFiles(files, changedBy)
		}
		for _, f := range files {
			cv.Files = append(cv.Files, FileView{Path: f.path, Area: f.t.Area, At: f.t.At, FromGit: f.t.FromGit})
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
	return v
}

// claimCopy is what a view shows of a claim, copied under the lock: its
// files with their touches, which are never changed once in a footprint,
// and its sessions with their states, those that moved to another worktree
// since (Also) included.
type claimCopy struct {
	view     ClaimView
	files    []fileAt
	sessions []sessionCopy
}

type sessionCopy struct {
	key  string
	view SessionView
	here bool // the session reports from the claim
}

// viewCopy copies, under the lock, what View shows of a repository: the
// view's own fields, and each claim's.
func (b *Board) viewCopy(now time.Time, repo string) (View, []claimCopy) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := View{Repo: repo, At: now, Policy: b.cfg.Policy, LastSeq: b.seq, Claims: []ClaimView{}, Recent: []Activity{}}
	live := b.liveAt(now)
	var claims []claimCopy
	for c := range b.claimsIn(repo) {
		cc := claimCopy{
			view: ClaimView{
				ID: c.ID, Member: c.Member, Host: c.Host, Worktree: c.Worktree, Branch: c.Branch, Task: c.Task,
				Active: live.claim(c.ID), Intents: append([]Intent{}, c.Intents...), Truncated: c.FootprintTruncated,
				CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Files: []FileView{},
			},
			files:    make([]fileAt, 0, len(c.Footprint)),
			sessions: make([]sessionCopy, 0, len(b.claimSessions[c.ID])+len(b.alsoSessions[c.ID])),
		}
		for p, t := range c.Footprint {
			cc.files = append(cc.files, fileAt{p, t})
		}
		for s := range b.sessionsOf(c.ID) {
			cc.sessions = append(cc.sessions, sessionCopy{key: s.Key, view: SessionView{ID: s.ID, Agent: s.Agent, State: b.state(now, s),
				Tool: s.Tool, ToolSince: s.ToolSince, StartedAt: s.StartedAt, LastSeen: s.LastSeen}, here: s.ClaimID == c.ID})
		}
		for _, it := range c.Inbox {
			if now.Sub(it.At) < inboxTTL && len(it.DeliveredTo) == 0 {
				cc.view.Pending++
			}
		}
		claims = append(claims, cc)
	}
	for _, a := range b.recent {
		if a.Repo == repo {
			v.Recent = append(v.Recent, a)
		}
	}
	v.Stats = b.statsFor(repo)
	return v, claims
}

// Repos lists every repository with claims, most recently active first.
func (b *Board) Repos(now time.Time) []RepoSummary {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]RepoSummary, 0, len(b.byRepo))
	live := b.liveAt(now)
	for repo := range b.byRepo {
		r := RepoSummary{Repo: repo}
		for c := range b.claimsIn(repo) {
			r.Claims++
			// A session counts once, under the claim it reports from; a
			// claim it moved from is active while it is live.
			for _, s := range b.claimSessions[c.ID] {
				if b.state(now, s).Live() {
					r.LiveSessions++
				}
			}
			if live.claim(c.ID) {
				r.ActiveClaims++
			}
			if c.UpdatedAt.After(r.UpdatedAt) {
				r.UpdatedAt = c.UpdatedAt
			}
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(x, y RepoSummary) int {
		if c := y.UpdatedAt.Compare(x.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(x.Repo, y.Repo)
	})
	return out
}

// sessionViews shows a claim's sessions, the most recently seen first, and
// those seen at the same moment by key.
func sessionViews(ss []sessionCopy) []SessionView {
	slices.SortFunc(ss, func(x, y sessionCopy) int {
		if c := y.view.LastSeen.Compare(x.view.LastSeen); c != 0 {
			return c
		}
		return strings.Compare(x.key, y.key)
	})
	out := make([]SessionView, len(ss))
	for i, s := range ss {
		out[i] = s.view
	}
	return out
}

// Since returns activities after seq for a repository ("" for all).
func (b *Board) Since(repo string, seq uint64) []Activity {
	acts, _ := b.Replay(repo, seq)
	return acts
}

// Replay returns activities after seq for a repository ("" for all), and
// whether they are all of them: false when the feed has already let go of an
// activity after seq for that repository, so a client that saw seq last has
// missed something it cannot be sent.
func (b *Board) Replay(repo string, seq uint64) ([]Activity, bool) {
	if repo != "" {
		repo = RepoID(repo)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Activity
	for _, a := range b.recent {
		if a.Seq > seq && (repo == "" || a.Repo == repo) {
			out = append(out, a)
		}
	}
	lost := b.droppedFloor
	if repo == "" {
		lost = max(lost, b.droppedAll)
	} else {
		lost = max(lost, b.dropped[repo])
	}
	return out, lost <= seq
}

// Text renders a view for a person or an agent reading a terminal.
func (v View) Text() string { return renderView(v) }

// capFiles keeps a claim's newest files and, past the cap, up to as many
// again that another claim also changed: the dashboard finds hot spots in the
// files a view lists. It does no more than a map lookup per file.
func capFiles(files []fileAt, changedBy map[string]int) []fileAt {
	out := files[:maxViewFiles:maxViewFiles]
	for _, f := range files[maxViewFiles:] {
		if len(out) == 2*maxViewFiles {
			break
		}
		if changedBy[f.path] > 1 {
			out = append(out, f)
		}
	}
	return out
}

// maxViewFiles bounds the files a view lists per claim, contested files
// aside; FileCount counts them all.
const maxViewFiles = 500
