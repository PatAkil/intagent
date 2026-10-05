package board

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
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
	h.edit("alice", "a1", "svc/later.go") // a file changed since
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
	want := durableView(h.b)
	durable := durableOf(t, h.b, h.now)

	b := New(DefaultConfig())
	if err := b.Restore(bytes.NewReader(snap), h.now); err != nil {
		t.Fatal(err)
	}
	if applied, err := b.RestoreDurable(bytes.NewReader(durable), h.now, time.Time{}); err != nil || !applied {
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
	if applied, err := b.RestoreDurable(bytes.NewReader(durable), h.now, time.Time{}); err != nil || applied {
		t.Fatalf("an older durable part: applied %t, %v", applied, err)
	}
	if c := b.findClaim("alice", whereOf("alice")); c != nil && len(c.Intents) != 0 {
		t.Fatalf("the released intent came back: %+v", c.Intents)
	}
	// Nor is one the snapshot was saved with, at the same version.
	same := durableOf(t, b, h.now)
	if applied, err := b.RestoreDurable(bytes.NewReader(same), h.now, time.Time{}); err != nil || applied {
		t.Fatalf("a durable part of the snapshot's version: applied %t, %v", applied, err)
	}
}

// The digest of what agents cannot send again changes with intents, notes
// and mail, with sessions ending and claims leaving the board, and with the
// claims, phases and tool calls of the sessions that keep reservations
// live; and not with what every hook changes: files, the feed, the stats,
// other sessions, when a session was last heard from, and notes being shown.
// So a server saves the durable part when it changes, not at every hook.
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
	step("a reservation", true, func() { h.declare("alice", ModeExclusive, "svc", "svc/**") })
	step("its holder going into a tool", true, func() { h.hook(KindToolStart, "alice", "a1") })
	step("its holder heard from in it", false, func() { h.hook(KindHeartbeat, "alice", "a1") })
	step("its holder out of it", true, func() { h.hook(KindToolEnd, "alice", "a1") })
	step("its holder's next prompt", false, func() { h.hook(KindPrompt, "alice", "a1") })
	step("its holder moving", true, func() { h.at(KindPrompt, "alice", "elsewhere", "a1") })
	step("a teammate going into a tool", false, func() { h.hook(KindToolStart, "bob", "b1") })
	step("a teammate's session ending", true, func() { h.hook(KindSessionEnd, "bob", "b1") })
	step("the board saved whole", true, func() { h.b.SnapshotSaved(h.b.Version()) })
}

// The durable part holds the live sessions of the claims that hold
// reservations, those reporting from them and those that moved from them,
// with what liveness reads and the identity of the claims they report from;
// not other sessions, nor those of a reservation that are not live.
func TestDurablePartHoldsTheSessionsOfReservations(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "svc", "svc/**")
	h.hook(KindPrompt, "alice", "a2")
	h.hook(KindSessionEnd, "alice", "a2") // not live
	h.hook(KindPrompt, "carol", "c1")
	h.declare("carol", ModeExclusive, "web", "web/**")
	h.at(KindToolStart, "carol", "moved", "c1") // carries carol's reservation from elsewhere
	h.hook(KindPrompt, "bob", "b1")             // holds none
	var d durableState
	if err := json.Unmarshal(durableOf(t, h.b, h.now), &d); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range d.Sessions {
		got = append(got, fmt.Sprintf("%s/%s in %s tool=%t also=%d acked=%d", x.Member, x.ID, x.ClaimID, inTool(x), len(x.Also),
			len(x.Acked)))
	}
	moved := h.claimIn("carol", "moved").ID
	want := []string{
		fmt.Sprintf("alice/a1 in %s tool=false also=0 acked=0", h.b.findClaim("alice", whereOf("alice")).ID),
		fmt.Sprintf("carol/c1 in %s tool=true also=1 acked=0", moved),
	}
	if !slices.Equal(got, want) {
		t.Errorf("the durable part holds the sessions\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.ContainsFunc(d.Claims, func(c *claim) bool { return c.ID == moved && len(c.Intents) == 0 }) {
		t.Errorf("the durable part leaves out the claim carol's session reports from")
	}
}

// A board restored from a snapshot and the durable part saved after it
// ends the sessions that ended since, takes off the board the claims
// released since, and has the sessions of reservations as they were, their
// silence credited with the downtime; until it is saved whole again, its
// own durable part says so too, for a crash before then.
func TestDurablePartRestoresEndsAndReservationsSince(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "svc", "svc/**")
	h.hook(KindPrompt, "bob", "b1")
	h.hook(KindPostEdit, "carol", "c1", "web/a.go")
	snap, version, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.hook(KindToolStart, "alice", "a1")
	h.hook(KindSessionEnd, "bob", "b1") // which releases bob's empty claim
	h.b.mu.Lock()
	bobs, carols := h.b.sessions[sessionKey("bob", AgentClaudeCode, "b1")].ClaimID, h.b.findClaim("carol", whereOf("carol")).ID
	h.b.mu.Unlock()
	h.b.SnapshotSaved(version) // a snapshot from before the end says nothing of it
	durable := durableOf(t, h.b, h.now)

	b := New(DefaultConfig())
	restart := h.now.Add(time.Minute)
	if err := b.Restore(bytes.NewReader(snap), restart); err != nil {
		t.Fatal(err)
	}
	if applied, err := b.RestoreDurable(bytes.NewReader(durable), restart, h.now); err != nil || !applied {
		t.Fatalf("RestoreDurable = %t, %v", applied, err)
	}
	alice, bob, carol := b.sessions[sessionKey("alice", AgentClaudeCode, "a1")], b.sessions[sessionKey("bob", AgentClaudeCode, "b1")],
		b.sessions[sessionKey("carol", AgentClaudeCode, "c1")]
	if bob.Phase != phaseEnded || bob.Reported != StateEnded || b.claims[bobs] != nil {
		t.Errorf("bob's session restored %s, announced %s; his claim released since is on the board: %t", bob.Phase, bob.Reported,
			b.claims[bobs] != nil)
	}
	if !inTool(alice) || !alice.LastSeen.Equal(restart) || alice.Unsure {
		t.Errorf("alice's session restored in a tool %t, last seen %s (want %s), unsure %t", inTool(alice), alice.LastSeen, restart,
			alice.Unsure)
	}
	if !carol.Unsure || b.claims[carols] == nil {
		t.Errorf("carol's working session, which the durable part does not hold, is not unsure")
	}
	mustIndex(t, b, "the durable part")
	var again durableState
	if err := json.Unmarshal(durableOf(t, b, restart), &again); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.Ended, []string{bob.Key}) || !slices.Equal(again.Removed, []string{bobs}) {
		t.Errorf("restored, the board's durable part says %v ended and %v were removed", again.Ended, again.Removed)
	}
	b.SnapshotSaved(b.Version())
	var saved durableState
	if err := json.Unmarshal(durableOf(t, b, restart), &saved); err != nil || saved.Ended != nil || saved.Removed != nil {
		t.Errorf("saved whole, the board's durable part says %v ended and %v were removed (%v)", saved.Ended, saved.Removed, err)
	}
	// The next hook of carol's makes her sure again.
	if _, err := b.Hook(restart, HookEvent{Kind: KindPrompt, Member: "carol", Agent: AgentClaudeCode, SessionID: "c1",
		Where: whereOf("carol")}); err != nil || carol.Unsure {
		t.Errorf("carol's session is unsure after her next hook (%v)", err)
	}
}

// What the durable part says ended or left the board since the last whole
// save is bounded, should those saves fail for long: the earliest go first.
func TestNotedEndsAreBounded(t *testing.T) {
	noted := map[string]uint64{}
	for i := range maxNoted + 1 {
		noted[fmt.Sprintf("s%05d", i)] = uint64(maxNoted + 1 - i) // the last key ended first
	}
	boundNoted(noted)
	if len(noted) != maxNoted*9/10 {
		t.Fatalf("%d kept, want %d", len(noted), maxNoted*9/10)
	}
	if _, ok := noted[fmt.Sprintf("s%05d", maxNoted)]; ok {
		t.Error("the first to end was kept")
	}
	if _, ok := noted["s00000"]; !ok {
		t.Error("the last to end was let go of")
	}
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
		if applied, err := b.RestoreDurable(bytes.NewReader(data), h.now, time.Time{}); err != nil || !applied {
			t.Fatalf("%s durable part: %t, %v", name, applied, err)
		}
		if c := b.findClaim("alice", whereOf("alice")); len(c.Intents) != 2 {
			t.Errorf("%s durable part: intents %+v", name, c.Intents)
		}
	}
	for name, data := range map[string][]byte{"cut short": durable[:len(durable)/2], "gzip cut short": zip(durable)[:20],
		"empty": nil, "two": append(slices.Clone(durable), durable...)} {
		if _, err := New(DefaultConfig()).RestoreDurable(bytes.NewReader(data), h.now, time.Time{}); !errors.Is(err, ErrCorruptSnapshot) {
			t.Errorf("durable part %s: %v, want it reported as damaged", name, err)
		}
	}
	if _, err := New(DefaultConfig()).RestoreDurable(strings.NewReader(`{"format":2}`), h.now, time.Time{}); err == nil || errors.Is(err, ErrCorruptSnapshot) {
		t.Errorf("a durable part of a newer format: %v, want an error that is not damage", err)
	}
	broken := errors.New("input/output error")
	if _, err := New(DefaultConfig()).RestoreDurable(io.MultiReader(bytes.NewReader(zip(durable)[:30]), failingReader{broken}), h.now, time.Time{}); !errors.Is(err, broken) ||
		errors.Is(err, ErrCorruptSnapshot) {
		t.Errorf("a durable part that could not be read: %v", err)
	}
}

func flip(data []byte, i int) []byte {
	out := slices.Clone(data)
	out[i] ^= 0x40
	return out
}
