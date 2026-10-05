package board

import (
	"iter"
	"maps"
	"slices"
	"strings"
	"time"
)

// The board answers an edit from indexes kept beside its maps, so that what
// one check costs grows with the claims of its repository, not with every
// claim, session and file on the server:
//
//   - each repository's claims, in ID order (byRepo);
//   - each claim's sessions (claimSessions), and the sessions that reported
//     from it before they moved to another, which keep it live (alsoSessions,
//     from each session's Also);
//   - for each claim, the latest change in each area it changed (areaAt) and
//     its changed paths in order (sortedPaths);
//   - what each claim's footprint and directories added whole, and each
//     member's claims', count against their byte budgets (fpBytes,
//     memberBytes).
//
// They are derived state, never saved, and rebuilt by Restore. Only the
// functions in this file change them, or what they are derived from: the
// board's claims and sessions, a session's ClaimID and Also, and a claim's
// Footprint and Dirs.
// A test recomputes them all after every step of the transcript.
//
// A snapshot shares each claim's Footprint and reads it after the lock is
// released (Snapshot), so a footprint is copied on write: putTouch changes a
// copy of a map a snapshot shares, and setFootprint puts a new map in place.
// Nothing else may change a footprint map, or a touch in one.

// repoIndex is a repository's claims in ID order. A removed claim stays in
// the list, marked, until removed ones make up half of it: removing one is
// then as cheap as adding one, and a sweep that forgets thousands does not
// move the list for each.
type repoIndex struct {
	claims  []*claim
	removed int
}

func byClaimID(c *claim, id string) int { return strings.Compare(c.ID, id) }

// addClaim puts c on the board, replacing a claim with its ID.
func (b *Board) addClaim(c *claim) {
	if old := b.claims[c.ID]; old != nil {
		b.removeClaim(old)
	}
	b.claims[c.ID] = c
	b.byKey[c.key()] = c.ID
	r := b.byRepo[c.Repo]
	if r == nil {
		r = &repoIndex{}
		b.byRepo[c.Repo] = r
	}
	i, _ := slices.BinarySearchFunc(r.claims, c.ID, byClaimID)
	r.claims = slices.Insert(r.claims, i, c)
	c.removed = false
	if c.Footprint == nil {
		c.Footprint = map[string]*touch{}
	}
	c.indexFootprint()
	b.charge(c.Member, c.fpBytes)
}

// removeClaim takes c off the board. Its sessions stay, as sessions of a
// claim that no longer exists, until Sweep drops them or they move; the
// sessions that kept it live from elsewhere let go of it.
func (b *Board) removeClaim(c *claim) {
	if b.claims[c.ID] != c {
		return
	}
	for _, s := range b.alsoSessions[c.ID] {
		delete(s.Also, c.ID)
		if len(s.Also) == 0 {
			s.Also = nil
		}
	}
	delete(b.alsoSessions, c.ID)
	delete(b.claims, c.ID)
	if b.byKey[c.key()] == c.ID {
		delete(b.byKey, c.key())
	}
	c.removed = true
	b.charge(c.Member, -c.fpBytes)
	r := b.byRepo[c.Repo]
	r.removed++
	if 2*r.removed <= len(r.claims) {
		return
	}
	keep := r.claims[:0]
	for _, o := range r.claims {
		if !o.removed {
			keep = append(keep, o)
		}
	}
	clear(r.claims[len(keep):])
	r.claims, r.removed = keep, 0
	if len(keep) == 0 {
		delete(b.byRepo, c.Repo)
	}
}

// claimsIn yields a repository's claims in ID order.
func (b *Board) claimsIn(repo string) iter.Seq[*claim] {
	return func(yield func(*claim) bool) {
		r := b.byRepo[repo]
		if r == nil {
			return
		}
		for _, c := range r.claims {
			if !c.removed && !yield(c) {
				return
			}
		}
	}
}

// attachSession puts s on the board, if it is not there yet, as a session of
// claim id: a session moves when its agent reports from another worktree,
// and the claim it moves from stays live while it is (Also). A session put
// on the board keeps the claims in its Also that are on the board.
func (b *Board) attachSession(s *session, id string) {
	switch old := b.sessions[s.Key]; {
	case old == s && s.ClaimID == id:
		return
	case old == s:
		b.unlinkSession(s)
		b.alsoOff(s, id) // the claim it reports from is its own
		if b.claims[s.ClaimID] != nil {
			b.alsoOn(s, s.ClaimID, s.LastSeen)
		}
	case old != nil:
		b.detachSession(old)
		b.linkAlso(s, id)
	default:
		b.linkAlso(s, id)
	}
	s.ClaimID = id
	b.sessions[s.Key] = s
	m := b.claimSessions[id]
	if m == nil {
		m = map[string]*session{}
		b.claimSessions[id] = m
	}
	m[s.Key] = s
}

// detachSession takes s off the board.
func (b *Board) detachSession(s *session) {
	if b.sessions[s.Key] != s {
		return
	}
	delete(b.sessions, s.Key)
	b.unlinkSession(s)
	for id := range s.Also {
		unlinkFrom(b.alsoSessions, id, s.Key)
	}
}

func (b *Board) unlinkSession(s *session) { unlinkFrom(b.claimSessions, s.ClaimID, s.Key) }

// unlinkFrom takes session k out of index's set for claim id.
func unlinkFrom(index map[string]map[string]*session, id, k string) {
	m := index[id]
	delete(m, k)
	if len(m) == 0 {
		delete(index, id)
	}
}

// alsoOn has session s keep claim id live, as last reported from at, and
// lets go of the claims it reported from longest ago (then by ID) past
// maxAlsoClaims.
func (b *Board) alsoOn(s *session, id string, at time.Time) {
	if s.Also == nil {
		s.Also = map[string]time.Time{}
	}
	s.Also[id] = at
	m := b.alsoSessions[id]
	if m == nil {
		m = map[string]*session{}
		b.alsoSessions[id] = m
	}
	m[s.Key] = s
	b.boundAlso(s)
}

// alsoOff has session s no longer keep claim id live from elsewhere.
func (b *Board) alsoOff(s *session, id string) {
	if _, ok := s.Also[id]; !ok {
		return
	}
	delete(s.Also, id)
	if len(s.Also) == 0 {
		s.Also = nil
	}
	unlinkFrom(b.alsoSessions, id, s.Key)
}

// linkAlso indexes the claims a session put on the board as a session of
// claim own keeps live, a snapshot's: those still on the board but own, as
// many as it may keep.
func (b *Board) linkAlso(s *session, own string) {
	for id := range s.Also {
		if b.claims[id] == nil || id == own {
			delete(s.Also, id)
			continue
		}
		m := b.alsoSessions[id]
		if m == nil {
			m = map[string]*session{}
			b.alsoSessions[id] = m
		}
		m[s.Key] = s
	}
	if len(s.Also) == 0 {
		s.Also = nil
	}
	b.boundAlso(s)
}

// boundAlso lets go of the claims session s reported from longest ago, ties
// going by ID, until it keeps maxAlsoClaims.
func (b *Board) boundAlso(s *session) {
	for len(s.Also) > maxAlsoClaims {
		oldest := ""
		for id, at := range s.Also {
			if was, ok := s.Also[oldest]; !ok || at.Before(was) || at.Equal(was) && id < oldest {
				oldest = id
			}
		}
		b.alsoOff(s, oldest)
	}
}

// sessionsOf yields the sessions whose liveness is claim id's: its own, then
// those that reported from it before they moved (Also).
func (b *Board) sessionsOf(id string) iter.Seq[*session] {
	return func(yield func(*session) bool) {
		for _, s := range b.claimSessions[id] {
			if !yield(s) {
				return
			}
		}
		for _, s := range b.alsoSessions[id] {
			if !yield(s) {
				return
			}
		}
	}
}

// setFootprint replaces c's footprint with fp, whose paths are paths, in any
// order, and keeps its member's byte count. It takes both. A snapshot may
// still be reading the map replaced, which is left as it was.
func (b *Board) setFootprint(c *claim, fp map[string]*touch, paths []string) {
	was := c.fpBytes
	c.setFootprint(fp, paths)
	b.charge(c.Member, c.fpBytes-was)
}

// putTouch records t as c's latest change to path, and keeps its member's
// byte count.
func (b *Board) putTouch(c *claim, path string, t *touch) {
	was := c.fpBytes
	c.putTouch(path, t)
	b.charge(c.Member, c.fpBytes-was)
}

// dropTouches takes paths, which it holds, out of c's footprint, and keeps
// its member's byte count.
func (b *Board) dropTouches(c *claim, paths []string) {
	was := c.fpBytes
	c.dropTouches(paths)
	b.charge(c.Member, c.fpBytes-was)
}

// setDirs replaces c's directories added whole with dirs, and keeps its
// byte counts. A snapshot may still be reading the map replaced.
func (b *Board) setDirs(c *claim, dirs map[string]*touch) {
	was := c.fpBytes
	c.fpBytes += dirsCost(dirs) - dirsCost(c.Dirs)
	c.Dirs = dirs
	b.charge(c.Member, c.fpBytes-was)
}

// charge adds n to what member's claims' footprints count, which the board
// keeps only for members whose claims have changed files.
func (b *Board) charge(member string, n int) {
	if b.memberBytes[member] += n; b.memberBytes[member] == 0 {
		delete(b.memberBytes, member)
	}
}

func (c *claim) setFootprint(fp map[string]*touch, paths []string) {
	c.Footprint, c.fpShared = fp, false
	// What git reports is usually in order already.
	if !slices.IsSorted(paths) {
		slices.Sort(paths)
	}
	c.sortedPaths = paths
	c.countBytes()
	c.indexAreas()
}

// putTouch records t as the claim's latest change to path. When a snapshot
// shares the footprint, it changes a copy, which the claim keeps.
func (c *claim) putTouch(path string, t *touch) {
	if c.fpShared {
		c.Footprint, c.fpShared = maps.Clone(c.Footprint), false
	}
	old, had := c.Footprint[path]
	c.Footprint[path] = t
	c.fpBytes += footprintCost(path, t)
	if had {
		c.fpBytes -= footprintCost(path, old)
	} else {
		i, _ := slices.BinarySearch(c.sortedPaths, path)
		c.sortedPaths = slices.Insert(c.sortedPaths, i, path)
	}
	if had && old.Area != "" && (old.Area != t.Area || t.At.Before(old.At)) && !old.At.Before(c.areaAt[old.Area]) {
		// The change replaced was the latest in its area, which may now
		// have an earlier one, or none.
		c.indexAreas()
		return
	}
	c.noteArea(t)
}

// dropTouches takes paths, which it holds, out of the claim's footprint,
// copying a map a snapshot shares first, and indexes what is left once.
func (c *claim) dropTouches(paths []string) {
	if c.fpShared {
		c.Footprint, c.fpShared = maps.Clone(c.Footprint), false
	}
	gone := make(map[string]bool, len(paths))
	for _, p := range paths {
		if t, ok := c.Footprint[p]; ok {
			delete(c.Footprint, p)
			c.fpBytes -= footprintCost(p, t)
			gone[p] = true
		}
	}
	c.sortedPaths = slices.DeleteFunc(c.sortedPaths, func(p string) bool { return gone[p] })
	c.indexAreas()
}

// indexFootprint rebuilds the claim's indexes from its footprint.
func (c *claim) indexFootprint() {
	c.sortedPaths = make([]string, 0, len(c.Footprint))
	for p := range c.Footprint {
		c.sortedPaths = append(c.sortedPaths, p)
	}
	slices.Sort(c.sortedPaths)
	c.countBytes()
	c.indexAreas()
}

// footprintCost is what one changed file, or one directory added whole,
// counts against the footprint byte budgets: its path and its area, and
// about what the board keeps for it beside them.
func footprintCost(path string, t *touch) int { return len(path) + len(t.Area) + 96 }

// dirsCost is what a claim's directories added whole count, of at most
// maxFootprintDirs.
func dirsCost(dirs map[string]*touch) int {
	n := 0
	for d, t := range dirs {
		n += footprintCost(d, t)
	}
	return n
}

func (c *claim) countBytes() {
	c.fpBytes = dirsCost(c.Dirs)
	for p, t := range c.Footprint {
		c.fpBytes += footprintCost(p, t)
	}
}

func (c *claim) indexAreas() {
	if trace != nil {
		trace.areaVisits += len(c.Footprint)
	}
	c.areaAt = make(map[string]time.Time)
	for _, t := range c.Footprint {
		c.noteArea(t)
	}
}

func (c *claim) noteArea(t *touch) {
	if t.Area == "" {
		return
	}
	if at, ok := c.areaAt[t.Area]; !ok || t.At.After(at) {
		c.areaAt[t.Area] = t.At
	}
}

// pathsIn returns the claim's paths from lo up to, not including, hi, in
// order.
func (c *claim) pathsIn(lo, hi string) []string {
	i, _ := slices.BinarySearch(c.sortedPaths, lo)
	j, _ := slices.BinarySearch(c.sortedPaths, hi)
	return c.sortedPaths[i:max(i, j)]
}

// sharesArea reports whether two claims changed files in a common area.
func sharesArea(a, b *claim) bool {
	if len(b.areaAt) < len(a.areaAt) {
		a, b = b, a
	}
	for area := range a.areaAt {
		if _, ok := b.areaAt[area]; ok {
			return true
		}
	}
	return false
}

// liveness says which claims and sessions are live at one moment. It reads
// only the sessions it is asked about, and the sessions of the claims it is
// asked about, so it costs nothing for the rest of the board; and it is
// derived from the time it is asked for, not from when Sweep last ran.
//
// It remembers what it found of each claim, which an edit of many paths
// asks again for each: a claim may hold hundreds of sessions that are not
// live (maxStalledSessions), and thousands until a sweep trims them. So a
// liveness is for one call, during which no session is added, moved or
// heard from.
type liveness struct {
	b      *Board
	now    time.Time
	claims map[string]claimLive // what find found, by claim ID
}

// claimLive is what liveness found of one claim's sessions.
type claimLive struct {
	live bool // one of them is live
	own  bool // one that reports from the claim is live
	// from is, when the claim is live only through sessions that moved from
	// it (Also), the claim they all report from now, if they report from one.
	from string
}

func (b *Board) liveAt(now time.Time) liveness {
	return liveness{b: b, now: now, claims: map[string]claimLive{}}
}

// claim reports whether claim id has a live session, its own or one that
// moved from it.
func (l liveness) claim(id string) bool {
	if trace != nil {
		trace.liveClaims++
	}
	return l.find(id).live
}

// carried reports whether claim id is live only through sessions that moved
// from it to claim self and report from there now. To self it is their own
// work in another worktree: what they left there neither holds them back
// nor is news to them. To anyone else it is live.
func (l liveness) carried(id, self string) bool {
	return self != "" && l.find(id).from == self
}

// find looks at claim id's sessions, its own first: one of those that is
// live settles it; of those that moved from it, two live ones that report
// from different claims do.
func (l liveness) find(id string) claimLive {
	if f, ok := l.claims[id]; ok {
		return f
	}
	var f claimLive
	for _, s := range l.b.claimSessions[id] {
		if trace != nil {
			trace.sessionVisits++
		}
		if l.b.state(l.now, s).Live() {
			f = claimLive{live: true, own: true}
			break
		}
	}
	if !f.live {
		f = l.moved(id)
	}
	l.claims[id] = f
	return f
}

// moved is what claim id's sessions that moved from it (Also) make of it.
func (l liveness) moved(id string) claimLive {
	var f claimLive
	for _, s := range l.b.alsoSessions[id] {
		if trace != nil {
			trace.sessionVisits++
		}
		switch {
		case !l.b.state(l.now, s).Live():
		case !f.live:
			f.live, f.from = true, s.ClaimID
		case s.ClaimID != f.from:
			f.from = ""
			return f
		}
	}
	return f
}

// session reports whether the session with key k is live.
func (l liveness) session(k string) bool {
	s := l.b.sessions[k]
	if s == nil {
		return false
	}
	if trace != nil {
		trace.sessionVisits++
	}
	return l.b.state(l.now, s).Live()
}
