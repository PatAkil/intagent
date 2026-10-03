package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/patakil/intagent/internal/gitx"
)

// Codex runs a project's hooks only if the project is trusted and each hook's
// hash is trusted, both in the user's ~/.codex/config.toml.

// codexTrust names what Codex must trust for a worktree: the config file, the
// project roots (a linked worktree reads hooks from its main checkout too) and
// the hooks files there.
type codexTrust struct {
	config     string
	roots      []string
	hooksFiles []string
}

func codexTrustFor(ctx context.Context, wt *gitx.Worktree) (codexTrust, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return codexTrust{}, err
		}
		home = filepath.Join(h, ".codex")
	}
	ct := codexTrust{config: filepath.Join(home, "config.toml"), roots: []string{wt.Root}}
	if main, err := wt.MainRoot(ctx); err == nil && main != wt.Root {
		ct.roots = append(ct.roots, main)
	}
	for _, r := range ct.roots {
		p := filepath.Join(r, ".codex", "hooks.json")
		if _, err := os.Stat(p); err == nil {
			ct.hooksFiles = append(ct.hooksFiles, p)
		}
	}
	return ct, nil
}

// wants lists each table, key and value Codex needs, with a name for notes.
func (ct codexTrust) wants() ([][4]string, error) {
	var out [][4]string
	for _, r := range ct.roots {
		out = append(out, [4]string{"[projects." + tomlString(r) + "]", "trust_level", tomlString("trusted"), "project " + r})
	}
	for _, hf := range ct.hooksFiles {
		entries, err := codexTrustEntries(hf)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			out = append(out, [4]string{"[hooks.state." + tomlString(k) + "]", "trusted_hash", tomlString(entries[k]), "hook " + k})
		}
	}
	return out, nil
}

// apply makes Codex trust the project and intagent's hooks: it sets missing
// entries and corrects ones that say otherwise (a project marked untrusted, a
// hash from an older command), leaving the rest of the file as it was.
func (ct codexTrust) apply() ([]string, error) {
	doc, perm, err := readTOMLDoc(ct.config)
	if err != nil {
		return nil, err
	}
	wants, err := ct.wants()
	if err != nil {
		return nil, err
	}
	var notes []string
	changed := false
	for _, w := range wants {
		prev, err := doc.set(w[0], w[1], w[2])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ct.config, err)
		}
		switch prev {
		case w[2]:
			notes = append(notes, "already trusted: "+w[3])
		case "":
			notes, changed = append(notes, "trusted "+w[3]), true
		default:
			notes, changed = append(notes, fmt.Sprintf("trusted %s (it had %s = %s)", w[3], w[1], prev)), true
		}
	}
	if !changed {
		return notes, nil
	}
	target := ct.config
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = real // a dotfiles link stays a link
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return nil, err
	}
	tmp := target + ".intagent.tmp"
	if err := os.WriteFile(tmp, []byte(doc.String()), perm); err != nil {
		return nil, err
	}
	return notes, os.Rename(tmp, target)
}

// problems says what keeps Codex from running intagent's hooks, if anything.
func (ct codexTrust) problems() ([]string, error) {
	doc, _, err := readTOMLDoc(ct.config)
	if err != nil {
		return nil, err
	}
	wants, err := ct.wants()
	if err != nil {
		return nil, err
	}
	var out []string
	hooks, untrusted := 0, 0
	for _, w := range wants {
		trusted := doc.get(w[0], w[1]) == w[2]
		if w[1] == "trust_level" {
			if !trusted {
				out = append(out, w[3])
			}
			continue
		}
		hooks++
		if !trusted {
			untrusted++
		}
	}
	if untrusted > 0 {
		out = append(out, fmt.Sprintf("%d of intagent's %d hooks (new, or changed since they were trusted)", untrusted, hooks))
	}
	return out, nil
}

// tomlDoc edits a TOML file line by line. It understands only what Codex
// writes for these tables, "[table]" headers and "key = value" lines, and
// leaves every other line as it found it.
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

// table returns the header's line and the end of its table, or -1, -1.
func (d *tomlDoc) table(header string) (int, int) {
	for i, l := range d.lines {
		if tomlHeader(l) == header {
			end := i + 1
			for end < len(d.lines) && !strings.HasPrefix(strings.TrimSpace(d.lines[end]), "[") {
				end++
			}
			return i, end
		}
	}
	return -1, -1
}

// tomlHeader normalises a table header line: no trailing comment, and
// single-quoted keys written double-quoted, as tomlString writes them.
func tomlHeader(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '"':
			j := i + 1
			for j < len(line) && line[j] != '"' {
				if line[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteString(line[i:min(j+1, len(line))])
			i = j
		case '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return line
			}
			b.WriteString(tomlString(line[i+1 : i+1+j]))
			i += j + 1
		case '#':
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// get returns a key's value in a table, without a trailing comment, or "".
func (d *tomlDoc) get(header, key string) string {
	i, end := d.table(header)
	for j := i + 1; i >= 0 && j < end; j++ {
		if k, v, ok := tomlKey(d.lines[j]); ok && k == key {
			return v
		}
	}
	return ""
}

// set sets a key in a table, adding the table or the key if needed, and
// returns the value it had. It refuses rather than add a second copy of a
// table it did not recognise, which would make the file invalid.
func (d *tomlDoc) set(header, key, value string) (string, error) {
	i, end := d.table(header)
	if i < 0 {
		name := strings.Trim(header, "[]")
		for _, l := range d.lines {
			if strings.HasPrefix(strings.TrimSpace(l), "[") && strings.Contains(l, name[strings.IndexByte(name, '.')+1:]) {
				return "", fmt.Errorf("it already has a table like %s that intagent cannot edit safely; "+
					"set %s = %s in it yourself", header, key, value)
			}
		}
		d.lines = append(d.lines, "", header, key+" = "+value)
		return "", nil
	}
	for j := i + 1; j < end; j++ {
		if k, v, ok := tomlKey(d.lines[j]); ok && k == key {
			if v != value {
				d.lines[j] = key + " = " + value
			}
			return v, nil
		}
	}
	d.lines = slices.Insert(d.lines, i+1, key+" = "+value)
	return "", nil
}

func tomlKey(line string) (key, value string, ok bool) {
	k, v, ok := strings.Cut(line, "=")
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if !ok || k == "" || strings.HasPrefix(k, "#") {
		return "", "", false
	}
	if len(k) >= 2 && (k[0] == '"' || k[0] == '\'') && k[len(k)-1] == k[0] {
		k = k[1 : len(k)-1] // "trust_level" = ...
	}
	if strings.HasPrefix(v, `"`) {
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' {
				i++
				continue
			}
			if v[i] == '"' {
				return k, v[:i+1], true
			}
		}
	}
	return k, v, true
}
