package board

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"slices"
	"strings"
	"time"
)

// Most of what a board holds its agents send it again: a worktree's changed
// files at its next scan, a session at its next hook, and what a session was
// told or a claim alerted of only keeps it from being told twice. What they
// cannot send again is the durable part: the intents claims declared, the
// notes waiting in their inboxes and in members' mailboxes, and the
// repositories' stats and the feed. A server saves the durable part soon
// after it changes, and the whole board seldom (internal/server/persist.go),
// and restores the whole board and then the durable part saved after it.

// durableFormat is the format of a durable part this board writes.
const durableFormat = 1

// durableState is a board's durable part, as Durable.WriteTo writes it.
type durableState struct {
	Format int       `json:"format"`
	Saved  time.Time `json:"saved"`
	// Version is the board's version the durable part holds.
	Version uint64 `json:"version"`
	// Claims are the claims with intents or notes, in order of ID, each
	// with its identity, intents and notes alone (durableClaim).
	Claims  []*claim          `json:"claims"`
	Mail    []*mailbox        `json:"mail,omitempty"`
	Seq     uint64            `json:"seq"`
	Recent  []Activity        `json:"recent"`
	Stats   map[string]*Stats `json:"stats,omitempty"`
	Dropped *droppedMarks     `json:"dropped,omitempty"`
}

// Durable is a copy of a board's durable part, taken under its lock.
type Durable struct{ s durableState }

// Durable copies the board's durable part, which the copy then writes
// (WriteTo) without the lock. The copy shares the claims' intents, which are
// replaced rather than changed.
func (b *Board) Durable(now time.Time) *Durable {
	b.mu.Lock()
	s := durableState{Format: durableFormat, Saved: now, Version: b.version, Seq: b.seq, Recent: slices.Clone(b.recent),
		Stats: b.statsCopy(), Dropped: b.droppedCopy()}
	for _, c := range b.claims {
		if d := c.durableClaim(); d != nil {
			s.Claims = append(s.Claims, d)
		}
	}
	if len(b.mail) > 0 {
		s.Mail = b.mailCopy()
	}
	b.mu.Unlock()
	slices.SortFunc(s.Claims, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
	return &Durable{s: s}
}

// durableClaim is what of claim c is durable, or nil if nothing is: its
// identity, its intents and the notes in its inbox, not yet shown, as a
// session's agent is shown them again after a restore that loses what was
// shown.
func (c *claim) durableClaim() *claim {
	var notes []InboxItem
	for _, it := range c.Inbox {
		if it.Kind == "note" {
			it.Shown = false
			notes = append(notes, it)
		}
	}
	if len(c.Intents) == 0 && len(notes) == 0 {
		return nil
	}
	return &claim{ID: c.ID, Repo: c.Repo, Member: c.Member, Host: c.Host, Worktree: c.Worktree, Branch: c.Branch,
		Intents: c.Intents, Inbox: notes, CreatedAt: c.CreatedAt}
}

// Version is the board's version the copy holds.
func (d *Durable) Version() uint64 { return d.s.Version }

// Kept is a digest of what the copy holds that agents cannot send again:
// the claims' intents and notes, and the mail. It changes with them, and not
// with the feed, the stats or the seq, which every hook moves on.
func (d *Durable) Kept() uint64 {
	h := fnv.New64a()
	enc := json.NewEncoder(h)
	_ = enc.Encode(d.s.Claims) // a hash takes every write
	_ = enc.Encode(d.s.Mail)
	return h.Sum64()
}

// WriteTo writes the copy to w as JSON.
func (d *Durable) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	bw := bufio.NewWriterSize(cw, snapshotBuffer)
	if err := json.NewEncoder(bw).Encode(d.s); err != nil {
		return cw.n, err
	}
	err := bw.Flush()
	return cw.n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// RestoreDurable applies a durable part read from r, as Durable.WriteTo
// wrote it or compressed with gzip, to a board restored from a snapshot,
// if it holds a later version of the board than the snapshot did: the
// claims' intents and notes, a claim made since with them, the mail, the
// feed and the stats. It reports whether it applied it. A durable part that
// is damaged is reported as ErrCorruptSnapshot, and one of a newer format
// as an error that is not.
func (b *Board) RestoreDurable(r io.Reader) (bool, error) {
	d, err := readDurable(r)
	if err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d.Version <= b.version {
		return false, nil // the snapshot was saved after it
	}
	b.applyDurable(d)
	return true, nil
}

func readDurable(r io.Reader) (durableState, error) {
	var d durableState
	src := &sourceReader{r: r}
	in, err := unzipped(src)
	if err == nil {
		dec := json.NewDecoder(in)
		if err = dec.Decode(&d); err == nil {
			if _, end := dec.Token(); !errors.Is(end, io.EOF) {
				err = errors.New("data after the durable part")
			}
		}
	}
	switch {
	case err != nil:
	case d.Format > durableFormat:
		err = unsupportedFormat(d.Format)
	case d.Format != durableFormat:
		err = fmt.Errorf("durable part format %d", d.Format)
	}
	if err != nil {
		return d, readError(src, err)
	}
	return d, nil
}

// applyDurable gives the board's claims the intents and notes d holds, and
// those d holds of claims made since the snapshot, and takes d's mail, feed
// and stats.
func (b *Board) applyDurable(d durableState) {
	held := make(map[string]*claim, len(d.Claims))
	for _, k := range d.Claims {
		if k != nil && k.ID != "" && k.Repo != "" {
			held[k.ID] = k
		}
	}
	for _, id := range sortedKeys(b.claims) {
		c := b.claims[id]
		b.setDurable(c, held[id]) // none held: none then
		delete(held, id)
	}
	for _, id := range sortedKeys(held) {
		k := held[id]
		if old := b.findClaim(k.Member, Where{Repo: k.Repo, Host: k.Host, Worktree: k.Worktree}); old != nil {
			b.removeClaim(old) // the claim made since took its place
			b.unpruned[old.Repo] = true
		}
		c := &claim{ID: k.ID, Repo: k.Repo, Member: k.Member, Host: k.Host, Worktree: k.Worktree, Branch: k.Branch,
			CreatedAt: k.CreatedAt, UpdatedAt: k.CreatedAt}
		b.setDurable(c, k)
		for _, in := range c.Intents {
			c.UpdatedAt = laterOf(c.UpdatedAt, in.DeclaredAt)
			if in.Summary != "" {
				c.Task = in.Summary
			}
		}
		for _, it := range c.Inbox {
			c.UpdatedAt = laterOf(c.UpdatedAt, it.At)
		}
		b.addClaim(c)
	}
	b.restoreFeed(d.Seq, d.Recent, d.Dropped, d.Stats)
	b.restoreMail(d.Mail)
	b.version = d.Version
}

// setDurable gives claim c the intents and notes durable claim k holds, or
// none if k is nil, beside the other items of its inbox, in time order.
// A note c holds already keeps whether it was shown.
func (b *Board) setDurable(c, k *claim) {
	var notes []InboxItem
	c.Intents = nil
	if k != nil {
		c.Intents = b.validIntents(k)
		notes = k.Inbox
	}
	shown := map[string]bool{}
	inbox := make([]InboxItem, 0, len(c.Inbox)+len(notes))
	for _, it := range c.Inbox {
		if it.Kind == "note" {
			shown[it.ID] = it.Shown
		} else {
			inbox = append(inbox, it)
		}
	}
	for _, it := range notes {
		if it.Kind == "note" {
			it.Shown = shown[it.ID]
			c.InboxSeq = max(c.InboxSeq, it.Seq)
			inbox = append(inbox, it)
		}
	}
	slices.SortStableFunc(inbox, func(x, y InboxItem) int { return x.At.Compare(y.At) })
	c.Inbox, c.inboxShared = nil, false
	if len(inbox) > 0 {
		c.Inbox = inbox
	}
	c.pruneBreaches()
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
