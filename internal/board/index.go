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
//   - each claim's sessions (claimSessions);
//   - for each claim, the latest change in each area it changed (areaAt) and
//     its changed paths in order (sortedPaths);
//   - what each claim's footprint, and each member's claims' footprints,
//     count against their byte budgets (fpBytes, memberBytes).
//
// They are derived state, never saved, and rebuilt by Restore. Only the
// functions in this file change them, or what they are derived from: the
// board's claims and sessions, a session's ClaimID and a claim's Footprint.
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
// claim that no longer exists, until Sweep drops them or they move.
func (b *Board) removeClaim(c *claim) {
	if b.claims[c.ID] != c {
		return
	}
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
// claim id: a session moves when its agent reports from another worktree.
func (b *Board) attachSession(s *session, id string) {
	switch old := b.sessions[s.Key]; {
	case old == s && s.ClaimID == id:
		return
	case old == s:
		b.unlinkSession(s)
	case old != nil:
		b.detachSession(old)
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
}

func (b *Board) unlinkSession(s *session) {
	m := b.claimSessions[s.ClaimID]
	delete(m, s.Key)
	if len(m) == 0 {
		delete(b.claimSessions, s.ClaimID)
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

// footprintCost is what one changed file counts against the footprint byte
// budgets: its path and its area, and about what the board keeps for it
// beside them.
func footprintCost(path string, t *touch) int { return len(path) + len(t.Area) + 96 }

func (c *claim) countBytes() {
	c.fpBytes = 0
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
type liveness struct {
	b   *Board
	now time.Time
}

func (b *Board) liveAt(now time.Time) liveness { return liveness{b: b, now: now} }

// claim reports whether claim id has a live session.
func (l liveness) claim(id string) bool {
	if trace != nil {
		trace.liveClaims++
	}
	for _, s := range l.b.claimSessions[id] {
		if trace != nil {
			trace.sessionVisits++
		}
		if l.b.state(l.now, s).Live() {
			return true
		}
	}
	return false
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
