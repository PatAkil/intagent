package board

import (
	"encoding/json"
	"fmt"
	"time"
)

const snapshotFormat = 1

type snapshot struct {
	Format   int               `json:"format"`
	Saved    time.Time         `json:"saved"`
	Seq      uint64            `json:"seq"`
	Claims   []*Claim          `json:"claims"`
	Sessions []*Session        `json:"sessions"`
	Recent   []Activity        `json:"recent"`
	Stats    map[string]*Stats `json:"stats,omitempty"`
}

// Snapshot serialises the board for persistence.
func (b *Board) Snapshot(now time.Time) ([]byte, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := snapshot{Format: snapshotFormat, Saved: now, Seq: b.seq, Recent: b.recent, Stats: b.stats}
	for _, c := range b.claims {
		s.Claims = append(s.Claims, c)
	}
	for _, x := range b.sessions {
		s.Sessions = append(s.Sessions, x)
	}
	data, err := json.Marshal(s)
	return data, b.version, err
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
	b.claims = map[string]*Claim{}
	b.byKey = map[string]string{}
	b.sessions = map[string]*Session{}
	for _, c := range s.Claims {
		if c == nil || c.ID == "" {
			continue
		}
		if c.Footprint == nil {
			c.Footprint = map[string]*Touch{}
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
	b.stats = s.Stats
	if b.stats == nil {
		b.stats = map[string]*Stats{}
	}
	return nil
}
