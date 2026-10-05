package board

import (
	"cmp"
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

// Bounds on mailboxes: the notes one keeps, and the mailboxes the board
// does, as a member can name any repository.
const (
	maxMailItems  = 20
	maxMailboxes  = 1024
	mailKeyJoiner = "\n" // in neither a repository ID nor a member's name
)

func mailKey(repo, member string) string { return repo + mailKeyJoiner + member }

// hold puts a note for member in their mailbox for repo, letting go of what
// a full one, or a full board of them, holds as enqueue lets go of a full
// inbox's items.
func (b *Board) hold(now time.Time, repo, member string, it InboxItem) {
	k := mailKey(repo, member)
	m := b.mail[k]
	if m == nil {
		if len(b.mail) >= maxMailboxes {
			b.dropOldestMailbox()
		}
		m = &mailbox{Repo: repo, Member: member}
		b.mail[k] = m
	}
	it.ID, it.At, it.Text = b.newID("i_"), now, Clean(it.Text, maxNoteLen)
	m.Items = append(m.Items, it)
	for len(m.Items) > maxMailItems {
		i := evictIndex(m.Items)
		m.Items = slices.Delete(m.Items, i, i+1)
	}
}

// dropOldestMailbox lets go of the mailbox whose newest note is oldest,
// ties going by member and repository.
func (b *Board) dropOldestMailbox() {
	oldest := ""
	for k, m := range b.mail {
		if oldest == "" {
			oldest = k
			continue
		}
		o := b.mail[oldest]
		if c := cmp.Or(m.newest().Compare(o.newest()), strings.Compare(k, oldest)); c < 0 {
			oldest = k
		}
	}
	delete(b.mail, oldest)
}

func (m *mailbox) newest() time.Time {
	if len(m.Items) == 0 {
		return time.Time{}
	}
	return m.Items[len(m.Items)-1].At
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
