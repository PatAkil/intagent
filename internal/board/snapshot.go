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
}

// Snapshot serialises the board for persistence. The state is copied under
// the lock and encoded outside it, so a large board does not hold up hooks
// while it is written.
func (b *Board) Snapshot(now time.Time) ([]byte, uint64, error) {
	b.mu.Lock()
	s := snapshot{Format: snapshotFormat, Saved: now, Seq: b.seq, Recent: slices.Clone(b.recent), Stats: map[string]*Stats{}}
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

// Restore replaces the board's contents with a snapshot.
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
	for _, c := range s.Claims {
		if c == nil || c.ID == "" {
			continue
		}
		if c.Footprint == nil {
			c.Footprint = map[string]*touch{}
		}
		b.claims[c.ID] = c
		b.byKey[c.key()] = c.ID
	}
	for _, x := range s.Sessions {
		if x == nil || x.Key == "" {
			continue
		}
		b.sessions[x.Key] = x
	}
	b.seq = s.Seq
	b.recent = s.Recent
	if over := len(b.recent) - b.cfg.KeepActivities; over > 0 {
		b.recent = b.recent[over:] // the feed is kept shorter now
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
