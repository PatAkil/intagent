package board

import (
	"bytes"
	"errors"
	"fmt"
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
// and the board at most maxMailboxes, letting go of the one whose newest
// note is oldest.
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
	for i := range maxMailboxes {
		h.advance(time.Second)
		note("mallory", "bob", fmt.Sprintf("github.com/acme/r%04d", i), "x")
	}
	if len(h.b.mail) != maxMailboxes || h.b.mail[mailKey(repo, "bob")] != nil {
		t.Fatalf("%d mailboxes; the oldest kept: %v", len(h.b.mail), h.b.mail[mailKey(repo, "bob")] != nil)
	}
	h.advance(inboxTTL)
	h.b.Sweep(h.now)
	if len(h.b.mail) != 0 {
		t.Fatalf("%d mailboxes a day later", len(h.b.mail))
	}
}
