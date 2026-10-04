package gitx

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// statArea is the area as markers were once found, with an os.Stat per
// marker and directory: the answer Areas must keep.
func statArea(root, rel string) string {
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	for d := dir; d != "."; d = path.Dir(d) {
		abs := filepath.Join(root, filepath.FromSlash(d))
		for _, m := range markers {
			if _, err := os.Stat(filepath.Join(abs, m)); err == nil {
				return d
			}
		}
		entries, _ := os.ReadDir(abs)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".csproj") || strings.HasSuffix(e.Name(), ".fsproj") {
				return d
			}
		}
	}
	return strings.SplitN(rel, "/", 2)[0]
}

// countReads counts the directories Areas reads, by path.
func countReads(t *testing.T) map[string]int {
	t.Helper()
	reads := map[string]int{}
	readDir = func(name string) ([]os.DirEntry, error) {
		reads[name]++
		return os.ReadDir(name)
	}
	t.Cleanup(func() { readDir = os.ReadDir })
	return reads
}

// A monorepo's footprint names thousands of files in a few hundred
// directories: each directory is read once, and the areas are those os.Stat
// found.
func TestAreasReadEachDirectoryOnce(t *testing.T) {
	root := t.TempDir()
	rng := rand.New(rand.NewPCG(1, 2))
	var files []string
	for i := range 3000 {
		depth := 1 + rng.IntN(5)
		segs := make([]string, depth)
		for j := range segs {
			segs[j] = fmt.Sprintf("d%d", rng.IntN(4))
		}
		dir := strings.Join(segs, "/")
		files = append(files, fmt.Sprintf("%s/f%d.go", dir, i))
		if rng.IntN(40) == 0 {
			write(t, root, dir+"/"+markers[rng.IntN(len(markers))], "")
		}
		if rng.IntN(200) == 0 {
			write(t, root, dir+"/App.csproj", "")
		}
		if rng.IntN(100) == 0 {
			files = append(files, dir+"/gone/f.go") // a deleted directory
		}
	}
	write(t, root, "d1/d2/BUILD/x", "")          // a directory named like a marker
	write(t, root, "d2/build/x", "")             // and one that differs in case
	write(t, root, "d3/d3/elsewhere/go.mod", "") // a link to a marker
	if runtime.GOOS != "windows" {
		_ = os.Symlink(filepath.Join(root, "d3/d3/elsewhere/go.mod"), filepath.Join(root, "d3/d0/go.mod"))
		_ = os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "d0/d1/go.mod")) // leads nowhere
	}

	reads := countReads(t)
	a := NewAreas(root, nil)
	a.foldCase = false // as os.Stat on this file system, which tells case apart
	for _, f := range files {
		if got, want := a.Of(f), statArea(root, f); got != want {
			t.Errorf("Of(%q) = %q, want %q", f, got, want)
		}
	}
	for dir, n := range reads {
		if n > 1 {
			t.Errorf("%s read %d times", dir, n)
		}
	}
	if len(reads) == 0 {
		t.Fatal("no directory was read")
	}
}

// Where the file system ignores case, so does the marker lookup, as os.Stat
// did there.
func TestAreasFoldCaseWhereTheFileSystemDoes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tools/gen/Build/out.txt", "")
	write(t, root, "tools/gen/main.go", "")
	write(t, root, "web/app/Package.JSON", "")
	write(t, root, "web/app/src/main.ts", "")
	for _, c := range []struct {
		fold     bool
		gen, web string
	}{
		{fold: false, gen: "tools", web: "web"},
		{fold: true, gen: "tools/gen", web: "web/app"},
	} {
		a := NewAreas(root, nil)
		a.foldCase = c.fold
		if got := a.Of("tools/gen/main.go"); got != c.gen {
			t.Errorf("foldCase %v: tools/gen/main.go in %q, want %q", c.fold, got, c.gen)
		}
		if got := a.Of("web/app/src/main.ts"); got != c.web {
			t.Errorf("foldCase %v: web/app/src/main.ts in %q, want %q", c.fold, got, c.web)
		}
	}
	if want := runtime.GOOS == "darwin" || runtime.GOOS == "windows"; NewAreas(root, nil).foldCase != want {
		t.Errorf("foldCase on %s: want %v", runtime.GOOS, want)
	}
}
