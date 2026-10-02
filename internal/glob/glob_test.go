package glob

import (
	"errors"
	"strings"
	"testing"
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
		"services/payments/": "services/payments",
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
