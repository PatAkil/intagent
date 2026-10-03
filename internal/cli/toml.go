package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// tomlDoc edits a TOML file line by line, as Codex's config.toml needs: it
// finds a table by its key, reads or sets a key in it, and adds a table at the
// end, leaving every other line as it found it. It reads the rest of the file
// only to tell headers and keys from comments and multi-line values, and it
// refuses to add a table the file may already define another way, which would
// leave a file Codex cannot read.
type tomlDoc struct{ lines []string }

func readTOMLDoc(p string) (*tomlDoc, os.FileMode, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &tomlDoc{}, 0o600, nil
	}
	if err != nil {
		return nil, 0, err
	}
	perm := os.FileMode(0o600)
	if fi, err := os.Stat(p); err == nil {
		perm = fi.Mode().Perm()
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return &tomlDoc{}, perm, nil
	}
	return &tomlDoc{lines: strings.Split(text, "\n")}, perm, nil
}

func (d *tomlDoc) String() string {
	if len(d.lines) == 0 {
		return ""
	}
	return strings.Join(d.lines, "\n") + "\n"
}

type lineKind int

const (
	lineOther      lineKind = iota // blank, a comment, or within a multi-line value
	lineTable                      // [a.b]
	lineArrayTable                 // [[a.b]]
	lineKey                        // a.b = value
	lineUnknown                    // a line this cannot read
)

// tomlLine is what one line holds: a table's key, or a key and its value
// (without a trailing comment, when the value is a one-line string).
type tomlLine struct {
	kind  lineKind
	path  []string
	value string
}

// scan classifies the lines. The second result is false when the file ends
// inside a multi-line value.
func (d *tomlDoc) scan() ([]tomlLine, bool) {
	out := make([]tomlLine, len(d.lines))
	open, depth := "", 0 // a multi-line string's delimiter, or an open value's bracket depth
	for i, raw := range d.lines {
		l := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		switch {
		case open != "":
			if strings.Contains(l, open) {
				open = ""
			}
		case depth > 0:
			var ok bool
			if depth, ok = bracketDepth(l, depth); !ok {
				out[i].kind, depth = lineUnknown, 0
			}
		case l == "" || l[0] == '#':
		case strings.HasPrefix(l, "["):
			out[i] = header(l)
		default:
			out[i] = keyLine(l)
			v := out[i].value
			switch {
			case out[i].kind != lineKey:
			case strings.HasPrefix(v, `"""`) || strings.HasPrefix(v, "'''"):
				if !strings.Contains(v[3:], v[:3]) {
					open = v[:3]
				}
			case strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{"):
				var ok bool
				if depth, ok = bracketDepth(v, 0); !ok {
					out[i].kind, depth = lineUnknown, 0
				}
			}
			out[i].value = oneLineValue(v)
		}
	}
	return out, open == "" && depth == 0
}

// header reads a [table] or [[array of tables]] line.
func header(l string) tomlLine {
	kind, open, closing := lineTable, "[", "]"
	if strings.HasPrefix(l, "[[") {
		kind, open, closing = lineArrayTable, "[[", "]]"
	}
	path, rest, ok := parseKey(l[len(open):])
	if !ok || !strings.HasPrefix(rest, closing) || !endsLine(rest[len(closing):]) {
		return tomlLine{kind: lineUnknown}
	}
	return tomlLine{kind: kind, path: path}
}

// keyLine reads a key = value line.
func keyLine(l string) tomlLine {
	path, rest, ok := parseKey(l)
	if !ok || !strings.HasPrefix(rest, "=") {
		return tomlLine{kind: lineUnknown}
	}
	return tomlLine{kind: lineKey, path: path, value: strings.TrimSpace(rest[1:])}
}

func endsLine(rest string) bool {
	rest = strings.TrimSpace(rest)
	return rest == "" || rest[0] == '#'
}

// oneLineValue drops a comment after a one-line basic string.
func oneLineValue(v string) string {
	if !strings.HasPrefix(v, `"`) || strings.HasPrefix(v, `"""`) {
		return v
	}
	for i := 1; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case '"':
			return v[:i+1]
		}
	}
	return v
}

// bracketDepth follows the brackets and braces of a value over one line,
// skipping strings and a trailing comment, from depth. It gives up on a
// multi-line string inside the value.
func bracketDepth(s string, depth int) (int, bool) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case '#':
			return depth, depth >= 0
		case '"', '\'':
			if strings.HasPrefix(s[i:], strings.Repeat(string(c), 3)) {
				return depth, false
			}
			j := i + 1
			for j < len(s) && s[j] != c {
				if c == '"' && s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return depth, false
			}
			i = j
		}
	}
	return depth, depth >= 0
}

// table returns the index of the [path] header and the end of its table, or -1, -1.
func table(lines []tomlLine, path []string) (int, int) {
	for i, l := range lines {
		if l.kind == lineTable && slices.Equal(l.path, path) {
			end := i + 1
			for end < len(lines) && lines[end].kind != lineTable && lines[end].kind != lineArrayTable {
				end++
			}
			return i, end
		}
	}
	return -1, -1
}

// headerPath is the key of a "[a.b]" header intagent writes.
func headerPath(h string) []string {
	path, _ := splitKey(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
	return path
}

// get returns a key's value in a table, or "".
func (d *tomlDoc) get(header, key string) string {
	lines, _ := d.scan()
	i, end := table(lines, headerPath(header))
	for j := i + 1; i >= 0 && j < end; j++ {
		if lines[j].kind == lineKey && slices.Equal(lines[j].path, []string{key}) {
			return lines[j].value
		}
	}
	return ""
}

// set sets a key in a table, adding the table or the key if needed, and
// returns the value it had. It refuses rather than add a table the file may
// already define another way.
func (d *tomlDoc) set(header, key, value string) (string, error) {
	lines, complete := d.scan()
	path := headerPath(header)
	i, end := table(lines, path)
	if i < 0 {
		if !complete || defines(lines, path) {
			return "", fmt.Errorf("it already has a table like %s that intagent cannot edit safely; "+
				"set %s = %s in it yourself", header, key, value)
		}
		d.lines = append(d.lines, "", header, key+" = "+value)
		return "", nil
	}
	for j := i + 1; j < end; j++ {
		if lines[j].kind == lineKey && slices.Equal(lines[j].path, []string{key}) {
			if lines[j].value != value {
				d.lines[j] = key + " = " + value
			}
			return lines[j].value, nil
		}
	}
	d.lines = slices.Insert(d.lines, i+1, key+" = "+value)
	return "", nil
}

// defines reports whether the file may already define the table at path
// without a [path] header: with a key above it or within it (projects =
// { ... }, or projects."/repo".trust_level = ...), as an array of tables, or
// on a line this cannot read.
func defines(lines []tomlLine, path []string) bool {
	related := func(p []string) bool {
		n := min(len(p), len(path))
		return slices.Equal(p[:n], path[:n])
	}
	var current []string
	for _, l := range lines {
		switch l.kind {
		case lineUnknown:
			return true
		case lineTable:
			current = l.path
		case lineArrayTable:
			if related(l.path) {
				return true
			}
			current = []string{"\x00"} // an element of an array: matches nothing
		case lineKey:
			if related(append(slices.Clone(current), l.path...)) {
				return true
			}
		}
	}
	return false
}

// splitKey splits a TOML dotted key into its unquoted parts.
func splitKey(s string) ([]string, bool) {
	parts, rest, ok := parseKey(s)
	return parts, ok && rest == ""
}

// parseKey reads the dotted key at the start of s and returns its unquoted
// parts and the rest of s, trimmed.
func parseKey(s string) ([]string, string, bool) {
	var parts []string
	s = strings.TrimSpace(s)
	for {
		part, rest, ok := keyPart(s)
		if !ok {
			return nil, "", false
		}
		parts = append(parts, part)
		s = strings.TrimSpace(rest)
		if !strings.HasPrefix(s, ".") {
			return parts, s, true
		}
		s = strings.TrimSpace(s[1:])
	}
}

// keyPart reads one bare or quoted key at the start of s.
func keyPart(s string) (string, string, bool) {
	switch {
	case strings.HasPrefix(s, `"`):
		end := 1
		for end < len(s) && s[end] != '"' {
			if s[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(s) {
			return "", "", false
		}
		u, err := strconv.Unquote(s[:end+1])
		return u, s[end+1:], err == nil
	case strings.HasPrefix(s, "'"):
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		return s[1 : end+1], s[end+2:], true
	}
	end := strings.IndexFunc(s, func(r rune) bool { return !bareKeyRune(r) })
	if end < 0 {
		end = len(s)
	}
	return s[:end], s[end:], end > 0
}

// bareKeyRune reports whether r may appear in an unquoted TOML key.
func bareKeyRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}
