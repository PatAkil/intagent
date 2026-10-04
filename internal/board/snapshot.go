package board

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/patakil/intagent/internal/glob"
)

const snapshotFormat = 1

type snapshot struct {
	Format   int               `json:"format"`
	Saved    time.Time         `json:"saved"`
	Seq      uint64            `json:"seq"`
	Claims   []*claim          `json:"claims"`
	Sessions []*session        `json:"sessions"`
	Recent   []Activity        `json:"recent"`
	Stats    map[string]*Stats `json:"stats,omitempty"`
	// Dropped is nil in a snapshot from a server that did not keep it.
	Dropped *droppedMarks `json:"dropped,omitempty"`
}

// droppedMarks are what the board's feed has let go of, as Board keeps them,
// so a replay after a restart can still tell a dashboard it missed nothing.
type droppedMarks struct {
	Repos map[string]uint64 `json:"repos,omitempty"`
	All   uint64            `json:"all,omitempty"`
	Floor uint64            `json:"floor,omitempty"`
}

// Snapshot serialises the board for persistence. The state is copied under
// the lock and encoded outside it, so a large board does not hold up hooks
// while it is written. The copy shares each claim's footprint with the
// board, so the time the lock is held grows with the claims and sessions,
// not with the files they changed.
func (b *Board) Snapshot(now time.Time) ([]byte, uint64, error) {
	s, version := b.snapshotCopy(now)
	data, err := json.Marshal(s)
	return data, version, err
}

// snapshotCopy copies what a snapshot holds, under the lock.
func (b *Board) snapshotCopy(now time.Time) (snapshot, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := snapshot{Format: snapshotFormat, Saved: now, Seq: b.seq, Recent: slices.Clone(b.recent), Stats: map[string]*Stats{},
		Dropped: &droppedMarks{Repos: maps.Clone(b.dropped), All: b.droppedAll, Floor: b.droppedFloor}}
	for repo, st := range b.stats {
		c := *st
		s.Stats[repo] = &c
	}
	s.Claims = make([]*claim, 0, len(b.claims))
	for _, c := range b.claims {
		s.Claims = append(s.Claims, c.shareFootprint())
	}
	s.Sessions = make([]*session, 0, len(b.sessions))
	for _, x := range b.sessions {
		s.Sessions = append(s.Sessions, x.clone())
	}
	return s, b.version
}

// shareFootprint copies a claim for a snapshot, deeply enough to be read
// while the original changes, except for what the claim no longer changes
// in place: its footprint and the touches in it (putTouch), the alerts it
// heard (alert) and its inbox items' paths. The copy reads the claim's own.
// Both are marked shared, so a copy put on a board copies them before it
// changes them too.
func (c *claim) shareFootprint() *claim {
	c.fpShared, c.alertedShared = true, true
	d := *c
	d.Intents = slices.Clone(c.Intents)
	d.Inbox = slices.Clone(c.Inbox)
	for i := range d.Inbox {
		d.Inbox[i].DeliveredTo = maps.Clone(d.Inbox[i].DeliveredTo) // deliverInbox adds to it
	}
	// The copy is not on the board: it has no indexes.
	d.areaAt, d.sortedPaths, d.removed = nil, nil, false
	return &d
}

// alert notes that the claim heard the alert k, and reports whether it had
// not already. When a snapshot shares the claim's alerts, it changes a copy,
// which the claim keeps.
func (c *claim) alert(k string) bool {
	if c.Alerted[k] {
		return false
	}
	switch {
	case c.Alerted == nil:
		c.Alerted = map[string]bool{}
	case c.alertedShared:
		c.Alerted = maps.Clone(c.Alerted)
	}
	c.alertedShared = false
	c.Alerted[k] = true
	return true
}

// clone copies a session deeply enough to be read while the original changes.
func (s *session) clone() *session {
	d := *s
	d.Acked = maps.Clone(s.Acked)
	d.Calls = maps.Clone(s.Calls)
	d.refused = slices.Clone(s.refused)
	return &d
}

// Restore replaces the board's contents with a snapshot, and rebuilds what
// is derived from them. It drops the intents whose patterns the board no
// longer accepts (glob.CleanPattern), and logs each.
func (b *Board) Restore(data []byte) error {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	if s.Format != snapshotFormat {
		return fmt.Errorf("snapshot format %d is not supported (want %d)", s.Format, snapshotFormat)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claims = map[string]*claim{}
	b.byKey = map[string]string{}
	b.sessions = map[string]*session{}
	b.byRepo = map[string]*repoIndex{}
	b.claimSessions = map[string]map[string]*session{}
	for _, c := range s.Claims {
		if c == nil || c.ID == "" {
			continue
		}
		c.Intents = b.validIntents(c)
		b.addClaim(c)
	}
	for _, x := range s.Sessions {
		if x == nil || x.Key == "" {
			continue
		}
		b.attachSession(x, x.ClaimID)
	}
	b.seq = s.Seq
	b.recent = s.Recent
	b.dropped = map[string]uint64{}
	if d := s.Dropped; d != nil {
		maps.Copy(b.dropped, d.Repos)
		b.droppedAll, b.droppedFloor = d.All, d.Floor
	} else {
		// What the feed let go of before this snapshot is not known by
		// repository: every activity before the feed's first, in any.
		b.droppedFloor = b.seq
		if len(b.recent) > 0 {
			b.droppedFloor = b.recent[0].Seq - 1
		}
		b.droppedAll = b.droppedFloor
	}
	if over := len(b.recent) - b.cfg.KeepActivities; over > 0 {
		b.letGo(b.recent[:over]) // the feed is kept shorter now
		b.recent = b.recent[over:]
	}
	b.stats = s.Stats
	if b.stats == nil {
		b.stats = map[string]*Stats{}
	}
	// What happened before the snapshot is in it, or gone with the process.
	b.notes = map[string][]time.Time{}
	b.pending = nil
	return nil
}

// validIntents is c's intents whose patterns the board accepts, cleaned. A
// snapshot from an older server can hold patterns it no longer accepts, such
// as ones too costly to match.
func (b *Board) validIntents(c *claim) []Intent {
	keep := c.Intents[:0]
	for _, in := range c.Intents {
		p, err := glob.CleanPattern(in.Pattern)
		if err != nil {
			b.log.Warn("dropping an intent from the snapshot", "claim", c.ID, "member", c.Member, "err", err)
			continue
		}
		if slices.ContainsFunc(keep, func(k Intent) bool { return k.Pattern == p }) {
			continue
		}
		in.Pattern = p
		keep = append(keep, in)
	}
	return keep
}
