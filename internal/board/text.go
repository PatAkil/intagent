package board

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Clean makes untrusted text safe to store and to show to another agent: control
// characters become spaces, runs of whitespace collapse to one space, and the
// result is cut to at most max runes.
func Clean(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == ' ' || r == ' ' {
			space = b.Len() > 0
			continue
		}
		if n >= max {
			return strings.TrimSpace(b.String()) + "…"
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// quote presents untrusted text as an obviously quoted value.
func quote(s string) string { return strconv.Quote(s) }

// ago renders a duration since t in a short human form.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "a while"
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// since says how long ago t was, as "just now" or "12m ago".
func since(now, t time.Time) string {
	if !t.IsZero() && now.Sub(t) < 10*time.Second {
		return "just now"
	}
	return ago(now, t) + " ago"
}

// plural adds an s to a noun unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func who(c *Claim) string {
	if c.Member == "" {
		return "a teammate"
	}
	return c.Member + "'s agent"
}

func onBranch(c *Claim) string {
	if c.Branch == "" {
		return ""
	}
	return " on branch " + c.Branch
}

// listPaths joins up to n paths and says how many more there are.
func listPaths(ps []string, n int) string {
	if len(ps) <= n {
		return strings.Join(ps, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ps[:n], ", "), len(ps)-n)
}

func joinBlocks(blocks ...string) string {
	var out []string
	for _, b := range blocks {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n\n")
}
