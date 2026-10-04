package glob

import (
	"math/rand"
	"path"
	"strings"
	"testing"
)

// The matcher as it was, kept as the oracle for the one that replaced it:
// both must agree on every pattern and name, clean or not.

func oldMatch(pattern, name string) bool {
	m := oldMatcher{p: strings.Split(pattern, "/"), s: strings.Split(name, "/"), memo: map[[2]int]bool{}}
	return m.match(0, 0)
}

type oldMatcher struct {
	p, s []string
	memo map[[2]int]bool
}

func (m *oldMatcher) match(i, j int) bool {
	if i == len(m.p) {
		return true
	}
	key := [2]int{i, j}
	if v, ok := m.memo[key]; ok {
		return v
	}
	var v bool
	switch {
	case m.p[i] == "**":
		v = m.match(i+1, j) || (j < len(m.s) && m.match(i, j+1))
	case j == len(m.s):
		v = false
	default:
		ok, err := path.Match(m.p[i], m.s[j])
		v = err == nil && ok && m.match(i+1, j+1)
	}
	m.memo[key] = v
	return v
}

func oldOverlap(a, b string) bool {
	o := oldOverlapper{a: strings.Split(a, "/"), b: strings.Split(b, "/"), memo: map[[2]int]bool{}}
	return o.overlap(0, 0)
}

type oldOverlapper struct {
	a, b []string
	memo map[[2]int]bool
}

func (o *oldOverlapper) overlap(i, j int) bool {
	if i == len(o.a) || j == len(o.b) {
		return true
	}
	key := [2]int{i, j}
	if v, ok := o.memo[key]; ok {
		return v
	}
	var v bool
	switch {
	case o.a[i] == "**":
		v = o.overlap(i+1, j) || o.overlap(i, j+1)
	case o.b[j] == "**":
		v = o.overlap(i, j+1) || o.overlap(i+1, j)
	default:
		v = segmentsOverlap(o.a[i], o.b[j]) && o.overlap(i+1, j+1)
	}
	o.memo[key] = v
	return v
}

func oldLiteralDir(pattern string) string {
	segs := strings.Split(pattern, "/")
	n := 0
	for n < len(segs) && !hasMeta(segs[n]) {
		n++
	}
	return strings.Join(segs[:n], "/")
}

var (
	patternSegments = []string{"a", "b", "ab", "svc", "x.go", "*", "**", "?", "a*", "*b", "a?c", "[ab]", "[^a]", "[a-c]*", `\*`,
		"[", "a[", "", "***", "**a", "]", "*.go", "a**", `\`}
	nameSegments = []string{"a", "b", "ab", "abc", "svc", "x.go", "y.go", "", "*", "[", "]", `\`, "ac", "bb"}
)

func randomPath(rng *rand.Rand, segs []string) string {
	n := rng.Intn(7)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = segs[rng.Intn(len(segs))]
	}
	return strings.Join(parts, "/")
}

func TestMatchAgreesWithTheOracle(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	matched := 0
	for range 300_000 {
		p, n := randomPath(rng, patternSegments), randomPath(rng, nameSegments)
		got, want := Match(p, n), oldMatch(p, n)
		if got != want {
			t.Fatalf("Match(%q, %q) = %v, the old matcher says %v", p, n, got, want)
		}
		if got {
			matched++
		}
	}
	if matched < 10_000 {
		t.Fatalf("only %d of the random pairs match: the test says little", matched)
	}
}

// Every pattern of up to five characters from a small alphabet against
// every name of up to four.
func TestMatchAgreesWithTheOracleExhaustively(t *testing.T) {
	t.Parallel()
	patterns := allStrings("a*/?[", 5)
	names := allStrings("ab/", 4)
	for _, p := range patterns {
		for _, n := range names {
			if got, want := Match(p, n), oldMatch(p, n); got != want {
				t.Fatalf("Match(%q, %q) = %v, the old matcher says %v", p, n, got, want)
			}
		}
	}
}

// allStrings lists every string of up to n characters from alphabet.
func allStrings(alphabet string, n int) []string {
	out := []string{""}
	for prev := out; n > 0; n-- {
		var next []string
		for _, s := range prev {
			for _, c := range alphabet {
				next = append(next, s+string(c))
			}
		}
		out, prev = append(out, next...), next
	}
	return out
}

func TestOverlapAndLiteralDirAgreeWithTheOracle(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(2))
	for range 100_000 {
		a, b := randomPath(rng, patternSegments), randomPath(rng, patternSegments)
		if got, want := Overlap(a, b), oldOverlap(a, b); got != want {
			t.Fatalf("Overlap(%q, %q) = %v, the old overlapper says %v", a, b, got, want)
		}
		if got, want := LiteralDir(a), oldLiteralDir(a); got != want {
			t.Fatalf("LiteralDir(%q) = %q, was %q", a, got, want)
		}
	}
}

// What the board matches on every edit: intents against paths, and areas
// against the directories intents are rooted in.
var realistic = [][2]string{
	{"services/payments/**", "services/payments/retry.go"},
	{"services/payments/**", "services/billing/client.go"},
	{"services/payments/retry.go", "services/payments/retry.go"},
	{"services/payments/retry.go", "services/payments/retry_test.go"},
	{"services/*/client.go", "services/billing/client.go"},
	{"services/*/client.go", "services/billing/api/client.go"},
	{"**/*.proto", "api/v1/payments.proto"},
	{"**/*.proto", "services/payments/retry.go"},
	{"docs/*.md", "docs/guide.md"},
	{"services/payments", "services"},
	{"services", "services/payments"},
	{"libs/*/src/**", "libs/common/src/strings/trim.go"},
}

func TestMatchDoesNotAllocate(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		for _, pn := range realistic {
			Match(pn[0], pn[1])
		}
		LiteralDir("services/*/client.go")
	})
	if allocs != 0 {
		t.Fatalf("matching %d realistic pairs made %.0f allocations", len(realistic), allocs)
	}
}

func BenchmarkMatch(b *testing.B) {
	for _, m := range []struct {
		name  string
		match func(p, n string) bool
	}{{"old", oldMatch}, {"new", Match}} {
		b.Run(m.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				for _, pn := range realistic {
					m.match(pn[0], pn[1])
				}
			}
		})
	}
}

// A segment pattern of literals and stars matches as path.Match matches it:
// every such pattern of up to six characters against every name of up to six.
func TestStarSegmentsMatchAsPathMatch(t *testing.T) {
	t.Parallel()
	names := allStrings("ab.", 6)
	for _, p := range allStrings("ab*", 6) {
		for _, n := range names {
			want, err := path.Match(p, n)
			if got := segmentMatch(p, n); err != nil || got != want {
				t.Fatalf("segmentMatch(%q, %q) = %v, path.Match says %v, %v", p, n, got, want, err)
			}
		}
	}
}
