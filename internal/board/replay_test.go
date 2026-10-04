package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// recordIn adds activities to the feed, one in each repository named, in order.
func recordIn(b *Board, repos ...string) {
	b.lock()
	defer b.unlock()
	for _, r := range repos {
		b.record(Activity{At: t0, Kind: ActivityFileChanged, Repo: r})
	}
}

func seqs(acts []Activity) []uint64 {
	out := []uint64{}
	for _, a := range acts {
		out = append(out, a.Seq)
	}
	return out
}

// restored is a board with keep activities restored from data.
func restored(t *testing.T, data []byte, keep int) *Board {
	t.Helper()
	b := New(Config{KeepActivities: keep})
	if err := b.Restore(bytes.NewReader(data), t0); err != nil {
		t.Fatal(err)
	}
	return b
}

// A dashboard that reconnects after the feed has let go of what it missed
// must be told, not handed a replay with a silent hole in it: at 200
// activities a second, a 3 s reconnect lost 300 of 600 without a sign. A
// restart must not tell it so when it missed nothing: the dashboard of a
// quiet repository was told after every restart that it had missed activity,
// because the board forgot which repositories it had let go of.
func TestReplaySaysWhenItMissesSomething(t *testing.T) {
	const ra, rb = "github.com/acme/a", "github.com/acme/b"
	b := New(Config{KeepActivities: 6})
	recordIn(b, ra, ra, rb, ra, rb, rb, ra, rb) // seqs 1 to 8; 1 and 2, both in a, are let go of
	data, _, err := b.Snapshot(t0)
	if err != nil {
		t.Fatal(err)
	}
	// A snapshot from a server that did not keep what it let go of.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "dropped")
	older, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := restored(t, older, 6).Snapshot(t0)
	if err != nil {
		t.Fatal(err)
	}
	same, shorter := restored(t, data, 6), restored(t, data, 3) // shorter keeps 6, 7 and 8
	fromOlder, fromOlderShorter, fromOlderAgain := restored(t, older, 6), restored(t, older, 3), restored(t, again, 6)
	for _, c := range []struct {
		what     string
		b        *Board
		repo     string
		after    uint64
		want     []uint64
		complete bool
	}{
		{"a, from the start", b, ra, 0, []uint64{4, 7}, false},
		{"a, after the first dropped", b, ra, 1, []uint64{4, 7}, false},
		{"a, after the last dropped", b, ra, 2, []uint64{4, 7}, true},
		{"b, none of whose were dropped", b, rb, 0, []uint64{3, 5, 6, 8}, true},
		{"all, after the first dropped", b, "", 1, []uint64{3, 4, 5, 6, 7, 8}, false},
		{"all, after the last dropped", b, "", 2, []uint64{3, 4, 5, 6, 7, 8}, true},
		{"all, up to date", b, "", 8, []uint64{}, true},
		{"restored: a, after the first dropped", same, ra, 1, []uint64{4, 7}, false},
		{"restored: a, after the last dropped", same, ra, 2, []uint64{4, 7}, true},
		{"restored: b, none of whose were dropped", same, rb, 0, []uint64{3, 5, 6, 8}, true},
		{"restored: all, after the first dropped", same, "", 1, []uint64{3, 4, 5, 6, 7, 8}, false},
		{"restored: all, after the last dropped", same, "", 2, []uint64{3, 4, 5, 6, 7, 8}, true},
		{"restored shorter: a, before 4, let go of now", shorter, ra, 3, []uint64{7}, false},
		{"restored shorter: a, after 4", shorter, ra, 4, []uint64{7}, true},
		{"restored shorter: b, before 5, let go of now", shorter, rb, 4, []uint64{6, 8}, false},
		{"restored shorter: b, after 5", shorter, rb, 5, []uint64{6, 8}, true},
		{"restored shorter: all, after 5", shorter, "", 5, []uint64{6, 7, 8}, true},
		{"from an older snapshot: b, whose losses it cannot tell", fromOlder, rb, 0, []uint64{3, 5, 6, 8}, false},
		{"from an older snapshot: b, after what it let go of", fromOlder, rb, 2, []uint64{3, 5, 6, 8}, true},
		{"from an older snapshot, shorter: a, after 4", fromOlderShorter, ra, 4, []uint64{7}, true},
		{"from an older snapshot, shorter: b, before 5", fromOlderShorter, rb, 4, []uint64{6, 8}, false},
		{"from a snapshot of a board restored from an older one: b", fromOlderAgain, rb, 0, []uint64{3, 5, 6, 8}, false},
		{"from a snapshot of a board restored from an older one: b, after 2", fromOlderAgain, rb, 2, []uint64{3, 5, 6, 8}, true},
	} {
		acts, complete := c.b.Replay(c.repo, c.after)
		if got := seqs(acts); !slices.Equal(got, c.want) || complete != c.complete {
			t.Errorf("%s: Replay(%q, %d) = %v, %v; want %v, %v", c.what, c.repo, c.after, got, complete, c.want, c.complete)
		}
		if got := seqs(c.b.Since(c.repo, c.after)); !slices.Equal(got, c.want) {
			t.Errorf("%s: Since = %v, want %v", c.what, got, c.want)
		}
	}
}

// The board remembers what it dropped for a bounded number of repositories;
// past that it answers for all of them at once, and may then report a hole
// that is not there, but never misses one.
func TestReplayForgetsRepositoriesBoundedly(t *testing.T) {
	b := New(Config{KeepActivities: 1})
	for i := range maxDroppedRepos + 2 {
		recordIn(b, fmt.Sprintf("github.com/acme/r%d", i))
	}
	if len(b.dropped) > maxDroppedRepos {
		t.Fatalf("%d repositories remembered", len(b.dropped))
	}
	last := b.Since("", 0)[0].Seq
	if _, complete := b.Replay("github.com/acme/r0", last-2); complete {
		t.Fatal("a replay from before a dropped activity of its repository says it is complete")
	}
	if _, complete := b.Replay("github.com/acme/r0", last-1); !complete {
		t.Fatal("a replay from after every dropped activity says it is not complete")
	}
}
