package board

import "github.com/patakil/intagent/internal/glob"

// workTrace counts the work the board does, so that tests can assert how
// much a call visits rather than how long it takes: counts hold on a busy
// machine, and they show work that grows with the board long before it is
// slow enough to time.
type workTrace struct {
	conflictsFor  int // paths checked against the claims of a repository
	conflictWith  int // other claims a path was compared with
	workedInArea  int // claims searched for work in an area
	areaVisits    int // footprint entries those searches read
	ranked        int // claims ranked for a greeting
	sessionVisits int // sessions read by Sweep and to find which are live
	globMatch     int // glob.Match calls
	globOverlap   int // glob.Overlap calls
}

// trace is nil except while a test counts. A test that sets it must not run
// the board from several goroutines, and must reset it when it ends.
var trace *workTrace

// match is glob.Match, counted.
func match(pattern, name string) bool {
	if trace != nil {
		trace.globMatch++
	}
	return glob.Match(pattern, name)
}

// overlap is glob.Overlap, counted.
func overlap(a, b string) bool {
	if trace != nil {
		trace.globOverlap++
	}
	return glob.Overlap(a, b)
}
