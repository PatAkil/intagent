package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/patakil/intagent/internal/fsutil"
	"github.com/patakil/intagent/internal/gitx"
)

// codexTrust names what Codex must trust for a worktree, which runs a
// project's hooks only if the project is trusted and each hook's hash is
// trusted, both in the user's ~/.codex/config.toml: the config file, the
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

// trustEntry is one setting Codex needs: key = value in a table, and what
// it trusts, for the person reading init's report.
type trustEntry struct{ table, key, value, what string }

// wants lists each table, key and value Codex needs, with a name for notes.
func (ct codexTrust) wants() ([]trustEntry, error) {
	var out []trustEntry
	for _, r := range ct.roots {
		out = append(out, trustEntry{"[projects." + tomlString(r) + "]", "trust_level", tomlString("trusted"), "project " + r})
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
			out = append(out, trustEntry{"[hooks.state." + tomlString(k) + "]", "trusted_hash", tomlString(entries[k]), "hook " + k})
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
		prev, err := doc.set(w.table, w.key, w.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ct.config, err)
		}
		switch prev {
		case w.value:
			notes = append(notes, "already trusted: "+w.what)
		case "":
			notes, changed = append(notes, "trusted "+w.what), true
		default:
			notes, changed = append(notes, fmt.Sprintf("trusted %s (it had %s = %s)", w.what, w.key, prev)), true
		}
	}
	if !changed {
		return notes, nil
	}
	if err := os.MkdirAll(filepath.Dir(ct.config), 0o700); err != nil {
		return nil, err
	}
	return notes, fsutil.WriteFile(ct.config, []byte(doc.String()), perm)
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
		trusted := doc.get(w.table, w.key) == w.value
		if w.key == "trust_level" {
			if !trusted {
				out = append(out, w.what)
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
