package gitx

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// markers name files that make a directory a package, project or module.
var markers = []string{
	"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py", "pom.xml",
	"build.gradle", "build.gradle.kts", "BUILD", "BUILD.bazel", "project.json",
	"composer.json", "Gemfile", "mix.exs", "deno.json", "AGENTS.md", "CLAUDE.md",
}

// readDir is os.ReadDir; tests count the directories read.
var readDir = os.ReadDir

// Areas maps files to the area of the monorepo they belong to. Configured
// patterns win; otherwise the nearest directory with a package marker; otherwise
// the top-level directory. It reads each directory at most once and caches
// what it found, and is meant for one worktree and one short-lived process.
type Areas struct {
	root     string
	patterns []string
	cache    map[string]string // directory -> area of the files directly in it
	marked   map[string]bool   // directory -> holds a marker
	// foldCase compares names as the file system does where it ignores case,
	// so a directory has the same area as when markers were looked up with
	// os.Stat.
	foldCase bool
}

// NewAreas returns an area resolver for a worktree. Patterns such as
// "services/*" or "libs/*/*" define areas explicitly.
func NewAreas(root string, patterns []string) *Areas {
	return &Areas{root: root, patterns: patterns, cache: map[string]string{}, marked: map[string]bool{},
		foldCase: runtime.GOOS == "darwin" || runtime.GOOS == "windows"}
}

// Of returns the area of a slash-separated, root-relative file path, or "" for
// files at the top level.
func (a *Areas) Of(rel string) string {
	return a.OfDir(path.Dir(rel))
}

// OfDir returns the area of the files directly in dir, a slash-separated,
// root-relative directory, or "" for the top level.
func (a *Areas) OfDir(dir string) string {
	if dir == "." || dir == "" {
		return ""
	}
	for _, p := range a.patterns {
		if area, ok := matchArea(strings.Trim(p, "/"), dir); ok {
			return area
		}
	}
	return a.markerArea(dir)
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

func (a *Areas) markerArea(dir string) string {
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
		area, _, _ = strings.Cut(dir, "/")
	}
	a.cache[dir] = area
	return area
}

// hasMarker reports whether dir holds a package marker. It reads the
// directory once, however many files below it are looked up.
func (a *Areas) hasMarker(dir string) bool {
	if v, ok := a.marked[dir]; ok {
		return v
	}
	abs := filepath.Join(a.root, filepath.FromSlash(dir))
	entries, _ := readDir(abs) // a directory that cannot be read holds no marker
	has := false
	for _, e := range entries {
		if a.isMarker(abs, e) {
			has = true
			break
		}
	}
	a.marked[dir] = has
	return has
}

// isMarker reports whether a directory entry marks a package: one of the
// markers, if it is not a link that leads nowhere, or a .NET project.
func (a *Areas) isMarker(dir string, e fs.DirEntry) bool {
	name := e.Name()
	if strings.HasSuffix(name, ".csproj") || strings.HasSuffix(name, ".fsproj") {
		return true
	}
	if !slices.ContainsFunc(markers, func(m string) bool { return name == m || a.foldCase && strings.EqualFold(name, m) }) {
		return false
	}
	if e.Type()&fs.ModeSymlink != 0 {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	return true
}
