package board

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// The board's bounds on what it keeps, whatever its clients send: sessions,
// claims nobody works in, and the files claims changed. Each keeps what an
// agent still needs, and lets go of the rest in an order that does not
// depend on the order of a map. None refuses a hook: an event the board
// cannot store is still answered, and an edit still checked.

// maxEndedSessions bounds the sessions that ended, or went silent for good,
// a claim keeps; only the dashboard shows them, and a client that starts a
// session for every event would otherwise leave thousands on one claim for
// liveness to read through. A claim may hold up to twice as many between
// trims (trimSessions), and each sweep trims every claim to it (trimEnded).
const maxEndedSessions = 20

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

// trimEnded drops, of each claim's sessions that ended or are gone, all but
// the maxEndedSessions heard from last: those of a flood that went gone
// together with none joining after them, which trimSessions never sees. done
// are the sessions the sweep keeps that ended or are gone, in the order they
// were last heard from; one that stalled is kept, as the dashboard shows it. Since the claims are trimmed, each looks afresh when the next
// session joins it. It reports whether it dropped any.
func (b *Board) trimEnded(done []*session) bool {
	kept := map[string]int{}
	dropped := false
	for _, s := range slices.Backward(done) {
		kept[s.ClaimID]++
		if kept[s.ClaimID] > maxEndedSessions {
			b.detachSession(s)
			dropped = true
		}
	}
	for _, c := range b.claims {
		c.trimAfter = 0
	}
	return dropped
}

// full reports whether ev comes from a session the board does not have and
// has no room for: it holds MaxSessions. Sweep makes room by dropping the
// sessions longest silent of those not live (evictSessions).
func (b *Board) full(ev HookEvent) bool {
	return len(b.sessions) >= b.cfg.MaxSessions && b.sessions[sessionKey(ev.Member, ev.Agent, ev.SessionID)] == nil
}

// unstored answers an event from a session the board has no room for,
// storing nothing: the board's sessions are a flood's, most likely, and a
// legitimate agent caught behind it is still checked. An edit is judged as
// a check judges it, against the claim of its worktree if there is one;
// with no session to remember an acknowledgement, what would refuse it once
// (a bump, or a question to an agent that cannot ask) warns instead, so
// that the agent is never refused for good. Its session start says what
// its teammates will not hear.
func (b *Board) unstored(now time.Time, ev HookEvent) HookResult {
	b.statsOf(ev.Where.Repo, now).Unstored++
	res := HookResult{Decision: DecisionAllow}
	switch ev.Kind {
	case KindPreEdit:
		self := b.findClaim(ev.Member, ev.Where)
		if self == nil {
			self = &claim{Repo: ev.Where.Repo, Member: ev.Member}
		}
		probe := &session{Key: sessionKey(ev.Member, ev.Agent, ev.SessionID), Acked: map[string]bool{}}
		v := b.judge(now, self, probe, ev.Paths[:min(len(ev.Paths), maxCheckPaths)], ev.NoAsk)
		res = b.answer(now, probe, b.warnOnce(v, ev.NoAsk))
		res.ClaimID = self.ID
	case KindSessionStart:
		res.Context = fmt.Sprintf("%s The team's intagent server keeps at most %d agent sessions and has no room for "+
			"this one, so your teammates' agents will not hear of the files this session changes; intagent still "+
			"checks your edits against their work. Tell your user, so they can let whoever runs the server know.",
			prefix, b.cfg.MaxSessions)
	}
	return res
}

// warnOnce turns what a verdict refuses or asks only once, which a session
// would remember was said, into warnings: a bump, and a question to an
// agent that cannot ask. A deny still refuses, and a question to an agent
// that can ask is still its person's to answer.
func (b *Board) warnOnce(v verdict, noAsk bool) verdict {
	refused := v.refused[:0:0]
	for _, cf := range v.refused {
		if b.cfg.Policy.action(cf.Severity) == ActionDeny {
			refused = append(refused, cf)
		} else {
			v.warned = append(v.warned, cf)
		}
	}
	v.refused, v.bumpKeys = refused, nil
	if noAsk {
		v.warned, v.asked, v.askKeys = append(v.warned, v.asked...), nil, nil
	}
	return v
}

// evictSessions drops, when the board holds more than nine tenths of
// MaxSessions, the sessions not live that went silent longest, in keys'
// order (by when each was last heard from), until it holds nine tenths:
// so that a new agent finds room until the next sweep. It reports whether
// it dropped any.
func (b *Board) evictSessions(now time.Time, keys []string) bool {
	keep := b.cfg.MaxSessions - b.cfg.MaxSessions/10
	dropped := false
	for _, k := range keys {
		if len(b.sessions) <= keep {
			break
		}
		if s := b.sessions[k]; s != nil && !b.state(now, s).Live() {
			b.detachSession(s)
			dropped = true
		}
	}
	return dropped
}

// maxForgetPerSweep bounds the claims one sweep forgets to keep under
// MaxDormantClaims, so a board restored over the bound sheds it a few
// sweeps at a time rather than holding the lock for all of it.
const maxForgetPerSweep = 200

// forgetDormant forgets, when more claims than MaxDormantClaims have no live
// session, the ones quiet longest (by UpdatedAt, then ID) of those, among
// idle, that hold no intent: at most maxForgetPerSweep, announced in one
// activity per repository rather than one each. idle is in ID order. It
// reports whether it forgot any.
func (b *Board) forgetDormant(now time.Time, dormant int, idle []*claim) bool {
	over := min(dormant-b.cfg.MaxDormantClaims, maxForgetPerSweep, len(idle))
	if over <= 0 {
		return false
	}
	slices.SortStableFunc(idle, func(x, y *claim) int { return x.UpdatedAt.Compare(y.UpdatedAt) })
	forgot := map[string]int{}
	for _, c := range idle[:over] {
		b.removeClaim(c)
		b.unpruned[c.Repo] = true
		forgot[c.Repo]++
	}
	for _, repo := range slices.Sorted(maps.Keys(forgot)) {
		b.record(Activity{At: now, Kind: ActivityClaimForgotten, Repo: repo,
			Text: fmt.Sprintf("%s nobody had worked in for longest forgotten: the board keeps at most %d with no agent running",
				plural(forgot[repo], "claim"), b.cfg.MaxDormantClaims)})
	}
	return true
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
