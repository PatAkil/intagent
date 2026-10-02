package gitx

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// markers name files that make a directory a package, project or module.
var markers = []string{
	"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py", "pom.xml",
	"build.gradle", "build.gradle.kts", "BUILD", "BUILD.bazel", "project.json",
	"composer.json", "Gemfile", "mix.exs", "deno.json", "AGENTS.md", "CLAUDE.md",
}

// Areas maps files to the area of the monorepo they belong to. Configured
// patterns win; otherwise the nearest directory with a package marker; otherwise
// the top-level directory. It caches directory lookups and is meant for one
// worktree and one short-lived process.
type Areas struct {
	root     string
	patterns []string
	cache    map[string]string
}

// NewAreas returns an area resolver for a worktree. Patterns such as
// "services/*" or "libs/*/*" define areas explicitly.
func NewAreas(root string, patterns []string) *Areas {
	return &Areas{root: root, patterns: patterns, cache: map[string]string{}}
}

// Of returns the area of a slash-separated, root-relative file path, or "" for
// files at the top level.
func (a *Areas) Of(rel string) string {
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	for _, p := range a.patterns {
		if area, ok := matchArea(strings.Trim(p, "/"), dir); ok {
			return area
		}
	}
	return a.markerArea(dir, rel)
}

// matchArea reports the prefix of dir with as many segments as pattern, if
// that prefix matches the pattern segment by segment.
func matchArea(pattern, dir string) (string, bool) {
	ps := strings.Split(pattern, "/")
	ds := strings.Split(dir, "/")
	if len(ds) < len(ps) {
		return "", false
	}
	for i, seg := range ps {
		if ok, err := path.Match(seg, ds[i]); err != nil || !ok {
			return "", false
		}
	}
	return strings.Join(ds[:len(ps)], "/"), true
}

func (a *Areas) markerArea(dir, rel string) string {
	if v, ok := a.cache[dir]; ok {
		return v
	}
	area := ""
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if a.hasMarker(d) {
			area = d
			break
		}
	}
	if area == "" {
		area = strings.SplitN(rel, "/", 2)[0]
	}
	a.cache[dir] = area
	return area
}

func (a *Areas) hasMarker(dir string) bool {
	abs := filepath.Join(a.root, filepath.FromSlash(dir))
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(abs, m)); err == nil {
			return true
		}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".csproj") || strings.HasSuffix(e.Name(), ".fsproj") {
			return true
		}
	}
	return false
}
