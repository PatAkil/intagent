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
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalid reports a pattern or path that cannot be used.
var ErrInvalid = errors.New("invalid path or pattern")

// MaxLen bounds a path or pattern, in bytes.
const MaxLen = 1024

// Bounds on the shape of paths and patterns, which keep the work of matching
// one against the other small: the board matches its teammates' patterns
// against every path an agent edits, under its lock. Real paths are far
// within them; the deepest seen are about 23 directories deep, and no file
// system allows a longer name than 255 bytes.
const (
	MaxPathSegments    = 64  // segments of a path
	MaxPatternSegments = 32  // segments of a pattern
	MaxDoubleStars     = 4   // ** segments of a pattern
	MaxSegmentLen      = 255 // bytes of a segment
	MaxWildSegmentLen  = 64  // bytes of a pattern segment with wildcards
)

// CleanPath normalises a repo-relative file path: forward slashes, no leading
// "./", no redundant separators. It rejects empty, absolute, escaping and
// overlong paths, and paths with control or invisible formatting characters
// or invalid UTF-8: paths are shown to other members' agents, and must not be
// able to start a line of their own.
func CleanPath(p string) (string, error) {
	if len(p) > MaxLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalid, MaxLen)
	}
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: %q is not valid UTF-8", ErrInvalid, p)
	}
	for _, r := range p {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", fmt.Errorf("%w: %q contains a control, formatting or line separator character", ErrInvalid, p)
		}
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
	if n := strings.Count(c, "/") + 1; n > MaxPathSegments {
		return "", fmt.Errorf("%w: %.40q… has %d segments, more than %d", ErrInvalid, c, n, MaxPathSegments)
	}
	for rest := c; rest != ""; {
		var seg string
		seg, rest, _ = strings.Cut(rest, "/")
		if len(seg) > MaxSegmentLen {
			return "", fmt.Errorf("%w: %.40q… has a segment longer than %d bytes", ErrInvalid, c, MaxSegmentLen)
		}
	}
	return c, nil
}

// CleanPattern normalises a pattern the way CleanPath does and checks its
// syntax and shape: at most MaxPatternSegments segments, MaxDoubleStars of
// them **, and segments with wildcards of at most MaxWildSegmentLen bytes. A
// trailing slash is dropped, since every pattern covers the directories it
// matches.
func CleanPattern(p string) (string, error) {
	p = strings.TrimSpace(p)
	dir := strings.HasSuffix(p, "/")
	c, err := CleanPath(strings.TrimSuffix(p, "/"))
	if err != nil {
		return "", err
	}
	if dir && !strings.HasSuffix(c, "**") {
		// Said to be a directory: keep that, even for a name like conf.d.
		c += "/**"
	}
	segs := strings.Split(c, "/")
	// "**/**" means "**"; keeping one makes matching cheaper.
	segs = slices.CompactFunc(segs, func(a, b string) bool { return a == "**" && b == "**" })
	c = strings.Join(segs, "/")
	if len(segs) > MaxPatternSegments {
		return "", fmt.Errorf("%w: %.40q… has %d segments, more than %d", ErrInvalid, c, len(segs), MaxPatternSegments)
	}
	stars := 0
	for _, seg := range segs {
		if seg == "**" {
			stars++
			continue
		}
		if strings.Contains(seg, "**") {
			return "", fmt.Errorf("%w: %q: ** must be a whole segment", ErrInvalid, p)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return "", fmt.Errorf("%w: %q: %w", ErrInvalid, p, err)
		}
		if hasMeta(seg) && len(seg) > MaxWildSegmentLen {
			return "", fmt.Errorf("%w: %.40q…: a segment with wildcards is longer than %d bytes", ErrInvalid, c, MaxWildSegmentLen)
		}
	}
	if stars > MaxDoubleStars {
		return "", fmt.Errorf("%w: %.40q… has %d ** segments, more than %d", ErrInvalid, c, stars, MaxDoubleStars)
	}
	return c, nil
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z'
}

// Match reports whether pattern covers the file or directory at name. Both
// must already be clean; an invalid pattern matches nothing.
//
// It runs on every edit for every intent and footprint it meets, so it
// allocates nothing: a literal pattern, or one of the form dir/**, is a
// prefix comparison, and any other pattern is matched segment by segment in
// place.
func Match(pattern, name string) bool { return match(pattern, name, nil) }

// match is Match, charged to w when w is not nil.
func match(pattern, name string, w *Work) bool {
	if !hasMeta(pattern) {
		return w.spend(1) && covers(pattern, name)
	}
	if dir, ok := strings.CutSuffix(pattern, "/**"); ok && !hasMeta(dir) {
		return w.spend(1) && covers(dir, name)
	}
	if pattern == "**" {
		return w.spend(1) // it covers every name, and matching it compares nothing
	}
	return matchSegments(pattern, name, w)
}

// covers reports whether dir, a pattern without wildcards, is name or a
// directory above it. The empty pattern covers only the empty name, among
// clean ones.
func covers(dir, name string) bool {
	return strings.HasPrefix(name, dir) && (len(name) == len(dir) || name[len(dir)] == '/')
}

// matchSegments matches pattern against name a segment at a time. When a
// segment fails, it lets the latest ** take one more segment of name and
// tries again from there. Backtracking to the latest ** alone is enough, as
// in wildcard matching with *: every other segment matches exactly one
// segment of name, so taking the first place where the segments between two
// ** match leaves the most of name for the rest. Positions are byte offsets
// to the start of a segment, past the end once a string is used up.
func matchSegments(pattern, name string, w *Work) bool {
	pi, ni := 0, 0
	starP, starN := -1, -1 // the pattern after the latest **, and the next segment of name it would take
	for {
		if pi > len(pattern) {
			// The pattern is used up: it matched name itself or a directory above it.
			return true
		}
		pe := segmentEnd(pattern, pi)
		seg := pattern[pi:pe]
		if seg == "**" {
			pi, starP, starN = pe+1, pe+1, ni
			continue
		}
		if ni <= len(name) {
			ne := segmentEnd(name, ni)
			if w != nil && !w.spend(segmentCost(seg, name[ni:ne], hasMeta(seg))) {
				return false
			}
			if segmentMatch(seg, name[ni:ne]) {
				pi, ni = pe+1, ne+1
				continue
			}
		}
		if starP < 0 || starN > len(name) {
			return false
		}
		starN = segmentEnd(name, starN) + 1
		pi, ni = starP, starN
	}
}

// segmentEnd is where the segment of s that starts at i ends.
func segmentEnd(s string, i int) int {
	if j := strings.IndexByte(s[i:], '/'); j >= 0 {
		return i + j
	}
	return len(s)
}

// segmentMatch matches one segment as path.Match does. Most wildcard
// segments are a literal and stars, such as *.proto, which it matches
// without path.Match's general machinery.
func segmentMatch(pattern, name string) bool {
	star := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			star = true
		case '?', '[', '\\':
			ok, err := path.Match(pattern, name)
			return err == nil && ok
		}
	}
	if !star {
		return pattern == name
	}
	return starMatch(pattern, name)
}

// starMatch matches a segment pattern whose only wildcard is *: its literal
// parts must begin and end name and occur in order between, and taking each
// middle part at its first occurrence leaves the most room for the rest.
func starMatch(pattern, name string) bool {
	first, rest, _ := strings.Cut(pattern, "*")
	if !strings.HasPrefix(name, first) {
		return false
	}
	name = name[len(first):]
	middle, last := "", rest
	if i := strings.LastIndexByte(rest, '*'); i >= 0 {
		middle, last = rest[:i], rest[i+1:]
	}
	if !strings.HasSuffix(name, last) {
		return false
	}
	name = name[:len(name)-len(last)]
	for middle != "" {
		var part string
		part, middle, _ = strings.Cut(middle, "*")
		i := strings.Index(name, part)
		if i < 0 {
			return false
		}
		name = name[i+len(part):]
	}
	return true
}

// Overlap reports whether some path could be covered by both patterns. It
// errs towards true when two wildcard segments might intersect, because a
// missed overlap is worse than an extra warning.
//
// It compares the patterns a segment at a time, in step, and allocates
// nothing. Once either reaches a **, they overlap: the ** can take every
// segment the other pattern has left and then go on below it, as a pattern
// covers everything below what it matches. Once either is used up, they
// overlap too, since a name without wildcards may be a directory
// (src/Acme.Payments, conf.d). This makes **/*.proto overlap
// docs/readme.md, an extra warning taken over a missed clash.
func Overlap(a, b string) bool { return overlap(a, b, nil) }

// overlap is Overlap, charged to w when w is not nil.
func overlap(a, b string, w *Work) bool {
	for {
		as, ar, aMore := strings.Cut(a, "/")
		bs, br, bMore := strings.Cut(b, "/")
		// Charged before a ** answers, so that every comparison costs a unit.
		if w != nil && !w.spend(segmentCost(as, bs, hasMeta(as) || hasMeta(bs))) {
			return false
		}
		if as == "**" || bs == "**" {
			return true
		}
		if !segmentsOverlap(as, bs) {
			return false
		}
		if !aMore || !bMore {
			return true
		}
		a, b = ar, br
	}
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

func hasMeta(s string) bool { return indexMeta(s) >= 0 }

// indexMeta is strings.IndexAny(s, `*?[\`), which for a set of several
// bytes builds the set on every call: a check of an edit of many files calls
// it for every path and every teammate's intent.
func indexMeta(s string) int {
	for i := range len(s) {
		switch s[i] {
		case '*', '?', '[', '\\':
			return i
		}
	}
	return -1
}

func literalPrefix(s string) string {
	if i := indexMeta(s); i >= 0 {
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
	i := indexMeta(pattern)
	if i < 0 {
		return pattern
	}
	// The segment with the first wildcard starts after the slash before it.
	return pattern[:max(0, strings.LastIndexByte(pattern[:i], '/'))]
}

// Work bounds the matching a caller may do, so that how long a match takes
// does not depend on how hostile a pattern is. It is counted in units the
// same way on every run: a comparison of two segments costs one, and one
// with wildcards len(p)·len(s)/32 more, about in proportion to its time;
// every call makes at least one. Once a comparison would cost more than is
// left, Match and Overlap answer false without making it, and Short reports
// that they did.
//
// The zero Work is unbounded.
type Work struct {
	bounded bool
	left    int
	short   bool
}

// NewWork returns a budget of units.
func NewWork(units int) Work { return Work{bounded: true, left: units} }

// Short reports whether the budget ran out: an answer since was false for
// want of work, not because nothing matched.
func (w *Work) Short() bool { return w.short }

// Left reports the units left.
func (w *Work) Left() int { return w.left }

// Match is Match, charged to w.
func (w *Work) Match(pattern, name string) bool { return match(pattern, name, w) }

// Overlap is Overlap, charged to w.
func (w *Work) Overlap(a, b string) bool { return overlap(a, b, w) }

// spend takes units from w, unless w is nil or unbounded, and reports
// whether there were enough.
func (w *Work) spend(units int) bool {
	if w == nil || !w.bounded {
		return true
	}
	if units > w.left {
		w.left, w.short = 0, true
		return false
	}
	w.left -= units
	return true
}

// segmentCost is what comparing two segments costs, one of them with
// wildcards or not.
func segmentCost(a, b string, wild bool) int {
	if !wild {
		return 1
	}
	return 1 + len(a)*len(b)/32
}
