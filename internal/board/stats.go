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
}

func (b *Board) statsOf(repo string, now time.Time) *Stats {
	s := b.stats[repo]
	if s == nil {
		s = &Stats{Since: now}
		b.stats[repo] = s
	}
	return s
}

func (b *Board) statsFor(repo string) Stats {
	if s := b.stats[repo]; s != nil {
		return *s
	}
	return Stats{}
}
