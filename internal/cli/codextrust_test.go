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
	// A table it cannot recognise is left alone rather than declared twice.
	doc := &tomlDoc{lines: []string{`[ projects . "/repo" ]`, `trust_level = "untrusted"`}}
	if _, err := doc.set(`[projects."/repo"]`, "trust_level", `"trusted"`); err == nil || !strings.Contains(err.Error(), "yourself") {
		t.Fatalf("set over an unrecognised table: %v\n%s", err, doc.String())
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
