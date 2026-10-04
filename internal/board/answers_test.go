package board

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A collision names the other teammates it ran into in a few lines: one
// for the same news of a member's many worktrees, at most four, and one
// that counts the rest. A fleet on one file used to add a line per worktree
// to every stream, the feed, the snapshot and the webhook's message.
func TestCollisionNamesAFewTeammates(t *testing.T) {
	h := newHarness(t)
	for i := range 6 {
		h.at(KindPostEdit, fmt.Sprintf("dev%d", i), "w", fmt.Sprint("d", i), "svc/x.go")
	}
	h.advance(time.Second) // a fleet's worktrees change it last
	for i := range 30 {
		h.at(KindPostEdit, "bot", fmt.Sprintf("w%02d", i), fmt.Sprint("b", i), "svc/x.go")
	}
	h.advance(time.Second)
	if res := h.hook(KindPreEdit, "alice", "a1", "svc/x.go"); res.Decision != DecisionRefuse {
		t.Fatalf("alice's edit: %s", res.Decision)
	}
	acts := h.activities(ActivityConflict)
	if len(acts) != 1 {
		t.Fatalf("%d conflicts announced", len(acts))
	}
	also := acts[0].Also
	t.Logf("%s; also %s", acts[0].Text, strings.Join(also, "; "))
	if len(also) != maxAlso+1 || !strings.HasSuffix(also[0], "(in 29 worktrees)") || also[maxAlso] != "3 more" {
		t.Fatalf("also: %q; want bot's 29 other worktrees in one line, three teammates, and 3 more", also)
	}
}

// An edit's answer lists the conflicts that block or overlap, up to 200,
// and the 20 newest of those nearby, and counts the rest: an edit of many
// files in a busy area met thousands, which went back in every answer.
func TestEditAnswersListBoundedConflicts(t *testing.T) {
	h := newHarness(t)
	files := make([]string, 100)
	for i := range files {
		files[i] = fmt.Sprintf("svc/f%03d.go", i)
	}
	for i := range 3 {
		h.at(KindPostEdit, fmt.Sprintf("dev%d", i), "w", "d", files...)
	}
	for i := range 30 { // working in the same area, on other files
		h.advance(time.Second)
		h.at(KindPostEdit, fmt.Sprintf("near%02d", i), "w", "n", fmt.Sprintf("svc/other%02d.go", i))
	}
	h.advance(time.Second)
	all, err := h.b.Check(h.now, CheckRequest{Member: "alice", Where: whereOf("alice"), Paths: refs(files...)})
	if err != nil {
		t.Fatal(err)
	}
	res := h.hook(KindPreEdit, "alice", "a1", files...)
	var overlaps, nearby []Conflict
	for _, cf := range res.Conflicts {
		if cf.Severity == SeverityNearby {
			nearby = append(nearby, cf)
		} else {
			overlaps = append(overlaps, cf)
		}
	}
	t.Logf("%d conflicts; the answer lists %d overlaps and %d nearby, and counts %d more",
		len(all.Conflicts), len(overlaps), len(nearby), res.MoreConflicts)
	if len(overlaps) != maxAnswerConflicts || len(nearby) != maxNearbyConflicts || res.MoreConflicts != len(all.Conflicts)-len(res.Conflicts) {
		t.Fatalf("listed %d overlaps and %d nearby and counted %d more, of %d", len(overlaps), len(nearby), res.MoreConflicts, len(all.Conflicts))
	}
	// The nearby ones listed are the newest: the last ten teammates' work.
	for _, cf := range nearby {
		if n := cf.Member; n < "near20" {
			t.Errorf("an older teammate's nearby work is listed: %s", n)
		}
	}
	if !slices.IsSortedFunc(res.Conflicts, func(a, b Conflict) int { return strings.Compare(a.Path, b.Path) }) {
		t.Error("the conflicts listed are not in the order of the paths they are about")
	}
}
