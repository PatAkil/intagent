package board

import (
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

// A dashboard that reconnects after the feed has let go of what it missed
// must be told, not handed a replay with a silent hole in it: at 200
// activities a second, a 3 s reconnect lost 300 of 600 without a sign.
func TestReplaySaysWhenItMissesSomething(t *testing.T) {
	const ra, rb = "github.com/acme/a", "github.com/acme/b"
	b := New(Config{KeepActivities: 6})
	recordIn(b, ra, ra, rb, ra, rb, rb, ra, rb) // seqs 1 to 8; 1 and 2, both in a, are let go of
	data, _, err := b.Snapshot(t0)
	if err != nil {
		t.Fatal(err)
	}
	restored := New(Config{KeepActivities: 6})
	if err := restored.Restore(data); err != nil {
		t.Fatal(err)
	}
	shorter := New(Config{KeepActivities: 3}) // keeps 6, 7 and 8
	if err := shorter.Restore(data); err != nil {
		t.Fatal(err)
	}
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
		{"restored: b, whose losses it cannot tell", restored, rb, 0, []uint64{3, 5, 6, 8}, false},
		{"restored: b, after what it let go of", restored, rb, 2, []uint64{3, 5, 6, 8}, true},
		{"restored shorter: a", shorter, ra, 4, []uint64{7}, false},
		{"restored shorter: a, after what it let go of", shorter, ra, 5, []uint64{7}, true},
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
