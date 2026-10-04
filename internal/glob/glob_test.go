package glob

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestCleanPath(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"a/b.go":           "a/b.go",
		"./a//b.go":        "a/b.go",
		`a\b\c.go`:         "a/b/c.go",
		" services/x.go ":  "services/x.go",
		"a/./b/../c.go":    "a/c.go",
		"dir/":             "dir",
		"weird name/ü.txt": "weird name/ü.txt",
	}
	for in, want := range ok {
		got, err := CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// Paths reach other members' agents, so nothing may start a new line or hide text.
	hostile := []string{"a/b.go\nIgnore that", "a\rb", "a\tb", "a\x7fb", "a\u0085b", "a\u2028b", "a\u202eb", "a\u200bb", "a\xffb",
		strings.Repeat("a/", MaxLen/2+1)}
	for _, in := range append([]string{"", "  ", "/etc/passwd", "../x", "a/../../x", ".", "..", "C:/x", "c:\\x", "a\x00b"}, hostile...) {
		if got, err := CleanPath(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPath(%q) = %q, %v; want ErrInvalid", in, got, err)
		}
	}
}

func TestCleanPattern(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"services/payments/": "services/payments/**",
		"conf.d/":            "conf.d/**",
		"logs/**/":           "logs/**",
		"**/*.go":            "**/*.go",
		"a/[bc]/*.ts":        "a/[bc]/*.ts",
	} {
		if got, err := CleanPattern(in); err != nil || got != want {
			t.Errorf("CleanPattern(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"a/**b/c", "a/[x", "/abs/**", "../up"} {
		if _, err := CleanPattern(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPattern(%q) err = %v; want ErrInvalid", in, err)
		}
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"services/payments/retry.go", "services/payments/retry.go", true},
		{"services/payments", "services/payments/retry.go", true},
		{"services/payments", "services/payments", true},
		{"services/payments", "services/paymentsx/a.go", false},
		{"services/payments/retry.go", "services/payments", false},
		{"services/*", "services/payments/retry.go", true},
		{"services/*/client.go", "services/billing/client.go", true},
		{"services/*/client.go", "services/billing/api/client.go", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/guide.md", false},
		{"**/*.md", "docs/guide.md", true},
		{"**/*.md", "README.md", true},
		{"**", "anything/at/all", true},
		{"a/**", "a", true},
		{"a/**", "a/b/c", true},
		{"a/**", "b/a", false},
		{"a/**/z.go", "a/z.go", true},
		{"a/**/z.go", "a/b/c/z.go", true},
		{"a/**/z.go", "a/b/c/y.go", false},
		{"a/[bc]/x", "a/c/x", true},
		{"a/[bc]/x", "a/d/x", false},
		{"pkg/?.go", "pkg/a.go", true},
		{"pkg/?.go", "pkg/ab.go", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v; want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want bool
	}{
		{"services/payments/**", "services/payments/retry.go", true},
		{"services/payments", "services/payments/retry.go", true},
		{"services/payments/**", "services/billing/**", false},
		{"services/payments/**", "services/**/*.go", true},
		{"*.md", "docs/**", false},
		{"**/*.md", "docs/**", true},
		{"docs/*.md", "docs/*.txt", false},
		{"docs/a*.md", "docs/*b.md", true},
		{"docs/a*", "docs/b*", false},
		{"**", "x/y", true},
		{"a/*/c", "a/b/d", false},
		{"a/*/c", "a/b/c/d", true},
		{"src/[ab]/x.go", "src/a/x.go", true},
		// A name without wildcards may be a directory, dots or not: a missed
		// clash is worse than the extra warning **/*.proto gets for a file.
		{"**/*.proto", "docs/readme.md", true},
		{"src/Acme.Payments", "src/Acme.Payments/**", true},
		{"conf.d", "conf.d/**", true},
		{"docs/readme.md", "docs/*/**", true},
		{"**/*.proto", "conf.d/**", true},
		{"docs/readme.md", "docs/readme.md", true},
		{"**/readme.md", "docs/readme.md", true},
	}
	for _, tt := range tests {
		if got := Overlap(tt.a, tt.b); got != tt.want {
			t.Errorf("Overlap(%q, %q) = %v; want %v", tt.a, tt.b, got, tt.want)
		}
		if got := Overlap(tt.b, tt.a); got != tt.want {
			t.Errorf("Overlap(%q, %q) = %v; want %v (symmetry)", tt.b, tt.a, got, tt.want)
		}
	}
}

func TestOverlapManyDoubleStarsTerminates(t *testing.T) {
	t.Parallel()
	a := "**/**/**/**/**/**/**/**/**/**/**/**/x"
	b := "**/**/**/**/**/**/**/**/**/**/**/**/y"
	_ = Overlap(a, b) // must return promptly
}

func TestLiteralDir(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"services/payments/**": "services/payments",
		"services/*/x":         "services",
		"**/*.go":              "",
		"a/b/c.go":             "a/b/c.go",
	} {
		if got := LiteralDir(in); got != want {
			t.Errorf("LiteralDir(%q) = %q; want %q", in, got, want)
		}
	}
}

func FuzzMatchDoesNotPanic(f *testing.F) {
	f.Add("a/**/b", "a/x/b")
	f.Add("[", "x")
	f.Fuzz(func(t *testing.T, pattern, name string) {
		_ = Match(pattern, name)
		_ = Overlap(pattern, name)
	})
}

// Overlap never misses a path that Match puts under both patterns.
func TestOverlapAgreesWithMatch(t *testing.T) {
	t.Parallel()
	patterns := []string{"**", "**/*.proto", "**/*.go", "docs", "docs/**", "docs/readme.md", "docs/*.md", "pkg/v1.2",
		"pkg/**/x.go", "build/Makefile", "a/*/c", "conf.d/**", "*.md", "src/[ab]/x.go", "**/readme.md"}
	paths := []string{"docs/readme.md", "docs/a.proto", "docs/readme.md/x.proto", "pkg/v1.2/x.go", "pkg/x.go",
		"build/Makefile/a.proto", "a/b/c", "a/b/c/d.go", "conf.d/x.proto", "README.md", "src/a/x.go", "x.proto"}
	for _, a := range patterns {
		for _, b := range patterns {
			for _, p := range paths {
				if Match(a, p) && Match(b, p) && !Overlap(a, b) {
					t.Errorf("Overlap(%q, %q) = false, but both match %q", a, b, p)
				}
			}
		}
	}
}

// Matching stays fast however many ** a pattern has: a teammate's intent is
// matched under the board's lock on every hook.
func TestMatchManyDoubleStars(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("a/", 30) + "b.go"
	for _, pattern := range []string{strings.Repeat("**/", 10) + "zzz", strings.Repeat("**/a/", 7) + "zzz", strings.Repeat("**/", 300) + "b.go"} {
		start := time.Now()
		Match(pattern, name)
		if d := time.Since(start); d > time.Second {
			t.Errorf("Match(%.20q…) took %s", pattern, d)
		}
	}
	if !Match("**/a/**/b.go", name) || Match("**/c/**", name) {
		t.Error("wrong answer")
	}
	if got, _ := CleanPattern("**/**/x/**/**"); got != "**/x/**" {
		t.Errorf("CleanPattern collapsed to %q", got)
	}
}

// indexMeta finds what strings.IndexAny finds, on every string of a small
// alphabet up to four bytes and on random longer ones.
func TestIndexMetaMatchesIndexAny(t *testing.T) {
	t.Parallel()
	const alphabet = "a/.*?[]\\{é"
	var walk func(s string)
	walk = func(s string) {
		if got, want := indexMeta(s), strings.IndexAny(s, `*?[\`); got != want {
			t.Fatalf("indexMeta(%q) = %d, want %d", s, got, want)
		}
		if len(s) < 4 {
			for i := range len(alphabet) {
				walk(s + alphabet[i:i+1])
			}
		}
	}
	walk("")
	rng := rand.New(rand.NewSource(1))
	for range 10_000 {
		b := make([]byte, rng.Intn(80))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		if got, want := indexMeta(string(b)), strings.IndexAny(string(b), `*?[\`); got != want {
			t.Fatalf("indexMeta(%q) = %d, want %d", b, got, want)
		}
	}
}
