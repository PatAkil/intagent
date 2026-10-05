package board

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A note to a member none of whose worktrees in the repository is listening
// waits for their next session there, in whichever worktree: one they never
// used before, which a note queued in a worktree they left would never
// reach. It is heard once, as a note in an inbox is, and saved with the
// board until it is.
func TestNoteWaitsForAMembersNextSession(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bob", "old", "b0", "go.mod")
	h.at(KindSessionEnd, "bob", "old", "b0")
	h.advance(30 * time.Hour)
	res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "bob", Text: "go.mod is mine today"})
	if err != nil || res.HeldFor != "bob" || len(res.Delivered) != 0 {
		t.Fatalf("note to bob, away: %+v, %v", res, err)
	}
	if acts := h.activities(ActivityNoteSent); len(acts) != 1 || !strings.Contains(acts[0].Text, "to bob, for their next session") {
		t.Fatalf("announced %+v", acts)
	}
	if n := h.b.statsFor(repo).Notes; n != 1 {
		t.Fatalf("the note held counted as %d notes", n)
	}
	// Saved and restored with the board, as a restart does.
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.b = New(DefaultConfig())
	if err := h.b.Restore(bytes.NewReader(data), h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Hour)
	if ctx := h.at(KindSessionStart, "bob", "fresh", "b1").Context; !strings.Contains(ctx, `Note from alice's agent: "go.mod is mine today"`) {
		t.Fatalf("bob's session in a fresh worktree was told:\n%s", ctx)
	}
	if ctx := h.at(KindSessionStart, "bob", "old", "b2").Context; strings.Contains(ctx, "go.mod is mine today") {
		t.Fatalf("bob's session in the worktree he left heard it again:\n%s", ctx)
	}
	if len(h.b.mail) != 0 {
		t.Fatalf("%d mailboxes left", len(h.b.mail))
	}
}

// A note to a member whose agent left a worktree a few hours ago, which
// still listens, waits for their next session all the same, which hears it
// in a fresh worktree. Before, it went to the worktree they had left, which
// per-task worktrees never go back to.
func TestNoteReachesAMembersFreshWorktree(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bob", "task1", "b1", "go.mod")
	h.at(KindSessionEnd, "bob", "task1", "b1")
	h.advance(2 * time.Hour)
	res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "bob", Text: "go.mod is mine today", ToMember: true})
	if err != nil || res.HeldFor != "bob" || len(res.Delivered) != 0 {
		t.Fatalf("note to bob, who left an hour ago: %+v, %v", res, err)
	}
	h.advance(time.Hour)
	if ctx := h.at(KindSessionStart, "bob", "task2", "b2").Context; !strings.Contains(ctx, "go.mod is mine today") {
		t.Fatalf("bob's next session, in a fresh worktree, was told:\n%s", ctx)
	}
}

// Notes collected from a mailbox join the inbox in time order with what it
// holds, which tidy relies on to find the oldest first; and one past
// inboxTTL, which no session can be shown, is left out, though no sweep has
// dropped it yet.
func TestCollectedNotesKeepTheInboxInOrder(t *testing.T) {
	h := newHarness(t)
	h.at(KindPostEdit, "bob", "w", "b1", "go.mod")
	h.at(KindSessionEnd, "bob", "w", "b1")
	send := func(text string) {
		t.Helper()
		if _, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "bob", Text: text, ToMember: true}); err != nil {
			t.Fatal(err)
		}
	}
	send("stale")
	h.advance(inboxTTL - time.Hour)
	send("fresh")
	h.advance(time.Minute)
	h.hook(KindPostEdit, "alice", "a1", "go.mod") // an alert, queued in bob's inbox after the notes
	h.advance(time.Hour)
	h.at(KindPrompt, "bob", "w", "b2")
	var got []string
	for _, it := range h.claimIn("bob", "w").Inbox {
		got = append(got, it.Kind+":"+it.Text)
	}
	want := []string{`note:Note from alice's agent: "fresh"`, "overlap:alice's agent also changed go.mod on branch feat/alice."}
	if !slices.Equal(got, want) {
		t.Fatalf("bob's inbox holds %q, want %q", got, want)
	}
}

// A note to a member of the team who has no claim in the repository yet
// waits for their first session there; one to a name the team does not
// know, and nothing on the board answers to, finds no one.
func TestNoteWaitsForAMemberNewToTheRepository(t *testing.T) {
	h := newHarness(t)
	res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "carol", Text: "start with the docs", ToMember: true})
	if err != nil || res.HeldFor != "carol" {
		t.Fatalf("note to carol: %+v, %v", res, err)
	}
	if _, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "nobody", Text: "x"}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("a note to nobody: %v", err)
	}
	if _, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "alice", Text: "x", ToMember: true}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("a note to herself: %v", err)
	}
	if ctx := h.hook(KindPrompt, "carol", "c1").Context; !strings.Contains(ctx, "start with the docs") {
		t.Fatalf("carol's first prompt was told:\n%s", ctx)
	}
}

// Mailboxes are bounded: a note a day old is dropped by the sweep, one
// holds at most maxMailItems, a flood from one sender pushing out its own,
// and one sender's notes waiting across mailboxes are at most
// maxSenderMail, its own oldest let go of first. Before, the board held
// 1024 mailboxes and let go of the one whose newest note was oldest, so
// mallory's notes to invented repositories pushed out carol's.
func TestMailboxesAreBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.NotesPerMinute = 1000 })
	note := func(from, to, repo, text string) {
		t.Helper()
		w := whereOf(from)
		w.Repo = repo
		if _, err := h.b.Note(h.now, NoteRequest{Member: from, Where: w, To: to, Text: text, ToMember: true}); err != nil {
			t.Fatal(err)
		}
	}
	note("carol", "bob", repo, "from carol")
	for i := range maxMailItems + 5 {
		h.advance(time.Second)
		note("mallory", "bob", repo, fmt.Sprint("ping ", i))
	}
	m := h.b.mail[mailKey(repo, "bob")]
	if len(m.Items) != maxMailItems || m.Items[0].From != "carol" {
		t.Fatalf("bob's mailbox holds %d notes, the first from %s", len(m.Items), m.Items[0].From)
	}
	for i := range 1024 {
		h.advance(time.Second)
		note("mallory", "bob", fmt.Sprintf("github.com/acme/r%04d", i), "x")
	}
	// carol's note, and mallory's newest maxSenderMail, one a mailbox.
	if m := h.b.mail[mailKey(repo, "bob")]; m == nil || len(m.Items) != 1 || m.Items[0].From != "carol" {
		t.Fatalf("bob's mailbox in %s: %+v", repo, m)
	}
	if len(h.b.mail) != 1+maxSenderMail {
		t.Fatalf("%d mailboxes, want carol's and %d of mallory's", len(h.b.mail), maxSenderMail)
	}
	for i := 1024 - maxSenderMail; i < 1024; i++ {
		if h.b.mail[mailKey(fmt.Sprintf("github.com/acme/r%04d", i), "bob")] == nil {
			t.Fatalf("mallory's note %d, one of her newest, was let go of", i)
		}
	}
	h.advance(inboxTTL)
	h.b.Sweep(h.now)
	if len(h.b.mail) != 0 {
		t.Fatalf("%d mailboxes a day later", len(h.b.mail))
	}
}

// One member's notes at the rate limit, to someone with no claim in
// repositories they make up, do not push out a note held for a teammate:
// in the persist lane's probe, eve's 1024th note, 51 minutes on, evicted
// the note alice left for carol.
func TestOneSendersNotesKeepATeammatesHeldNote(t *testing.T) {
	h := newHarness(t)
	h.hook(KindSessionStart, "carol", "c1")
	h.hook(KindSessionEnd, "carol", "c1")
	res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "carol",
		Text: "the migration lands at 3pm, hold your PR", ToMember: true})
	if err != nil || res.HeldFor != "carol" {
		t.Fatalf("alice's note to carol: %+v, %v", res, err)
	}
	for i := range 1100 {
		h.advance(3 * time.Second) // 20 a minute, the default limit
		w := Where{Repo: fmt.Sprintf("github.com/eve/r%04d", i), Host: "eve-laptop", Worktree: "/w"}
		if _, err := h.b.Note(h.now, NoteRequest{Member: "eve", Where: w, To: "bob", Text: "ping", ToMember: true}); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	if h.b.mail[mailKey(repo, "carol")] == nil {
		t.Fatal("carol's note was let go of for eve's")
	}
	if ctx := h.hook(KindSessionStart, "carol", "c2").Context; !strings.Contains(ctx, "the migration lands at 3pm") {
		t.Fatalf("carol's next session was told:\n%s", ctx)
	}
}

// Past maxMail notes waiting on the board, which takes more senders than
// maxMail/maxSenderMail, a new note makes room in fair share: the oldest
// note of whoever has the most waiting goes, never one of a sender who has
// fewer while another has more.
func TestTheBoardsMailIsSharedFairly(t *testing.T) {
	h := newHarness(t)
	at := h.now
	put := func(from string, n int) {
		for i := range n {
			at = at.Add(time.Second)
			k := mailKey(fmt.Sprintf("github.com/%s/r%02d", from, i), "bob")
			h.b.mail[k] = &mailbox{Repo: fmt.Sprintf("github.com/%s/r%02d", from, i), Member: "bob",
				Items: []InboxItem{{ID: fmt.Sprintf("%s-%02d", from, i), At: at, Kind: "note", From: from, Text: "x"}}}
		}
	}
	senders := maxMail/maxSenderMail - 1
	for s := range senders { // each at the bound
		put(fmt.Sprintf("s%02d", s), maxSenderMail)
	}
	put("few", maxSenderMail-4)
	put("carol", 4)
	if n := mailNotes(h.b); n != maxMail {
		t.Fatalf("set up %d notes, want %d", n, maxMail)
	}
	h.now = at.Add(time.Second)
	for _, from := range []string{"carol", "few", "carol"} {
		h.b.hold(h.now, repo, "dave", InboxItem{Kind: "note", From: from, Text: "y"})
	}
	if n := mailNotes(h.b); n != maxMail {
		t.Fatalf("%d notes held, want %d", n, maxMail)
	}
	held := map[string]int{}
	for _, m := range h.b.mail {
		for _, it := range m.Items {
			held[it.From]++
		}
	}
	if held["carol"] != 6 || held["few"] != maxSenderMail-3 {
		t.Fatalf("carol has %d notes waiting, few %d; want all of each", held["carol"], held["few"])
	}
	// The three let go of: the oldest of the senders at the bound, s00's,
	// s01's and s02's first notes.
	for s := range senders {
		k := mailKey(fmt.Sprintf("github.com/s%02d/r00", s), "bob")
		if gone := h.b.mail[k] == nil; gone != (s < 3) {
			t.Errorf("s%02d's oldest note let go of: %v", s, gone)
		}
	}
}

// A snapshot from an older server, which kept up to 1024 mailboxes of 20
// notes whoever sent them, is fitted to the bounds as it is restored.
func TestRestoredMailIsFitted(t *testing.T) {
	h := newHarness(t)
	h.b.mail[mailKey(repo, "carol")] = &mailbox{Repo: repo, Member: "carol",
		Items: []InboxItem{{ID: "c", At: h.now, Kind: "note", From: "alice", Text: "x"}}}
	for i := range 100 {
		r := fmt.Sprintf("github.com/eve/r%03d", i)
		h.b.mail[mailKey(r, "bob")] = &mailbox{Repo: r, Member: "bob",
			Items: []InboxItem{{ID: fmt.Sprint("e", i), At: h.now.Add(time.Duration(i+1) * time.Second), Kind: "note", From: "eve", Text: "x"}}}
	}
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig())
	if err := b.Restore(bytes.NewReader(data), h.now); err != nil {
		t.Fatal(err)
	}
	if len(b.mail) != 1+maxSenderMail || b.mail[mailKey(repo, "carol")] == nil || b.mail[mailKey("github.com/eve/r099", "bob")] == nil {
		t.Fatalf("restored %d mailboxes; carol's: %v; eve's newest: %v", len(b.mail),
			b.mail[mailKey(repo, "carol")] != nil, b.mail[mailKey("github.com/eve/r099", "bob")] != nil)
	}
}

func mailNotes(b *Board) int {
	n := 0
	for _, m := range b.mail {
		n += len(m.Items)
	}
	return n
}
