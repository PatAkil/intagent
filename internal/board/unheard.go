package board

import "time"

// Answers nobody hears. A hook client waits a short time and then lets the
// agent go ahead; under load the board can get to an event after that.
// Deciding it then would spend a one-time answer on nobody, and count and
// announce a refusal that never happened.

// advisory reports whether an event only asks something: its answer is its
// point, and the session's later events record what it would have. Every
// other event reports a fact, which is recorded even when nobody hears the
// answer.
func (ev HookEvent) advisory() bool {
	switch ev.Kind {
	case KindPreEdit, KindToolStart:
		return true
	case KindHeartbeat:
		return ev.Footprint == nil
	}
	return false
}

// abandon answers an advisory event that came too late. It changes nothing
// but the count of edits that went ahead unchecked.
func (b *Board) abandon(now time.Time, ev HookEvent) HookResult {
	if ev.Kind == KindPreEdit {
		b.statsOf(ev.Where.Repo, now).Unheard++
		b.changed()
	}
	return HookResult{Decision: DecisionAllow, Unchecked: true}
}
