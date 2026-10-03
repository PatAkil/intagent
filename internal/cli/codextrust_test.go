package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTOMLDocFindsWhatPeopleWrite(t *testing.T) {
	tests := []struct {
		name, text, want string
	}{
		{"plain", "[projects.\"/repo\"]\ntrust_level = \"untrusted\"\n", `"untrusted"`},
		{"header comment", "[projects.\"/repo\"]   # my work repo\ntrust_level = \"untrusted\"\n", `"untrusted"`},
		{"single quotes", "[projects.'/repo']\ntrust_level = \"untrusted\"\n", `"untrusted"`},
		{"quoted key", "[projects.\"/repo\"]\n\"trust_level\" = \"untrusted\"\n", `"untrusted"`},
		{"value comment", "[projects.\"/repo\"]\ntrust_level = \"untrusted\" # for now\n", `"untrusted"`},
		{"crlf", "[projects.\"/repo\"]\r\ntrust_level = \"untrusted\"\r\n", `"untrusted"`},
		{"other table", "[projects.\"/repo2\"]\ntrust_level = \"untrusted\"\n", ""},
	}
	for _, tt := range tests {
		doc := &tomlDoc{lines: strings.Split(strings.TrimSuffix(tt.text, "\n"), "\n")}
		if got := doc.get(`[projects."/repo"]`, "trust_level"); strings.TrimSuffix(got, "\r") != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
		if _, err := doc.set(`[projects."/repo"]`, "trust_level", `"trusted"`); err != nil {
			t.Errorf("%s: %v", tt.name, err)
		}
		if n := strings.Count(doc.String(), "projects."); tt.want != "" && n != 1 {
			t.Errorf("%s: %d project tables after set:\n%s", tt.name, n, doc.String())
		}
	}
	// A header with spaces, or an escape, is the same table.
	for _, h := range []string{`[ projects . "/repo" ]`, `[projects."/repo"]`} {
		doc := &tomlDoc{lines: []string{h, `trust_level = "untrusted"`}}
		if prev, err := doc.set(`[projects."/repo"]`, "trust_level", `"trusted"`); err != nil || prev != `"untrusted"` ||
			doc.String() != h+"\ntrust_level = \"trusted\"\n" {
			t.Fatalf("set in %s: %q %v\n%s", h, prev, err, doc.String())
		}
	}
}

// Lines that look like a header or a key inside a multi-line value are not
// one, and a key with "=" inside its quotes is one key.
func TestTOMLDocReadsMultiLineValues(t *testing.T) {
	for name, text := range map[string]string{
		"'=' in a dotted key":   `projects."/srv/a=b".trust_level = "untrusted"`,
		"'=' in a quoted key":   "[projects]\n\"/srv/a=b\" = { trust_level = \"untrusted\" }",
		"after a nested array":  "matrix = [\n  [1, 2],\n]\nprojects = { \"/srv/a=b\" = { trust_level = \"untrusted\" } }",
		"after a string":        "notes = \"\"\"\n[NOTE] keep this\n\"\"\"\nprojects = { \"/srv/a=b\" = { trust_level = \"untrusted\" } }",
		"an unreadable line":    "[projects]\nthis is not toml",
		"an unterminated array": "matrix = [\n  1,",
	} {
		doc := &tomlDoc{lines: strings.Split(text, "\n")}
		if _, err := doc.set(`[projects."/srv/a=b"]`, "trust_level", `"trusted"`); err == nil {
			t.Errorf("%s: set wrote\n%s", name, doc.String())
		}
	}
	// A header inside a multi-line string is no table to edit, and does not
	// end the one it sits in.
	doc := &tomlDoc{lines: strings.Split("[projects.\"/repo\"]\nnote = '''\n[projects.\"/other\"]\n'''\ntrust_level = \"untrusted\"", "\n")}
	if prev, err := doc.set(`[projects."/repo"]`, "trust_level", `"trusted"`); err != nil || prev != `"untrusted"` {
		t.Fatalf("set past a multi-line string: %q %v\n%s", prev, err, doc.String())
	}
	if got := doc.get(`[projects."/other"]`, "trust_level"); got != "" {
		t.Fatalf("read a table out of a string: %q", got)
	}
}

// A dotfiles setup links config.toml elsewhere; trusting keeps the link.
func TestTrustCodexWritesThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "config.toml")
	writeFile(t, real, "model = \"gpt-6\"\n")
	link := filepath.Join(dir, "codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	ct := codexTrust{config: link, roots: []string{"/repo"}}
	if _, err := ct.apply(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.toml is no longer a link: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), `trust_level = "trusted"`) || !strings.HasPrefix(string(data), "model") {
		t.Fatalf("linked file:\n%s", data)
	}
}

// A project table can also be written inline or with dotted keys; adding a
// [projects."/repo"] header after either would define it twice, and Codex
// could not read its config.
func TestTOMLDocSeesTablesDefinedByKeys(t *testing.T) {
	for name, text := range map[string]string{
		"inline parent":   `projects = { "/other" = { trust_level = "trusted" } }`,
		"inline in table": "[projects]\n\"/repo\" = { trust_level = \"untrusted\" }",
		"dotted":          `projects."/repo".trust_level = "untrusted"`,
		"dotted, quoted":  `"projects".'/repo'.trust_level = "untrusted"`,
	} {
		doc := &tomlDoc{lines: strings.Split(text, "\n")}
		if _, err := doc.set(`[projects."/repo"]`, "trust_level", `"trusted"`); err == nil || !strings.Contains(err.Error(), "yourself") {
			t.Errorf("%s: set gave %v:\n%s", name, err, doc.String())
		}
	}
	// Codex's older form, a [projects] table of inline tables for other
	// projects, takes a table for this one.
	doc := &tomlDoc{lines: []string{"[projects]", `"/other" = { trust_level = "trusted" }`, "[[mcp_list]]", `projects = 1`}}
	if _, err := doc.set(`[projects."/repo"]`, "trust_level", `"trusted"`); err != nil || !strings.Contains(doc.String(), "[projects.\"/repo\"]\ntrust_level") {
		t.Fatalf("set beside other projects: %v\n%s", err, doc.String())
	}
}

func TestSplitKey(t *testing.T) {
	for in, want := range map[string]string{
		`projects."/repo"`:     "projects|/repo",
		` a . 'b c' . "d\"e" `: `a|b c|d"e`,
		`hooks.state."x:y.z"`:  "hooks|state|x:y.z",
		`trust_level`:          "trust_level",
		`"unterminated`:        "!",
		`a b`:                  "!",
		`a.`:                   "!",
		`  "x = y",`:           "!",
	} {
		parts, ok := splitKey(in)
		got := strings.Join(parts, "|")
		if !ok {
			got = "!"
		}
		if got != want {
			t.Errorf("splitKey(%q) = %q, want %q", in, got, want)
		}
	}
}
