package board

import "time"

// Stats counts what intagent noticed and did in one repository. They are the
// numbers a team needs to judge whether coordination is paying for itself.
type Stats struct {
	Since time.Time `json:"since"`
	// Checks is the number of edits checked before they happened.
	Checks int `json:"checks"`
	// Blocks, Overlaps and Nearby count checks by their most severe finding.
	Blocks   int `json:"blocks"`
	Overlaps int `json:"overlaps"`
	Nearby   int `json:"nearby"`
	// Refused, Bumped, Asked and Warned count checks by what the agent was told.
	Refused int `json:"refused"`
	Bumped  int `json:"bumped"`
	Asked   int `json:"asked"`
	Warned  int `json:"warned"`
	// Alerts counts claims told that someone else touched their files.
	Alerts int `json:"alerts"`
	// Notes counts notes delivered; Intents counts intents declared.
	Notes   int `json:"notes"`
	Intents int `json:"intents"`
	// Unheard counts edits the board did not check because their agent had
	// stopped waiting for the answer, and gone ahead, before it got to them.
	Unheard int `json:"unheard,omitempty"`
	// Unstored counts hooks from new sessions the board had no room for
	// (Config.MaxSessions): their edits were checked, but nothing of them was
	// kept for their teammates to hear.
	Unstored int `json:"unstored,omitempty"`
	// Partial counts hooks, declarations and notes the board stopped
	// comparing with teammates' patterns before it had compared them all, as
	// one call may match only so much: what they did not compare went
	// through unchecked.
	Partial int `json:"partial,omitempty"`
}

// maxStatsRepos bounds the repositories the board keeps stats for, far
// above any team's: a client can name any repository, and each would
// otherwise keep its stats for good, in every snapshot.
const maxStatsRepos = 1024

func (b *Board) statsOf(repo string, now time.Time) *Stats {
	s := b.stats[repo]
	if s == nil {
		if len(b.stats) >= maxStatsRepos {
			b.forgetStats()
		}
		s = &Stats{Since: now}
		b.stats[repo] = s
	}
	b.statsAt[repo] = now
	return s
}

// forgetStats drops the stats of the repository with no claims that was
// counted in longest ago, by when it was last counted (as far as this
// process knows, which after a restart is not at all), then when it was
// first, then by name. Stats of a repository with claims are kept, past
// the bound if they must be: claims are bounded themselves.
func (b *Board) forgetStats() {
	oldest := ""
	before := func(r string) bool {
		if c := b.statsAt[r].Compare(b.statsAt[oldest]); c != 0 {
			return c < 0
		}
		if c := b.stats[r].Since.Compare(b.stats[oldest].Since); c != 0 {
			return c < 0
		}
		return r < oldest
	}
	for r := range b.stats {
		if b.byRepo[r] == nil && (oldest == "" || before(r)) {
			oldest = r
		}
	}
	if oldest != "" {
		delete(b.stats, oldest)
		delete(b.statsAt, oldest)
	}
}

func (b *Board) statsFor(repo string) Stats {
	if s := b.stats[repo]; s != nil {
		return *s
	}
	return Stats{}
}
