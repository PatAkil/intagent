package board

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// What an agent reads when it asks about its teammates' work (the MCP
// team_board tool) is bounded like the greeting, not the whole board: a busy
// repository's full text runs to hundreds of kilobytes, more than an agent's
// tool output may hold and a large bite of its context.
const (
	// maxAgentRows bounds the claims AgentText lists, whatever the caller asks.
	maxAgentRows = 100
	// maxAgentText bounds AgentText's answer in bytes.
	maxAgentText = 16 << 10
	// rowIntents and rowFiles bound what one listed claim shows.
	rowIntents = 3
	rowFiles   = 4
)

// AgentText describes other agents' work in a repository for the agent of
// member working in w: claims that share a file or an area with the caller's
// come first, then running ones, then the most recently updated, at most limit
// of them and maxAgentText bytes in all. Like the greeting, it presents
// teammates' reports as information.
func (b *Board) AgentText(now time.Time, w Where, member string, limit int) string {
	limit = min(max(limit, 1), maxAgentRows)
	repo := RepoID(w.Repo)
	b.mu.Lock()
	defer b.mu.Unlock()
	var self *claim
	if cw, err := cleanWhere(w); err == nil {
		self = b.findClaim(member, cw)
	}
	live := b.liveClaims(now)
	type row struct {
		c       *claim
		near    bool
		running bool
	}
	var rows []row
	running := 0
	near := nearTo(self)
	for _, o := range b.claimsInRepo(repo) {
		if self != nil && o.ID == self.ID {
			continue
		}
		rows = append(rows, row{c: o, near: near(o), running: live[o.ID]})
		if live[o.ID] {
			running++
		}
	}
	if len(rows) == 0 {
		return fmt.Sprintf("No other agents have work in %s right now.", repo)
	}
	sort.Slice(rows, func(i, j int) bool {
		x, y := rows[i], rows[j]
		if x.near != y.near {
			return x.near
		}
		if x.running != y.running {
			return x.running
		}
		if !x.c.UpdatedAt.Equal(y.c.UpdatedAt) {
			return x.c.UpdatedAt.After(y.c.UpdatedAt)
		}
		return x.c.ID < y.c.ID
	})

	head := fmt.Sprintf("Other agents' work in %s: %s, %d running %s.", repo, plural(len(rows), "claim"), running, dataNotice)
	if self != nil {
		head += " Work that shares files or areas with yours comes first."
	}
	lines := []string{head}
	size := len(head)
	agents := b.agentsByClaim(now)
	shown, shownRunning := 0, 0
	for _, r := range rows {
		if shown == limit {
			break
		}
		line := rowText(now, r.c, r.running, agents[r.c.ID])
		// Keep room for the closing line.
		if size+1+len(line) > maxAgentText-200 {
			break
		}
		lines = append(lines, line)
		size += 1 + len(line)
		shown++
		if r.running {
			shownRunning++
		}
	}
	if more := len(rows) - shown; more > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more (%d running); use the intagent check_paths tool for the files you plan to change.",
			more, running-shownRunning))
	}
	return strings.Join(lines, "\n")
}

// nearTo returns whether a claim shares a changed file, or the area of one,
// with self. With no self, nothing is near.
func nearTo(self *claim) func(*claim) bool {
	if self == nil || len(self.Footprint) == 0 {
		return func(*claim) bool { return false }
	}
	areas := areasOf(self)
	return func(o *claim) bool {
		for p, t := range o.Footprint {
			if _, ok := self.Footprint[p]; ok || (t.Area != "" && areas[t.Area]) {
				return true
			}
		}
		return false
	}
}

// agentsByClaim describes each claim's sessions that have not ended or gone,
// as "claude-code working", with repeats counted: "2 codex waiting".
func (b *Board) agentsByClaim(now time.Time) map[string]string {
	counts := map[string]map[string]int{}
	for _, s := range b.sessions {
		st := b.state(now, s)
		if st == StateEnded || st == StateGone {
			continue
		}
		if counts[s.ClaimID] == nil {
			counts[s.ClaimID] = map[string]int{}
		}
		counts[s.ClaimID][string(s.Agent)+" "+string(st)]++
	}
	out := make(map[string]string, len(counts))
	for id, m := range counts {
		parts := make([]string, 0, len(m))
		for k, n := range m {
			if n > 1 {
				k = fmt.Sprintf("%d %s", n, k)
			}
			parts = append(parts, k)
		}
		sort.Strings(parts)
		out[id] = strings.Join(parts, ", ")
	}
	return out
}

// rowText is one claim in AgentText: who, where, the claim to address a note
// to, its agents, task, first intents and newest files.
func rowText(now time.Time, o *claim, running bool, agents string) string {
	var sb strings.Builder
	sb.WriteString("- " + o.Member)
	if o.Branch != "" {
		sb.WriteString(" on " + o.Branch)
	}
	sb.WriteString(" (claim " + o.ID)
	if agents != "" {
		sb.WriteString("; " + agents)
	}
	sb.WriteString(")")
	if !running {
		sb.WriteString(", not running, last seen " + since(now, o.UpdatedAt))
	}
	if o.Task != "" {
		sb.WriteString(": " + quote(o.Task))
	}
	for i, in := range o.Intents {
		if i == rowIntents {
			fmt.Fprintf(&sb, "; and %s", plural(len(o.Intents)-rowIntents, "more intent"))
			break
		}
		fmt.Fprintf(&sb, "; %s intent %s", in.Mode, in.Pattern)
	}
	if n := len(o.Footprint); n > 0 {
		files := newestFiles(o, rowFiles)
		fmt.Fprintf(&sb, "; changed %s: %s", plural(n, "file"), strings.Join(files, ", "))
		if n > len(files) {
			fmt.Fprintf(&sb, " and %d more", n-len(files))
		}
	}
	return sb.String()
}
