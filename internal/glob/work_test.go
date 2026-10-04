package glob

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// segments joins n copies of seg.
func segments(n int, seg string) string { return strings.TrimSuffix(strings.Repeat(seg+"/", n), "/") }

// Paths and patterns are accepted up to the bounds on their shape, and
// refused past them.
func TestCleanBoundsTheShape(t *testing.T) {
	t.Parallel()
	long, wild := strings.Repeat("a", MaxSegmentLen), "*"+strings.Repeat("a", MaxWildSegmentLen-1)
	for _, p := range []string{segments(MaxPathSegments, "a"), long + "/" + long, "./" + segments(MaxPathSegments, "a")} {
		if _, err := CleanPath(p); err != nil {
			t.Errorf("CleanPath(%.30q…): %v", p, err)
		}
	}
	for _, p := range []string{segments(MaxPathSegments+1, "a"), long + "a/b", "b/" + long + "a"} {
		if got, err := CleanPath(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPath(%.30q…) = %.30q…, %v; want ErrInvalid", p, got, err)
		}
	}
	for _, p := range []string{segments(MaxPatternSegments, "a"), segments(MaxDoubleStars, "**/a"), wild + "/" + long,
		segments(MaxPatternSegments-1, "a") + "/", "**/**/" + segments(MaxDoubleStars-1, "**/a")} {
		if _, err := CleanPattern(p); err != nil {
			t.Errorf("CleanPattern(%.30q…): %v", p, err)
		}
	}
	for _, p := range []string{segments(MaxPatternSegments+1, "a"), segments(MaxDoubleStars+1, "**/a"), wild + "a",
		"a/" + wild + "?/b", segments(MaxPatternSegments, "a") + "/", long + "a/**"} {
		if got, err := CleanPattern(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPattern(%.30q…) = %.30q…, %v; want ErrInvalid", p, got, err)
		}
	}
}

// Matching under a budget gives Match's and Overlap's answers while the
// budget lasts.
func TestWorkAgreesWithMatchAndOverlap(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(9))
	w := NewWork(1 << 40)
	for range 100_000 {
		p, q, n := randomPath(rng, patternSegments, 7), randomPath(rng, patternSegments, 7), randomPath(rng, nameSegments, 7)
		if got, want := w.Match(p, n), Match(p, n); got != want {
			t.Fatalf("Work.Match(%q, %q) = %v, Match says %v", p, n, got, want)
		}
		if got, want := w.Overlap(p, q), Overlap(p, q); got != want {
			t.Fatalf("Work.Overlap(%q, %q) = %v, Overlap says %v", p, q, got, want)
		}
	}
	if w.Short() {
		t.Fatal("the budget ran out")
	}
}

// A comparison of two segments costs a unit, and one with wildcards
// len(p)·len(s)/32 more.
func TestWorkCountsComparisons(t *testing.T) {
	t.Parallel()
	star := "*" + strings.Repeat("x", 63)
	name := "y" + strings.Repeat("x", 63)
	for _, tc := range []struct {
		pattern, name string
		units         int
	}{
		{"a/b/c.go", "a/b/c.go", 1},     // a prefix, compared once
		{"a/b/**", "a/b/c/d.go", 1},     // likewise
		{"a/*/c.go", "a/b/c.go", 3},     // a, then * against b, then c.go
		{"a/*/c.go", "x/b/c.go", 1},     // a fails at once
		{"**/c.go", "a/b/c.go", 3},      // c.go against a, b and c.go
		{star + "/z", name + "/z", 130}, // 1 + 64·64/32, then 1
		{"**", "a/b/c.go", 1},           // it covers every name, but costs a unit
	} {
		w := NewWork(1000)
		w.Match(tc.pattern, tc.name)
		if used := 1000 - w.Left(); used != tc.units {
			t.Errorf("Match(%.20q, %.20q) cost %d units, want %d", tc.pattern, tc.name, used, tc.units)
		}
	}
	for _, tc := range []struct {
		a, b  string
		units int
	}{
		{"a/b", "a/c", 2},                    // a, then b against c
		{"**/x.go", "a/b", 1},                // ** against a: 1 + 2·1/32
		{"a/**", strings.Repeat("b", 64), 1}, // a against b…: no wildcard
		{"**", strings.Repeat("b", 64), 5},   // 1 + 2·64/32
	} {
		w := NewWork(1000)
		w.Overlap(tc.a, tc.b)
		if used := 1000 - w.Left(); used != tc.units {
			t.Errorf("Overlap(%.20q, %.20q) cost %d units, want %d", tc.a, tc.b, used, tc.units)
		}
	}
}

// Once the budget is spent, matching stops: no call costs more than is
// left, every answer after is false, and the same calls stop at the same
// place on every run.
func TestWorkStopsWhenSpent(t *testing.T) {
	t.Parallel()
	hostile := segments(7, "**/"+segments(7, "*")) + "/zz"
	name := segments(MaxPathSegments-1, "a") + "/zz"
	run := func() (answers []bool, left int) {
		w := NewWork(100_000)
		for range 1000 {
			answers = append(answers, w.Match(hostile, name), w.Match("a/**", name), w.Overlap("a/*/b", "a/x/b"),
				w.Match("**", name), w.Overlap("**/b", "a/b"))
		}
		if !w.Short() || w.Left() < 0 {
			t.Fatalf("after 1000 hostile matches the budget is not spent: %d left", w.Left())
		}
		return answers, w.Left()
	}
	a1, l1 := run()
	a2, l2 := run()
	if l1 != l2 || len(a1) != len(a2) {
		t.Fatalf("two runs stopped differently: %d and %d left", l1, l2)
	}
	stopped := -1
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("two runs answered call %d differently", i)
		}
		if i%5 == 1 && !a1[i] && stopped < 0 {
			stopped = i
		}
	}
	if stopped < 0 {
		t.Fatal("a match the budget could not pay for was answered true")
	}
	for i := stopped; i < len(a1); i++ {
		if a1[i] {
			t.Fatalf("call %d, after the budget ran out at call %d, answered true", i, stopped)
		}
	}
}
