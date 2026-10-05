package board

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
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
//
// Some of what agents send again matters before they do: a reservation
// blocks teammates only while its agent is live, and an agent deep in a long
// tool call sends nothing until it ends; an agent that ended sends nothing
// more at all. So the durable part also holds the live sessions of the claims
// that hold exclusive intents, with what their liveness is read from, and
// which sessions ended and which claims were taken off the board since the
// board was last saved whole, which a restore then applies to the older
// snapshot.

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
	// Sessions are the live sessions of the claims that hold exclusive
	// intents, those that report from them and those that keep them live
	// from elsewhere (Also), in order of key, each with what liveness reads
	// (durableSession). Claims holds the identity of each claim they report
	// from.
	Sessions []*session `json:"sessions,omitempty"`
	// Ended are the keys of the sessions that ended, and Removed the IDs of
	// the claims taken off the board, since the board was last saved whole
	// (Board.SnapshotSaved), each in order.
	Ended   []string `json:"ended,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// Durable is a copy of a board's durable part, taken under its lock.
type Durable struct{ s durableState }

// Durable copies the board's durable part, which the copy then writes
// (WriteTo) without the lock. The copy shares the claims' intents, which are
// replaced rather than changed, and the sessions' Heard, which is too.
func (b *Board) Durable(now time.Time) *Durable {
	b.mu.Lock()
	s := durableState{Format: durableFormat, Saved: now, Version: b.version, Seq: b.seq, Recent: slices.Clone(b.recent),
		Stats: b.statsCopy(), Dropped: b.droppedCopy(), Ended: slices.Collect(maps.Keys(b.ended)),
		Removed: slices.Collect(maps.Keys(b.removed))}
	kept := map[string]bool{}  // the claims s holds
	from := map[string]bool{}  // the claims its sessions report from
	saved := map[string]bool{} // the sessions it holds
	for _, c := range b.claims {
		if d := c.durableClaim(); d != nil {
			s.Claims = append(s.Claims, d)
			kept[c.ID] = true
		}
		if !holdsReservation(c) {
			continue
		}
		for x := range b.sessionsOf(c.ID) {
			if !saved[x.Key] && b.state(now, x).Live() {
				s.Sessions = append(s.Sessions, x.durableSession())
				saved[x.Key], from[x.ClaimID] = true, true
			}
		}
	}
	for id := range from {
		if c := b.claims[id]; c != nil && !kept[id] {
			s.Claims = append(s.Claims, c.identity())
		}
	}
	if len(b.mail) > 0 {
		s.Mail = b.mailCopy()
	}
	b.mu.Unlock()
	slices.SortFunc(s.Claims, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
	slices.SortFunc(s.Sessions, func(x, y *session) int { return strings.Compare(x.Key, y.Key) })
	slices.Sort(s.Ended)
	slices.Sort(s.Removed)
	return &Durable{s: s}
}

// holdsReservation reports whether claim c holds an exclusive intent.
func holdsReservation(c *claim) bool {
	return slices.ContainsFunc(c.Intents, func(in Intent) bool { return in.Mode == ModeExclusive })
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
	d := c.identity()
	d.Intents, d.Inbox = c.Intents, notes
	return d
}

// identity is claim c's identity alone.
func (c *claim) identity() *claim {
	return &claim{ID: c.ID, Repo: c.Repo, Member: c.Member, Host: c.Host, Worktree: c.Worktree, Branch: c.Branch,
		CreatedAt: c.CreatedAt}
}

// durableSession copies of session s what the durable part keeps: what its
// liveness is read from (its claim, the claims it keeps live, its phase, its
// tools and when it was last heard from), and what keeps its agent from
// being told again what it heard of inboxes or that a prompt named its
// claim's task. Not what it was told (Acked), which only keeps it from being
// told twice, and may be large.
func (s *session) durableSession() *session {
	return &session{Key: s.Key, ID: s.ID, Member: s.Member, Agent: s.Agent, ClaimID: s.ClaimID, StartedAt: s.StartedAt,
		LastSeen: s.LastSeen, Phase: s.Phase, Tool: s.Tool, ToolSince: s.ToolSince, Calls: maps.Clone(s.Calls),
		InFlight: s.InFlight, Prompted: s.Prompted, Heard: s.Heard, Reported: s.Reported, Unsure: s.Unsure,
		Also: maps.Clone(s.Also)}
}

// Version is the board's version the copy holds.
func (d *Durable) Version() uint64 { return d.s.Version }

// Kept is a digest of what the copy holds that agents cannot send again,
// or not soon enough: the claims' intents and notes, the mail, the sessions
// that ended and the claims taken off the board, and of the sessions that
// keep reservations live, their claims, phases and tool calls. It changes
// with them, and not with the feed, the stats or the seq, nor when a
// session was last heard from, which every hook moves on.
func (d *Durable) Kept() uint64 {
	h := fnv.New64a()
	enc := json.NewEncoder(h)
	// A hash takes every write.
	_ = enc.Encode(d.s.Claims)
	_ = enc.Encode(d.s.Mail)
	_ = enc.Encode(d.s.Ended)
	_ = enc.Encode(d.s.Removed)
	for _, x := range d.s.Sessions {
		_ = enc.Encode([]any{x.Key, x.ClaimID, x.Phase, inTool(x), x.ToolSince, slices.Sorted(maps.Keys(x.Also))})
	}
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

// ErrStaleDurable reports a durable part found beside the snapshot of an
// older server, which records no version of the board, saved after it: the
// durable part is left from before the older server ran, and is not applied.
var ErrStaleDurable = errors.New("the durable part was saved before the snapshot of an older server")

// RestoreDurable applies a durable part read from r, as Durable.WriteTo
// wrote it or compressed with gzip, to a board restored from a snapshot at
// now, if it holds a later version of the board than the snapshot did: the
// claims' intents and notes, a claim made since with them, the sessions that
// keep reservations live, the sessions that ended and the claims taken off
// the board since the snapshot, the mail, the feed and the stats. The
// sessions' silence is credited with the downtime as Restore credits the
// snapshot's, from when the durable part was saved or the server stopped,
// if that is later (RestoreAfter). It reports whether it applied it. A
// durable part that is damaged is reported as ErrCorruptSnapshot, one of a
// newer format as an error that is not, and one older than the versionless
// snapshot of an older server as ErrStaleDurable.
func (b *Board) RestoreDurable(r io.Reader, now, stopped time.Time) (bool, error) {
	d, err := readDurable(r)
	if err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case d.Version <= b.version:
		return false, nil // the snapshot was saved after it
	case b.restored != nil && !b.restored.versioned && !d.Saved.After(b.restored.saved):
		// An older server's versions are not this one's: the snapshot holds
		// none, and board versions count again from 0 after it.
		return false, ErrStaleDurable
	}
	b.applyDurable(d, now, stopped)
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
// those d holds of claims made since the snapshot; takes the claims off the
// board, and ends the sessions, that d says were since; puts the sessions d
// holds in place of the snapshot's, crediting their silence with the time
// from d's save or stopped, if later, to now; and takes d's mail, feed and
// stats. A claim made since that d holds only notes of goes, as nothing
// would keep it on the board until its agent reports again: its notes wait
// in its member's mailbox for their next session in its repository.
func (b *Board) applyDurable(d durableState, now, stopped time.Time) {
	held := make(map[string]*claim, len(d.Claims))
	for _, k := range d.Claims {
		if k != nil && k.ID != "" && k.Repo != "" {
			held[k.ID] = k
		}
	}
	var sessions []*session
	from := map[string]bool{} // the claims d's sessions report from, or keep live
	for _, x := range d.Sessions {
		if x != nil && x.Key != "" && x.ClaimID != "" {
			sessions = append(sessions, x)
			from[x.ClaimID] = true
			for id := range x.Also {
				from[id] = true
			}
		}
	}
	for _, id := range sortedKeys(b.claims) {
		c := b.claims[id]
		b.setDurable(c, held[id]) // none held: none then
		delete(held, id)
	}
	var mailed []*claim
	for _, id := range sortedKeys(held) {
		k := held[id]
		if old := b.findClaim(k.Member, Where{Repo: k.Repo, Host: k.Host, Worktree: k.Worktree}); old != nil {
			b.removeClaim(old) // the claim made since took its place
			b.unpruned[old.Repo] = true
		}
		c := &claim{ID: k.ID, Repo: k.Repo, Member: k.Member, Host: k.Host, Worktree: k.Worktree, Branch: k.Branch,
			CreatedAt: k.CreatedAt, UpdatedAt: k.CreatedAt}
		b.setDurable(c, k)
		if len(c.Intents) == 0 && !from[c.ID] {
			mailed = append(mailed, c)
			continue
		}
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
	b.ended, b.removed = map[string]uint64{}, map[string]uint64{}
	for _, id := range d.Removed {
		if c := b.claims[id]; c != nil {
			b.removeClaim(c)
			b.unpruned[c.Repo] = true
		}
		b.removed[id] = d.Version // until a snapshot holds it
	}
	for _, k := range d.Ended {
		if x := b.sessions[k]; x != nil {
			x.Phase, x.Reported = phaseEnded, StateEnded
			clearTools(x)
		}
		b.ended[k] = d.Version
	}
	since := laterOf(d.Saved, stopped)
	creditDowntime(sessions, now, since)
	for _, x := range sessions {
		if old := b.sessions[x.Key]; old != nil {
			x.Acked, x.Pending = old.Acked, old.Pending // which d does not hold
		}
		b.attachSession(x, x.ClaimID)
	}
	b.doubt(now, sessions)
	b.restoreFeed(d.Seq, d.Recent, d.Dropped, d.Stats)
	b.restoreMail(d.Mail)
	for _, c := range mailed {
		b.mailNotes(c)
	}
	if len(mailed) > 0 {
		b.fitAllMail()
	}
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

// doubt marks unsure (session.Unsure) the working sessions the snapshot
// holds that durable part d does not: what they did after the snapshot was
// not saved, and may have been to go into a long tool call, which would be
// taken for silence. The sessions of the claims that hold reservations are
// as d has them (sessions), or were not live when it was saved.
func (b *Board) doubt(now time.Time, sessions []*session) {
	durable := make(map[string]bool, len(sessions))
	for _, x := range sessions {
		durable[x.Key] = true
	}
	reserved := func(id string) bool {
		c := b.claims[id]
		return c != nil && holdsReservation(c)
	}
	for _, x := range b.sessions {
		if durable[x.Key] || b.state(now, x) != StateWorking || reserved(x.ClaimID) {
			continue
		}
		carries := false
		for id := range x.Also {
			carries = carries || reserved(id)
		}
		if !carries {
			x.Unsure = true
		}
	}
}

// noteEnded notes, for the durable part, that session s ended, if it did
// (end), or that it reports again, if it ended since the board was last
// saved whole.
func (b *Board) noteEnded(s *session, end bool) {
	switch {
	case end:
		b.changed() // after any snapshot without it (SnapshotSaved)
		b.ended[s.Key] = b.version
		boundNoted(b.ended)
	case s.Phase != phaseEnded:
		delete(b.ended, s.Key)
	}
}

// noteRemoved notes, for the durable part, that claim c was taken off the
// board.
func (b *Board) noteRemoved(c *claim) {
	b.changed()
	b.removed[c.ID] = b.version
	boundNoted(b.removed)
}

// maxNoted bounds the sessions that ended, and the claims taken off the
// board, the durable part says were since the board was last saved whole.
// The acceptance churn ends about 1,500 sessions in the 5 minutes between
// whole saves; this many are held only while those saves fail, and past it
// a crash brings back the earliest.
const maxNoted = 20_000

// boundNoted lets go of the earliest noted past maxNoted, to nine tenths of
// it, ties going by key.
func boundNoted(noted map[string]uint64) {
	if len(noted) <= maxNoted {
		return
	}
	keys := slices.SortedFunc(maps.Keys(noted), func(x, y string) int {
		if c := cmp.Compare(noted[x], noted[y]); c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	for _, k := range keys[:len(keys)-maxNoted*9/10] {
		delete(noted, k)
	}
}

// SnapshotSaved tells the board that a snapshot holding its version was
// saved whole (WriteSnapshot): the sessions that ended and the claims taken
// off it by then are in it, and its durable part need not say so any more.
func (b *Board) SnapshotSaved(version uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	saved := func(_ string, v uint64) bool { return v <= version }
	maps.DeleteFunc(b.ended, saved)
	maps.DeleteFunc(b.removed, saved)
}
