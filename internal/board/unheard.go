package board

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Answers nobody hears. A hook client waits a short time and then lets the
// agent go ahead; under load the board can get to an event after that.
// Deciding it then would spend a one-time answer on nobody, and count and
// announce a refusal that never happened. And an answer can be lost for other
// reasons: the server down, a connection refused, a reply cut off.

// maxRefused bounds the refused calls a session remembers.
const maxRefused = 8

// abandon answers a pre_edit that came too late. A pre_edit only asks
// whether an edit may go ahead: its answer is its point, the edit has gone
// ahead without it, and the edit's post_edit reports what it did. So it
// changes nothing but the count of edits that went ahead unchecked.
//
// Every other event reports a fact, which is recorded even when nobody hears
// the answer: a tool started, a session alive, an edit made. A tool start's
// answer carries nothing anyway, and dropping it would make a session deep in
// a long shell command look stalled, and its reservations stop blocking
// teammates' edits.
func (b *Board) abandon(now time.Time, ev HookEvent) HookResult {
	b.statsOf(ev.Where.Repo, now).Unheard++
	b.changed()
	return HookResult{Decision: DecisionAllow, Unchecked: true}
}

// refuseTool ends a refused tool call and remembers it, and the one-time
// answers its check spent, among the session's last few: a post_edit for it
// would mean the refusal never reached the agent.
func refuseTool(s *session, id string, spent []string) {
	endTool(s, id)
	if id == "" {
		return
	}
	s.refused = append(s.refused, refusal{id: id, spent: spent})
	if over := len(s.refused) - maxRefused; over > 0 {
		s.refused = slices.Delete(s.refused, 0, over)
	}
}

// unansweredEdit handles a post_edit for a call the board did not see start,
// or saw refused: the agent made the edit without hearing a check of it. Its
// pre_edit never arrived, arrived after the agent stopped waiting, was let
// through unchecked by a server too busy to check it, or was answered with a
// refusal that did not reach the agent.
//
// What a refusal spent is unspent, and only that: the next attempt is
// refused again and heard, while bumps the agent heard before stay spent. The
// edit is then judged without acknowledging anything, and what the agent
// would have been told is held for it, as information; a change inside an
// active reservation is recorded as a breach.
func (b *Board) unansweredEdit(now time.Time, c *claim, s *session, ev HookEvent) {
	if i := slices.IndexFunc(s.refused, func(r refusal) bool { return r.id == ev.ToolUseID }); i >= 0 {
		for _, k := range s.refused[i].spent {
			delete(s.Acked, k)
		}
		s.refused = slices.Delete(s.refused, i, i+1)
	}
	// Judged as its pre_edit would have been, on the first maxCheckPaths
	// paths: a post_edit keeps up to MaxFootprint, and each path judged costs
	// a pass over the repository's claims under the lock.
	told := b.wouldTell(now, c, s, ev.Paths[:min(len(ev.Paths), maxCheckPaths)], ev.Worker)
	if len(told) == 0 {
		return
	}
	s.Pending = joinBlocks(s.Pending, renderUnanswered(now, told))
	b.recordUncheckedBreach(now, c, s, told)
}

// wouldTell is what a check of paths would tell the session's worker now,
// judged on a copy so that nothing is acknowledged.
func (b *Board) wouldTell(now time.Time, c *claim, s *session, paths []PathRef, worker string) []Conflict {
	probe := *s
	probe.Acked = maps.Clone(s.Acked)
	if probe.Acked == nil {
		probe.Acked = map[string]bool{}
	}
	return b.judge(now, c, &probe, paths, true, worker).acted()
}

// recordUncheckedBreach records, once per file and reservation, an edit made
// without a check inside a teammate's active exclusive intent, as
// reportUnchecked does for changes git finds.
func (b *Board) recordUncheckedBreach(now time.Time, c *claim, s *session, told []Conflict) {
	action := b.cfg.Policy.action(SeverityBlock)
	var paths []string
	var first Conflict
	for _, cf := range told {
		if cf.Severity != SeverityBlock || cf.SameClaim {
			continue
		}
		if !c.alert(uncheckedKey(action, cf)) {
			continue
		}
		if len(paths) == 0 {
			first = cf
		}
		if !slices.Contains(paths, cf.Path) {
			paths = append(paths, cf.Path)
		}
	}
	if len(paths) == 0 {
		return
	}
	why := "holds it exclusively" // a breach's kind already says it was not checked
	if action == ActionWarn {
		why = "holds it exclusively; the change was not checked"
	}
	b.record(Activity{At: now, Kind: ActivityConflict, Repo: c.Repo, Member: c.Member, ClaimID: c.ID, Session: s.ID, Agent: s.Agent,
		Paths: paths, Severity: SeverityBlock, Decision: DecisionAllow, Breach: action != ActionWarn,
		Text: fmt.Sprintf("%s → %s (%s)", first.Path, first.Member, why)})
}

// uncheckedKey remembers in Alerted that an unchecked change to a file inside
// a reservation was reported under a policy action. reportUnchecked and
// recordUncheckedBreach both keep it, so a change is reported once whichever
// finds it.
func uncheckedKey(action Action, cf Conflict) string {
	return fmt.Sprintf("unchecked|%s|%s|%s|%d|%s", action, cf.ClaimID, cf.Pattern, cf.Since.UnixNano(), cf.Path)
}

// renderUnanswered tells an agent what a check of an edit it already made
// would have said.
func renderUnanswered(now time.Time, cs []Conflict) string {
	var paths []string
	reserved := false
	for _, cf := range cs {
		if !slices.Contains(paths, cf.Path) {
			paths = append(paths, cf.Path)
		}
		reserved = reserved || cf.Severity == SeverityBlock
	}
	lines := []string{fmt.Sprintf("%s Your edit of %s was not checked before it ran: the team server was too busy to check it, "+
		"or its answer did not reach your agent in time. Other work on it %s:", prefix, listPaths(paths, 3), dataNotice)}
	for i, cf := range cs {
		if i == 4 {
			lines = append(lines, fmt.Sprintf("- and %d more.", len(cs)-4))
			break
		}
		lines = append(lines, describeConflict(now, cf))
	}
	if reserved {
		lines = append(lines, "A teammate reserved it while their agent is active. Undo your change if it was not meant for that file, "+
			"or tell your user so they can agree it with that teammate.")
	} else {
		lines = append(lines, "Keep your change compatible with theirs, and tell them with the intagent send_note tool if it affects their work.")
	}
	return strings.Join(lines, "\n")
}
