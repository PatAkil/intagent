package board

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// The board's bounds on what it keeps, whatever its clients send: sessions,
// claims nobody works in, and the files claims changed. Each keeps what an
// agent still needs, and lets go of the rest in an order that does not
// depend on the order of a map. None refuses a hook.

// Bounds on the sessions a claim keeps that are not live, which only the
// dashboard shows, and which liveness reads through to learn whether the
// claim is live: a client that starts a session for every event would
// otherwise leave thousands on one claim. A claim keeps the
// maxEndedSessions that ended or are gone heard from last, and the
// maxStalledSessions that stalled: more of those, as a fleet working in one
// worktree may stall together on a network blip, and each that comes back
// is announced as recovered rather than as a new session. Each sweep trims
// every claim to both (trimQuiet); between sweeps, a claim may hold up to
// twice as many that ended or are gone (trimSessions), and any number that
// stalled since.
const (
	maxEndedSessions   = 20
	maxStalledSessions = 200
)

// trimSessions drops the sessions of claim c that ended or are gone, past
// the maxEndedSessions heard from last, ties going by key; but not keep, the
// session reporting, which is about to be heard from. It looks only once the
// claim holds more than twice maxEndedSessions, and more than twice what it
// held after it last looked (c.trimAfter): the live sessions of a flood into
// one worktree cannot be dropped, and looking at all of them for each new
// one made the flood quadratic.
func (b *Board) trimSessions(now time.Time, c *claim, keep *session) {
	m := b.claimSessions[c.ID]
	if len(m) <= max(2*maxEndedSessions, c.trimAfter) {
		return
	}
	defer func() { c.trimAfter = 2 * len(b.claimSessions[c.ID]) }()
	var done []*session
	for _, s := range m {
		if trace != nil {
			trace.sessionVisits++
		}
		if st := b.state(now, s); s != keep && (st == StateEnded || st == StateGone) {
			done = append(done, s)
		}
	}
	if len(done) <= maxEndedSessions {
		return
	}
	slices.SortFunc(done, func(x, y *session) int {
		if c := y.LastSeen.Compare(x.LastSeen); c != 0 {
			return c
		}
		return strings.Compare(x.Key, y.Key)
	})
	for _, s := range done[maxEndedSessions:] {
		b.detachSession(s)
	}
}

// trimQuiet drops, of the sessions in quiet, all but the keep of each claim
// heard from last: those of a flood that stalled or went gone together with
// none joining after them, which trimSessions never sees. quiet are the
// sessions a sweep keeps that ended or are gone, or those that stalled, in
// the order they were last heard from. It reports whether it dropped any.
func (b *Board) trimQuiet(quiet []*session, keep int) bool {
	kept := map[string]int{}
	dropped := false
	for _, s := range slices.Backward(quiet) {
		kept[s.ClaimID]++
		if kept[s.ClaimID] > keep {
			b.detachSession(s)
			dropped = true
		}
	}
	return dropped
}

// sessionRoom is how many sessions the board keeps once it has made room for
// more (evictSessions): nine tenths of MaxSessions, so that it makes room
// once for every tenth, not for every session.
func (b *Board) sessionRoom() int { return b.cfg.MaxSessions - max(1, b.cfg.MaxSessions/10) }

// evictSessions drops, when the board holds more than sessionRoom sessions,
// enough of them that it holds sessionRoom: first those that ended or are
// gone, then those that stalled, which the dashboard shows and whose agents
// may yet come back, and only then live ones; and of each, those of the
// member holding the most first, each member's silent longest first
// (fairShare). A flood of fresh session ids fills the board with live
// sessions, each heard from once; were they kept, no new session would find
// room until they went gone, two hours on. Were live ones dropped silent
// longest first whoever held them, the flood would cost each teammate whose
// agent is deep in a long tool call its session, and a reservation it holds
// would stop blocking; it costs the flooding member's own sessions instead.
// An agent whose session was dropped while live starts a new one when it
// next reports, and may be told again what it was told; each is counted in
// its repository's stats (Stats.Evicted). Hook calls it when a new session
// finds the board full, and Sweep when the board is past sessionRoom. It
// reports whether it dropped any.
func (b *Board) evictSessions(now time.Time) bool {
	keep := b.sessionRoom()
	if len(b.sessions) <= keep {
		return false
	}
	var tiers [3][]*session // ended or gone, stalled, live
	for _, s := range b.sessions {
		switch st := b.state(now, s); {
		case st.Live():
			tiers[2] = append(tiers[2], s)
		case st == StateStalled:
			tiers[1] = append(tiers[1], s)
		default:
			tiers[0] = append(tiers[0], s)
		}
	}
	for i, tier := range tiers {
		for _, s := range fairShare(tier, memberOf, bySilence) {
			if len(b.sessions) <= keep {
				return true
			}
			if c := b.claims[s.ClaimID]; i == 2 && c != nil {
				b.statsOf(c.Repo, now).Evicted++
			}
			b.detachSession(s)
		}
	}
	return true
}

// bySilence orders sessions by when each was last heard from, then key.
func bySilence(x, y *session) int {
	if c := x.LastSeen.Compare(y.LastSeen); c != 0 {
		return c
	}
	return strings.Compare(x.Key, y.Key)
}

func memberOf(s *session) string { return s.Member }

// fairShare orders items to be let go of in fair share: each next from the
// owner holding the most of those not let go of yet, that owner's first in
// the order of before (the oldest, say), and between owners holding as
// many, the first in that order. Let go of in this order, an owner never
// loses one while another holds more of them, so a flood of one owner's
// costs that owner's own down to as many as the next owner holds before it
// costs anyone else one. It reorders items in place and returns them.
func fairShare[T any](items []T, owner func(T) string, before func(x, y T) int) []T {
	slices.SortFunc(items, before)
	// An item's height is how many of its owner's are left when its turn
	// comes, itself included. Highest first, and in the order of before
	// among items of one height, is fair share; a counting sort keeps that
	// order in one pass.
	height := make([]int, len(items))
	left, most := map[string]int{}, 0
	for i, it := range slices.Backward(items) {
		o := owner(it)
		left[o]++
		height[i] = left[o]
		most = max(most, height[i])
	}
	next := make([]int, most+1) // where the next item of each height goes
	for _, h := range height {
		next[h]++
	}
	for h, at := most, 0; h > 0; h-- {
		next[h], at = at, at+next[h]
	}
	order := make([]T, len(items))
	for i, it := range items {
		order[next[height[i]]] = it
		next[height[i]]++
	}
	copy(items, order)
	return items
}

// full reports whether ev comes from a session the board does not have and
// has no room for: it holds MaxSessions. Hook then makes room
// (evictSessions).
func (b *Board) full(ev HookEvent) bool {
	return len(b.sessions) >= b.cfg.MaxSessions && b.sessions[sessionKey(ev.Member, ev.Agent, ev.SessionID)] == nil
}

// maxForgetPerSweep bounds the claims one sweep forgets to keep under
// MaxDormantClaims, so a board restored over the bound sheds it a few
// sweeps at a time rather than holding the lock for all of it.
const maxForgetPerSweep = 200

// forgetDormant forgets, when more claims than MaxDormantClaims have no live
// session, of those among quiet, which no longer listen (listening), first
// the ones that hold no intent and then the ones that do, each quiet longest
// first (by UpdatedAt, then ID): at most maxForgetPerSweep, announced in one
// activity per repository rather than one each. A claim still listening is
// not forgotten for the bound, however many there are: its agent left less
// than DormantFor ago, and its changes count in full. quiet is in ID order.
// It reports whether it forgot any.
func (b *Board) forgetDormant(now time.Time, dormant int, quiet []*claim) bool {
	over := min(dormant-b.cfg.MaxDormantClaims, maxForgetPerSweep, len(quiet))
	if over <= 0 {
		return false
	}
	planned := func(c *claim) int { return min(len(c.Intents), 1) }
	slices.SortStableFunc(quiet, func(x, y *claim) int {
		return cmp.Or(planned(x)-planned(y), x.UpdatedAt.Compare(y.UpdatedAt))
	})
	forgot := map[string]int{}
	for _, c := range quiet[:over] {
		b.forget(c)
		forgot[c.Repo]++
	}
	for _, repo := range slices.Sorted(maps.Keys(forgot)) {
		b.record(Activity{At: now, Kind: ActivityClaimForgotten, Repo: repo,
			Text: fmt.Sprintf("%s nobody had worked in for longest forgotten: the board keeps at most %d with no agent running",
				plural(forgot[repo], "claim"), b.cfg.MaxDormantClaims)})
	}
	return true
}

// forget takes claim c off the board, released or forgotten, which its
// caller announces, and notes that it did for the durable part (noteRemoved).
func (b *Board) forget(c *claim) {
	b.removeClaim(c)
	b.unpruned[c.Repo] = true
	b.noteRemoved(c)
}

// fitMembers forgets, of the claims among idle, which have no live session
// and are in ID order, those of each member whose footprints hold more than
// three quarters of MemberFootprintBytes, quiet longest first (by
// UpdatedAt, then ID), until their claims hold three quarters: a member's
// new work matters more than their oldest, which would otherwise fill the
// budget and leave nothing of the new kept, so a sweep leaves room for it.
// A claim with an agent running is not forgotten for it. It announces one
// activity per repository and member, and reports whether it forgot any.
func (b *Board) fitMembers(now time.Time, idle []*claim) bool {
	mark := b.cfg.MemberFootprintBytes - b.cfg.MemberFootprintBytes/4
	var over []*claim
	for _, c := range idle {
		if !c.removed && c.fpBytes > 0 && b.memberBytes[c.Member] > mark {
			over = append(over, c)
		}
	}
	if len(over) == 0 {
		return false
	}
	slices.SortStableFunc(over, func(x, y *claim) int { return x.UpdatedAt.Compare(y.UpdatedAt) })
	type whose struct{ repo, member string }
	forgot := map[whose]int{}
	for _, c := range over {
		if b.memberBytes[c.Member] > mark {
			b.forget(c)
			forgot[whose{c.Repo, c.Member}]++
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(forgot), func(x, y whose) int {
		return cmp.Or(strings.Compare(x.repo, y.repo), strings.Compare(x.member, y.member))
	}) {
		b.record(Activity{At: now, Kind: ActivityClaimForgotten, Repo: k.repo, Member: k.member,
			Text: fmt.Sprintf("%d of %s's claims nobody had worked in for longest forgotten: the board keeps at most %s "+
				"of one member's changed files", forgot[k], k.member, sizeText(b.cfg.MemberFootprintBytes))})
	}
	return true
}

// sizeText says n bytes in megabytes, or in kilobytes below one.
func sizeText(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MB", n>>20)
	}
	return fmt.Sprintf("%d KB", n>>10)
}

// Footprint byte budgets, which footprintCost counts against: what one
// claim's footprint may hold, and what all of one member's may. A footprint
// of 2000 files of the paths a large monorepo has (60 bytes, and an area of
// 20) costs about 350 KB; one of 2000 paths of a kilobyte, 2.2 MB. A member
// runs every one of their agents with one token, a fleet's too.
const (
	defaultFootprintBytes       = 512 << 10
	defaultMemberFootprintBytes = 32 << 20
)

// footprintRoom is how many bytes claim c's footprint may hold: what its own
// budget allows, and what its member's other claims leave of theirs. The
// changes hooks reported may take hookHeadroom past both (hooks).
func (b *Board) footprintRoom(c *claim, hooks bool) int {
	own, member := b.cfg.MaxFootprintBytes, b.cfg.MemberFootprintBytes
	if hooks {
		own, member = hookHeadroom(own), hookHeadroom(member)
	}
	return min(own, member-(b.memberBytes[c.Member]-c.fpBytes))
}

// hookHeadroom is a bound on a footprint, on its files or its bytes, raised
// for the changes hooks report: a hook reports a change as it is made, and
// the board lets go of the files git found to keep one, and past them keeps
// a quarter more, so that it loses none an agent makes in the usual course.
func hookHeadroom(n int) int { return n + n/4 }

// makeRoom lets go of the files git found in claim c's footprint, the
// greatest paths first, as far as it takes to keep files more files, of
// bytes more bytes, that hooks reported within the footprint's bounds; all
// at once, so the footprint is indexed again once.
func (b *Board) makeRoom(c *claim, files, bytes int) {
	overFiles := len(c.Footprint) + files - b.cfg.MaxFootprint
	overBytes := c.fpBytes + bytes - b.footprintRoom(c, false)
	var drop []string
	for i := len(c.sortedPaths) - 1; i >= 0 && (overFiles > 0 || overBytes > 0); i-- {
		p := c.sortedPaths[i]
		if t := c.Footprint[p]; t.FromGit {
			drop = append(drop, p)
			overFiles, overBytes = overFiles-1, overBytes-footprintCost(p, t)
		}
	}
	if len(drop) > 0 {
		b.dropTouches(c, drop)
		c.FootprintTruncated = true
	}
}

// hookFits reports whether claim c's footprint has room for a change a hook
// reported that costs cost, once makeRoom made what room it could: within
// hookHeadroom past the footprint's bounds.
func (b *Board) hookFits(c *claim, cost int) bool {
	return len(c.Footprint) < hookHeadroom(b.cfg.MaxFootprint) && c.fpBytes+cost <= b.footprintRoom(c, true)
}
