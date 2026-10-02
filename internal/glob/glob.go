// Package glob matches repo-relative paths against intent patterns.
//
// Patterns are slash-separated and anchored at the repository root. Within a
// segment, *, ? and [...] behave as in [path.Match]. A segment of exactly **
// matches zero or more segments. A pattern also covers everything below what
// it matches, so "services/payments" covers "services/payments/retry.go" and
// "services/*" covers every file under each direct child of services.
package glob

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ErrInvalid reports a pattern or path that cannot be used.
var ErrInvalid = errors.New("invalid path or pattern")

// CleanPath normalises a repo-relative file path: forward slashes, no leading
// "./", no redundant separators. It rejects empty, absolute and escaping
// paths and paths containing NUL.
func CleanPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q contains NUL", ErrInvalid, p)
	}
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalid)
	}
	if strings.HasPrefix(p, "/") || hasDrive(p) {
		return "", fmt.Errorf("%w: %q is absolute", ErrInvalid, p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: %q leaves the repository", ErrInvalid, p)
	}
	return c, nil
}

// CleanPattern normalises a pattern the way CleanPath does and checks its
// syntax. A trailing slash is dropped, since every pattern covers the
// directories it matches.
func CleanPattern(p string) (string, error) {
	c, err := CleanPath(strings.TrimSuffix(strings.TrimSpace(p), "/"))
	if err != nil {
		return "", err
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return "", fmt.Errorf("%w: %q: ** must be a whole segment", ErrInvalid, p)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return "", fmt.Errorf("%w: %q: %v", ErrInvalid, p, err)
		}
	}
	return c, nil
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z'
}

// Match reports whether pattern covers the file or directory at name. Both
// must already be clean; an invalid pattern matches nothing.
func Match(pattern, name string) bool {
	return match(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func match(p, s []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			for k := 0; k <= len(s); k++ {
				if match(p[1:], s[k:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if ok, err := path.Match(p[0], s[0]); err != nil || !ok {
			return false
		}
		p, s = p[1:], s[1:]
	}
	// The pattern is used up: it matched name itself or a directory above it.
	return true
}

// Overlap reports whether some path could be covered by both patterns. It
// errs towards true when two wildcard segments might intersect, because a
// missed overlap is worse than an extra warning.
func Overlap(a, b string) bool {
	o := overlapper{a: strings.Split(a, "/"), b: strings.Split(b, "/"), memo: map[[2]int]bool{}}
	return o.overlap(0, 0)
}

// overlapper searches suffixes a[i:] and b[j:]; memo makes patterns with many
// ** segments polynomial instead of exponential.
type overlapper struct {
	a, b []string
	memo map[[2]int]bool
}

func (o *overlapper) overlap(i, j int) bool {
	if i == len(o.a) || j == len(o.b) {
		// One pattern is used up, so it covers the whole subtree the other is in.
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

func segmentsOverlap(a, b string) bool {
	am, bm := hasMeta(a), hasMeta(b)
	switch {
	case !am && !bm:
		return a == b
	case !am:
		ok, _ := path.Match(b, a)
		return ok
	case !bm:
		ok, _ := path.Match(a, b)
		return ok
	}
	// Both are wildcards. They are disjoint only if their fixed prefixes or
	// fixed suffixes contradict each other.
	ap, bp := literalPrefix(a), literalPrefix(b)
	if !strings.HasPrefix(ap, bp) && !strings.HasPrefix(bp, ap) {
		return false
	}
	as, bs := literalSuffix(a), literalSuffix(b)
	return strings.HasSuffix(as, bs) || strings.HasSuffix(bs, as)
}

func hasMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }

func literalPrefix(s string) string {
	if i := strings.IndexAny(s, `*?[\`); i >= 0 {
		return s[:i]
	}
	return s
}

func literalSuffix(s string) string {
	if i := strings.LastIndexAny(s, `*?]`); i >= 0 {
		return s[i+1:]
	}
	return s
}

// LiteralDir returns the longest leading run of segments without wildcards:
// the directory a pattern is rooted in, or "" for patterns like "**/*.go".
func LiteralDir(pattern string) string {
	segs := strings.Split(pattern, "/")
	n := 0
	for n < len(segs) && !hasMeta(segs[n]) {
		n++
	}
	return strings.Join(segs[:n], "/")
}
