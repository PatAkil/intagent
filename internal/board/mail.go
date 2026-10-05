package board

import (
	"slices"
	"strings"
	"time"
)

// A note to a member none of whose worktrees in a repository has an agent
// running waits in a mailbox, which their next session there hears, in
// whichever worktree: a fresh one, which a note queued in a worktree they
// left would never reach, or one of those they left.

// mailbox holds the notes waiting for a member in a repository.
type mailbox struct {
	Repo   string      `json:"repo"`
	Member string      `json:"member"`
	Items  []InboxItem `json:"items"`
}

// Bounds on the notes waiting in mailboxes, as a member can name any
// repository: those one mailbox keeps, those of one sender the board keeps,
// and those it keeps in all. A mailbox left empty goes, so the board keeps
// at most maxMail mailboxes.
const (
	maxMailItems  = 20
	maxSenderMail = 64
	maxMail       = 4096
	mailKeyJoiner = "\n" // in neither a repository ID nor a member's name
)

func mailKey(repo, member string) string { return repo + mailKeyJoiner + member }

// hold puts a note for member in their mailbox for repo, letting go of what
// a full one holds as enqueue lets go of a full inbox's items, and then of
// what the board holds past its bounds (fitMail).
func (b *Board) hold(now time.Time, repo, member string, it InboxItem) {
	k := mailKey(repo, member)
	m := b.mail[k]
	if m == nil {
		m = &mailbox{Repo: repo, Member: member}
		b.mail[k] = m
	}
	it.ID, it.At, it.Text = b.newID("i_"), now, Clean(it.Text, maxNoteLen)
	m.Items = append(m.Items, it)
	for len(m.Items) > maxMailItems {
		i := evictIndex(m.Items)
		m.Items = slices.Delete(m.Items, i, i+1)
	}
	b.fitMail(it.From)
}

// fitMail lets go of the notes waiting past their bounds once sender has
// had one held: the sender's oldest past maxSenderMail of theirs, and past
// maxMail on the board, the oldest of whoever has the most waiting
// (fitAllMail). A sender's notes to people away, in repositories they may
// make up, push out their own, not a note a teammate left, as letting go of
// the mailbox whose newest note was oldest did: one member's notes at the
// rate limit pushed out a teammate's within the hour.
func (b *Board) fitMail(sender string) {
	var mine []heldNote
	waiting := 0
	for _, m := range b.mail {
		waiting += len(m.Items)
		for i := range m.Items {
			if m.Items[i].From == sender {
				mine = append(mine, heldNote{m, i})
			}
		}
	}
	if over := len(mine) - maxSenderMail; over > 0 {
		slices.SortFunc(mine, oldestNote)
		b.dropNotes(mine[:over])
		waiting -= over
	}
	if waiting > maxMail {
		b.fitAllMail()
	}
}

// fitAllMail lets go of the notes waiting past their bounds, whoever sent
// them: each sender's oldest past maxSenderMail of theirs, and then, past
// maxMail on the board, notes in fair share (fairShare): the oldest of
// whoever has the most waiting, never one of a sender who has fewer while
// another has more. Restore fits a snapshot from an older server with it.
func (b *Board) fitAllMail() {
	var notes []heldNote
	from := map[string]int{}
	for _, m := range b.mail {
		for i, it := range m.Items {
			notes = append(notes, heldNote{m, i})
			from[it.From]++
		}
	}
	slices.SortFunc(notes, oldestNote)
	var drop []heldNote
	notes = slices.DeleteFunc(notes, func(n heldNote) bool {
		if f := n.item().From; from[f] > maxSenderMail {
			from[f]--
			drop = append(drop, n)
			return true
		}
		return false
	})
	if over := len(notes) - maxMail; over > 0 {
		drop = append(drop, fairShare(notes, func(n heldNote) string { return n.item().From }, oldestNote)[:over]...)
	}
	b.dropNotes(drop)
}

// heldNote is a note waiting in a mailbox: the mailbox's i'th.
type heldNote struct {
	m *mailbox
	i int
}

func (n heldNote) item() *InboxItem { return &n.m.Items[n.i] }

// oldestNote orders notes by when each was held, then ID.
func oldestNote(x, y heldNote) int {
	if c := x.item().At.Compare(y.item().At); c != 0 {
		return c
	}
	return strings.Compare(x.item().ID, y.item().ID)
}

// dropNotes lets go of notes, and of the mailboxes it leaves empty.
func (b *Board) dropNotes(notes []heldNote) {
	at := map[*mailbox][]int{}
	for _, n := range notes {
		at[n.m] = append(at[n.m], n.i)
	}
	for m, is := range at {
		slices.Sort(is)
		for _, i := range slices.Backward(is) {
			m.Items = slices.Delete(m.Items, i, i+1)
		}
		if len(m.Items) == 0 {
			delete(b.mail, mailKey(m.Repo, m.Member))
		}
	}
}

// collectMail moves the notes waiting for claim c's member in its
// repository into its inbox, in time order with what it holds, for its
// session to hear, as deliver is about to. Its other sessions hear them
// there too, as earlier news once one has.
func (b *Board) collectMail(now time.Time, c *claim) {
	k := mailKey(c.Repo, c.Member)
	m := b.mail[k]
	if m == nil {
		return
	}
	delete(b.mail, k)
	c.ownInbox()
	for _, it := range m.Items {
		if now.Sub(it.At) < inboxTTL {
			c.InboxSeq++
			it.Seq = c.InboxSeq // put in the inbox now, after what it holds
			c.Inbox = append(c.Inbox, it)
		}
	}
	slices.SortStableFunc(c.Inbox, func(x, y InboxItem) int { return x.At.Compare(y.At) })
	for len(c.Inbox) > maxInbox {
		i := evictIndex(c.Inbox)
		c.Inbox = slices.Delete(c.Inbox, i, i+1)
	}
}

// tidyMail drops the notes no session can be shown any more, and the
// mailboxes left empty, and reports whether it dropped any.
func (b *Board) tidyMail(now time.Time) bool {
	tidied := false
	for k, m := range b.mail {
		if len(m.Items) > 0 && now.Sub(m.Items[0].At) < inboxTTL {
			continue
		}
		m.Items = slices.DeleteFunc(m.Items, func(it InboxItem) bool { return now.Sub(it.At) >= inboxTTL })
		if len(m.Items) == 0 {
			delete(b.mail, k)
		}
		tidied = true
	}
	return tidied
}

// mailCopy copies the mailboxes for a snapshot, in order.
func (b *Board) mailCopy() []*mailbox {
	keys := make([]string, 0, len(b.mail))
	for k := range b.mail {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]*mailbox, 0, len(keys))
	for _, k := range keys {
		m := *b.mail[k]
		m.Items = slices.Clone(m.Items) // notes, whose paths and deliveries are nil
		out = append(out, &m)
	}
	return out
}
