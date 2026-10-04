package board

// workTrace counts the work the board does, so that tests can assert how
// much a call visits rather than how long it takes: counts hold on a busy
// machine, and they show work that grows with the board long before it is
// slow enough to time.
type workTrace struct {
	conflictsFor  int // paths checked against the claims of a repository
	conflictWith  int // other claims a path was compared with
	workedInArea  int // claims asked for their work in an area
	areaVisits    int // footprint entries read to index the areas a claim changed files in
	ranked        int // claims ranked for a greeting
	liveClaims    int // claims asked whether they are live
	sessionVisits int // sessions read by Sweep and to find which are live
	globMatch     int // glob.Match calls
	globOverlap   int // glob.Overlap calls
}

// trace is nil except while a test counts. A test that sets it must not be
// parallel or run the board from several goroutines, and must reset it when
// it ends: the parallel tests run the board once the others are done.
var trace *workTrace
