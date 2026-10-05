package board

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// durableOf is the durable part of b, written.
func durableOf(t *testing.T, b *Board, now time.Time) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := b.Durable(now).WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// durableView is what of a board's claims is durable, and its mail, feed and
// stats, to compare.
func durableView(b *Board) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var lines []string
	for _, id := range sortedKeys(b.claims) {
		c := b.claims[id]
		var notes []string
		for _, it := range c.Inbox {
			if it.Kind == "note" {
				notes = append(notes, fmt.Sprintf("%s@%d:%s", it.ID, it.Seq, it.Text))
			}
		}
		intents := "none"
		if len(c.Intents) > 0 {
			intents = jsonOf(c.Intents)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s intents=%s notes=%v", id, c.Member, c.Worktree, intents, notes))
	}
	lines = append(lines, "mail "+jsonOf(b.mailCopy()), fmt.Sprintf("seq %d", b.seq), "recent "+jsonOf(b.recent), "stats "+jsonOf(b.stats))
	return strings.Join(lines, "\n")
}

// A board restored from a snapshot and then from the durable part saved
// after it has the intents, notes, mail, feed and stats of the board when
// the durable part was saved: an intent declared, one released, a note sent,
// one held for a member away, a claim made with an intent since. What its
// agents send it again (here a file changed since the snapshot) is as the
// snapshot had it.
func TestDurablePartRestoresWhatAgentsCannotSendAgain(t *testing.T) {
	h := newHarness(t)
	h.edit("alice", "a1", "svc/a.go")
	h.declare("alice", ModeExclusive, "Rework svc", "svc/**")
	h.edit("bob", "b1", "lib/b.go")
	h.declare("bob", ModeShared, "Tidy lib", "lib/**")
	snap, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.declare("alice", ModeShared, "And docs", "docs/**")
	if _, err := h.b.Release(h.now, ReleaseRequest{Member: "bob", Where: whereOf("bob")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.Note(h.now, NoteRequest{Member: "bob", Where: whereOf("bob"), To: "alice", Text: "svc/a.go is yours"}); err != nil {
		t.Fatal(err)
	}
	if res, err := h.b.Note(h.now, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "carol", Text: "hi", ToMember: true}); err != nil ||
		res.HeldFor != "carol" {
		t.Fatalf("note to carol: %+v %v", res, err)
	}
	h.declare("dave", ModeShared, "New work", "web/**") // a claim made since the snapshot
	h.edit("alice", "a1", "svc/later.go")               // and a file changed since
	want := durableView(h.b)
	durable := durableOf(t, h.b, h.now)

	b := New(DefaultConfig())
	if err := b.Restore(bytes.NewReader(snap), h.now); err != nil {
		t.Fatal(err)
	}
	if applied, err := b.RestoreDurable(bytes.NewReader(durable)); err != nil || !applied {
		t.Fatalf("RestoreDurable = %t, %v", applied, err)
	}
	if got := durableView(b); got != want {
		t.Fatalf("restored:\n%s\nwant:\n%s", got, want)
	}
	alice := b.findClaim("alice", whereOf("alice"))
	if _, ok := alice.Footprint["svc/later.go"]; ok || len(alice.Footprint) != 1 {
		t.Errorf("alice's files are %v, want the snapshot's", sortedKeys(alice.Footprint))
	}
	if dave := b.findClaim("dave", whereOf("dave")); dave == nil || dave.Task != "New work" || len(dave.Footprint) != 0 {
		t.Errorf("dave's claim made since the snapshot: %+v", dave)
	}
	mustIndex(t, b, "the durable part")
	// What was restored is saved as it was.
	if again := durableOf(t, b, h.now); !bytes.Equal(again, durable) {
		t.Errorf("saved again:\n%s\nwant\n%s", again, durable)
	}
	// Alice's agent hears the note bob sent, as it would have.
	res, err := b.Hook(h.now, HookEvent{Kind: KindPrompt, Member: "alice", Agent: AgentClaudeCode, SessionID: "a1", Where: whereOf("alice")})
	if err != nil || !strings.Contains(res.Context, "svc/a.go is yours") {
		t.Errorf("alice's next prompt was told %q (%v)", res.Context, err)
	}
}

// A durable part saved before the snapshot is not applied: the snapshot
// holds all of it, and maybe later changes.
func TestDurablePartOlderThanTheSnapshotIsLeftOut(t *testing.T) {
	h := newHarness(t)
	h.declare("alice", ModeExclusive, "Rework svc", "svc/**")
	durable := durableOf(t, h.b, h.now)
	if _, err := h.b.Release(h.now, ReleaseRequest{Member: "alice", Where: whereOf("alice")}); err != nil {
		t.Fatal(err)
	}
	snap, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig())
	if err := b.Restore(bytes.NewReader(snap), h.now); err != nil {
		t.Fatal(err)
	}
	if applied, err := b.RestoreDurable(bytes.NewReader(durable)); err != nil || applied {
		t.Fatalf("an older durable part: applied %t, %v", applied, err)
	}
	if c := b.findClaim("alice", whereOf("alice")); c != nil && len(c.Intents) != 0 {
		t.Fatalf("the released intent came back: %+v", c.Intents)
	}
	// Nor is one the snapshot was saved with, at the same version.
	same := durableOf(t, b, h.now)
	if applied, err := b.RestoreDurable(bytes.NewReader(same)); err != nil || applied {
		t.Fatalf("a durable part of the snapshot's version: applied %t, %v", applied, err)
	}
}

// The digest of what agents cannot send again changes with intents, notes
// and mail, and not with what every hook changes: files, the feed, the
// stats, sessions, and notes being shown. So a server saves the durable part
// when it changes, not at every hook.
func TestDurableDigestChangesWithWhatIsKept(t *testing.T) {
	h := newHarness(t)
	h.edit("alice", "a1", "svc/a.go")
	digest := func() uint64 { return h.b.Durable(h.now).Kept() }
	last := digest()
	step := func(name string, changes bool, do func()) {
		t.Helper()
		h.advance(time.Second)
		do()
		d := digest()
		if (d != last) != changes {
			t.Errorf("%s: the digest changed %t, want %t", name, d != last, changes)
		}
		last = d
	}
	step("an edit", false, func() { h.edit("alice", "a1", "svc/b.go") })
	step("a prompt", false, func() { h.hook(KindPrompt, "bob", "b1") })
	step("a declaration", true, func() { h.declare("alice", ModeShared, "svc", "svc/**") })
	step("a note", true, func() {
		if _, err := h.b.Note(h.now, NoteRequest{Member: "bob", Where: whereOf("bob"), To: "alice", Text: "hello"}); err != nil {
			t.Fatal(err)
		}
	})
	step("the note shown", false, func() { h.hook(KindPrompt, "alice", "a1") })
	step("a held note", true, func() {
		if _, err := h.b.Note(h.now, NoteRequest{Member: "bob", Where: whereOf("bob"), To: "carol", Text: "hi", ToMember: true}); err != nil {
			t.Fatal(err)
		}
	})
	step("an alert", false, func() { h.hook(KindPostEdit, "bob", "b1", "svc/a.go") })
	step("a release", true, func() {
		if _, err := h.b.Release(h.now, ReleaseRequest{Member: "alice", Where: whereOf("alice")}); err != nil {
			t.Fatal(err)
		}
	})
}

// A snapshot or durable part compressed with gzip, as a server saves them,
// restores as the plain one does; one cut short, or damaged, is reported as
// damaged; and a plain snapshot from an older server still restores.
func TestRestoreReadsGzip(t *testing.T) {
	h := newHarness(t)
	h.edit("alice", "a1", "svc/a.go")
	h.declare("alice", ModeShared, "svc", "svc/**")
	plain, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	zip := func(data []byte) []byte {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		_, _ = zw.Write(data)
		_ = zw.Close()
		return buf.Bytes()
	}
	zipped := zip(plain)
	for name, data := range map[string][]byte{"plain": plain, "gzip": zipped} {
		b := New(DefaultConfig())
		if err := b.Restore(bytes.NewReader(data), h.now); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, _, _ := b.Snapshot(h.now)
		if !bytes.Equal(again, plain) {
			t.Errorf("%s: restored and saved again, the snapshot differs", name)
		}
	}
	for name, data := range map[string][]byte{"cut short": zipped[:len(zipped)/2], "no end": zipped[:len(zipped)-4],
		"a bit flipped": flip(zipped, len(zipped)/2), "header only": zipped[:10]} {
		if err := New(DefaultConfig()).Restore(bytes.NewReader(data), h.now); !errors.Is(err, ErrCorruptSnapshot) {
			t.Errorf("%s: %v, want it reported as damaged", name, err)
		}
	}
	h.declare("alice", ModeShared, "more", "docs/**") // after the snapshot, so the durable part is newer
	durable := durableOf(t, h.b, h.now)
	for name, data := range map[string][]byte{"plain": durable, "gzip": zip(durable)} {
		b := New(DefaultConfig())
		if err := b.Restore(bytes.NewReader(zipped), h.now); err != nil {
			t.Fatal(err)
		}
		if applied, err := b.RestoreDurable(bytes.NewReader(data)); err != nil || !applied {
			t.Fatalf("%s durable part: %t, %v", name, applied, err)
		}
		if c := b.findClaim("alice", whereOf("alice")); len(c.Intents) != 2 {
			t.Errorf("%s durable part: intents %+v", name, c.Intents)
		}
	}
	for name, data := range map[string][]byte{"cut short": durable[:len(durable)/2], "gzip cut short": zip(durable)[:20],
		"empty": nil, "two": append(slices.Clone(durable), durable...)} {
		if _, err := New(DefaultConfig()).RestoreDurable(bytes.NewReader(data)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Errorf("durable part %s: %v, want it reported as damaged", name, err)
		}
	}
	if _, err := New(DefaultConfig()).RestoreDurable(strings.NewReader(`{"format":2}`)); err == nil || errors.Is(err, ErrCorruptSnapshot) {
		t.Errorf("a durable part of a newer format: %v, want an error that is not damage", err)
	}
	broken := errors.New("input/output error")
	if _, err := New(DefaultConfig()).RestoreDurable(io.MultiReader(bytes.NewReader(zip(durable)[:30]), failingReader{broken})); !errors.Is(err, broken) ||
		errors.Is(err, ErrCorruptSnapshot) {
		t.Errorf("a durable part that could not be read: %v", err)
	}
}

func flip(data []byte, i int) []byte {
	out := slices.Clone(data)
	out[i] ^= 0x40
	return out
}
