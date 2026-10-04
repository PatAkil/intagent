package board

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
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
// while it is written.
func (b *Board) Snapshot(now time.Time) ([]byte, uint64, error) {
	b.mu.Lock()
	s := snapshot{Format: snapshotFormat, Saved: now, Seq: b.seq, Recent: slices.Clone(b.recent), Stats: map[string]*Stats{},
		Dropped: &droppedMarks{Repos: maps.Clone(b.dropped), All: b.droppedAll, Floor: b.droppedFloor}}
	for repo, st := range b.stats {
		c := *st
		s.Stats[repo] = &c
	}
	for _, c := range b.claims {
		s.Claims = append(s.Claims, c.clone())
	}
	for _, x := range b.sessions {
		s.Sessions = append(s.Sessions, x.clone())
	}
	version := b.version
	b.mu.Unlock()
	data, err := json.Marshal(s)
	return data, version, err
}

// clone copies a claim deeply enough to be read while the original changes.
func (c *claim) clone() *claim {
	d := *c
	d.Intents = slices.Clone(c.Intents)
	d.Footprint = make(map[string]*touch, len(c.Footprint))
	for k, t := range c.Footprint {
		tt := *t
		d.Footprint[k] = &tt
	}
	d.Inbox = make([]InboxItem, len(c.Inbox))
	for i, it := range c.Inbox {
		it.Paths = slices.Clone(it.Paths)
		it.DeliveredTo = maps.Clone(it.DeliveredTo)
		d.Inbox[i] = it
	}
	d.Alerted = maps.Clone(c.Alerted)
	// The copy is not on the board: it has no indexes.
	d.areaAt, d.sortedPaths, d.removed = nil, nil, false
	return &d
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
// is derived from them.
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
