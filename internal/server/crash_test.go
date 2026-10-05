package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// What a crash loses, now that the whole board is saved every few minutes
// and its durable part within about a second. Each test plays a server whose
// persister looks as its loop does, with the board's clock and the clock
// that paces saves running together, kills it, and starts another from what
// it left in its data directory.

// crashRun is a persisting server run on its clocks.
type crashRun struct {
	t     *testing.T
	ts    *testServer
	clock *handClock
}

func newCrashRun(t *testing.T) *crashRun {
	t.Helper()
	ts, clock, _ := persistServer(t)
	ts.mu.Lock()
	ts.clock = clock.read() // the board's clock runs with the save clock
	ts.mu.Unlock()
	return &crashRun{t: t, ts: ts, clock: clock}
}

// pass moves both clocks on by d.
func (r *crashRun) pass(d time.Duration) {
	r.clock.add(d)
	r.ts.mu.Lock()
	r.ts.clock = r.ts.clock.Add(d)
	r.ts.mu.Unlock()
}

// run runs the server's persister for d, as its loop does: it saves what is
// due and notes that the server runs, and waits as long as that asks.
func (r *crashRun) run(d time.Duration) {
	end := r.clock.read().Add(d)
	for now := r.clock.read(); now.Before(end); now = r.clock.read() {
		r.pass(min(r.ts.saveIfDue(nil), r.ts.aliveIfDue(), end.Sub(now)))
	}
}

// busy runs the server for d while carol, in a repository of her own,
// changes a file every second: a board that keeps changing, whose whole is
// saved every bulkSaveEvery.
func (r *crashRun) busy(d time.Duration) {
	r.t.Helper()
	w := board.Where{Repo: "github.com/acme/other", Host: "carol-host", Worktree: "/w/carol"}
	for range int(d / time.Second) {
		r.hookOn(r.ts.Board(), board.KindPostEdit, "carol", "c1", w, "x.go")
		r.run(time.Second)
	}
}

// hookOn sends board b a hook of member's session from w.
func (r *crashRun) hookOn(b *board.Board, kind board.Kind, member, session string, w board.Where,
	paths ...string) board.HookResult {
	r.t.Helper()
	ev := hookEv(kind, member, session, paths...)
	ev.Member, ev.Where = member, w
	if kind == board.KindToolStart || kind == board.KindToolEnd {
		ev.Tool = "Bash"
	}
	res, err := b.Hook(r.ts.now(), ev)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// scanned sends a hook of member's session from their worktree, with a scan
// of it that found it clean.
func (r *crashRun) scanned(kind board.Kind, member, session string) {
	r.t.Helper()
	ev := hookEv(kind, member, session)
	ev.Member, ev.Tool, ev.Footprint = member, "Bash", &board.Footprint{}
	if _, err := r.ts.Board().Hook(r.ts.now(), ev); err != nil {
		r.t.Fatal(err)
	}
}

func (r *crashRun) hook(kind board.Kind, member, session string, w board.Where, paths ...string) board.HookResult {
	r.t.Helper()
	return r.hookOn(r.ts.Board(), kind, member, session, w, paths...)
}

// reserve has member's claim at w hold pattern exclusively.
func (r *crashRun) reserve(member string, w board.Where, pattern string) {
	r.t.Helper()
	res, err := r.ts.Board().Declare(r.ts.now(), board.DeclareRequest{Member: member, Where: w, Patterns: []string{pattern},
		Mode: board.ModeExclusive, Summary: "rework " + pattern})
	if err != nil || len(res.Accepted) != 1 {
		r.t.Fatalf("%s reserving %s: %+v %v", member, pattern, res, err)
	}
}

// crash kills the server now, and starts another from its data directory
// after down.
func (r *crashRun) crash(down time.Duration) *board.Board {
	r.t.Helper()
	return r.restart(down).Board()
}

// restart is crash's server.
func (r *crashRun) restart(down time.Duration) *Server {
	r.t.Helper()
	r.pass(down)
	return r.ts.killed(r.t)
}

// sweep has b sweep every sweepEvery for d, as the server does.
func (r *crashRun) sweep(b *board.Board, d time.Duration) {
	for range int(d / (15 * time.Second)) {
		r.pass(15 * time.Second)
		b.Sweep(r.ts.now())
	}
}

// edits is what bob's first two attempts at editing path on board b are
// answered.
func (r *crashRun) edits(b *board.Board, session, path string) []board.Decision {
	r.t.Helper()
	var out []board.Decision
	for range 2 {
		out = append(out, r.hookOn(b, board.KindPreEdit, "bob", session, where("bob"), path).Decision)
	}
	return out
}

// sessionState is the state of member's session id on board b, as its view
// shows it, or "" if it shows none.
func (r *crashRun) sessionState(b *board.Board, member, id string) (board.State, string) {
	for _, c := range b.View(r.ts.now(), repo).Claims {
		for _, s := range c.Sessions {
			if c.Member == member && s.ID == id {
				return s.State, s.Tool
			}
		}
	}
	return "", ""
}

// countKind counts the activities of kind in acts about member, or anyone
// if member is "".
func countKind(acts []board.Activity, kind board.ActivityKind, member string) int {
	n := 0
	for _, a := range acts {
		if a.Kind == kind && (member == "" || a.Member == member) {
			n++
		}
	}
	return n
}

var refused = []board.Decision{board.DecisionRefuse, board.DecisionRefuse}

// A reservation keeps refusing teammates' edits across a crash however its
// agent came to hold it in the minutes since the whole board was last saved,
// and its agent is as live as it was: the durable part holds the live
// sessions of claims that hold reservations, with their claims, phases and
// tool calls, saved within about a second of a change to them. Before, a
// claim made since came back with its reservation and no session; a session
// that moved since, or went into a long tool call, came back as the
// snapshot had it: the reservation bumped bob once and let his retry through,
// and bob could reserve the same files himself.
func TestACrashKeepsReservationsBlocking(t *testing.T) {
	alice2 := board.Where{Repo: repo, Host: "alice-host", Worktree: "/w/alice-2", Branch: "b-alice-2"}
	cases := []struct {
		name          string
		before, after func(r *crashRun)
		// later is how long after the crash bob tries, sweeps running.
		later time.Duration
	}{
		{name: "a new session in a new worktree", after: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a2", alice2)
			r.reserve("alice", alice2, "svc/**")
		}},
		{name: "a new session in a claim whose session had ended", before: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a0", alice2)
			r.hook(board.KindSessionEnd, "alice", "a0", alice2)
		}, after: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a3", alice2)
			r.reserve("alice", alice2, "svc/**")
		}},
		{name: "a session that moved to a new worktree", before: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a4", where("alice"))
		}, after: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a4", alice2)
			r.reserve("alice", alice2, "svc/**")
		}},
		{name: "a reservation held from a worktree its session moved from", before: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a5", where("alice"))
			r.reserve("alice", where("alice"), "svc/**")
		}, after: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a5", alice2)
		}, later: 5 * time.Minute},
		{name: "a long tool call started since", before: func(r *crashRun) {
			r.hook(board.KindPrompt, "alice", "a6", where("alice"))
			r.reserve("alice", where("alice"), "svc/**")
		}, after: func(r *crashRun) {
			r.hook(board.KindToolStart, "alice", "a6", where("alice"))
		}, later: 15 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCrashRun(t)
			if tc.before != nil {
				tc.before(r)
			}
			r.run(time.Second) // the whole board is saved
			r.busy(time.Minute)
			tc.after(r)
			r.busy(2 * time.Minute)
			b := r.crash(20 * time.Second)
			r.sweep(b, tc.later)
			if got := r.edits(b, "b1", "svc/a.go"); !slices.Equal(got, refused) {
				t.Errorf("after the crash bob's edit inside alice's reservation, and his retry: %v, want %v", got, refused)
			}
			res, err := b.Declare(r.ts.now(), board.DeclareRequest{Member: "bob", Where: where("bob"), Patterns: []string{"svc/**"},
				Mode: board.ModeExclusive})
			if err != nil || len(res.Accepted) != 0 {
				t.Errorf("after the crash bob reserved what alice holds: %+v %v", res, err)
			}
			if n := countKind(b.Since(repo, 0), board.ActivitySessionStalled, "alice"); n != 0 {
				t.Errorf("alice's agent was announced stalled %d times", n)
			}
		})
	}
}

// A session in a long tool call started since the whole board was last saved
// is not announced stalled after a crash when it holds no reservation
// either: the durable part does not hold it, so it comes back as working and
// unsure, and may be silent as long as one in a tool may until it reports
// again. Before, it came back working and out of tools, and was announced
// stalled 10 minutes on. The price is that an agent whose silence began
// before the snapshot is announced stalled only that long after it too.
func TestACrashKeepsALongToolCallFromLookingStalled(t *testing.T) {
	r := newCrashRun(t)
	r.hook(board.KindPrompt, "alice", "a1", where("alice"))
	r.hook(board.KindPrompt, "bob", "silent", where("bob"))
	r.run(time.Second)
	r.busy(time.Minute)
	r.hook(board.KindToolStart, "alice", "a1", where("alice")) // a test suite that runs half an hour
	r.busy(3 * time.Minute)
	b := r.crash(time.Minute)
	r.sweep(b, 30*time.Minute)
	if st, _ := r.sessionState(b, "alice", "a1"); st != board.StateWorking {
		t.Errorf("alice's session in its long tool call is %s after the crash, want working", st)
	}
	if n := countKind(b.Since(repo, 0), board.ActivitySessionStalled, ""); n != 0 {
		t.Errorf("%d agents were announced stalled within 30 minutes of the crash", n)
	}
	r.hookOn(b, board.KindToolEnd, "alice", "a1", where("alice"))
	r.sweep(b, 16*time.Minute)
	acts := b.Since(repo, 0)
	if n := countKind(acts, board.ActivitySessionStalled, "alice"); n != 1 {
		t.Errorf("alice's agent silent 16 minutes after its tool call ended was announced stalled %d times, want once", n)
	}
	if n := countKind(acts, board.ActivitySessionStalled, "bob"); n != 1 {
		t.Errorf("bob's agent silent since before the snapshot was announced stalled %d times %s after the crash, want once",
			n, 46*time.Minute)
	}
}

// A session the whole save had working comes back unsure after a crash
// however long it had been silent when the board was saved: its silence,
// credited with the downtime, runs to the crash, minutes longer than at the
// save, and it may have gone into a long tool call meanwhile. Here alice's
// agent prompts, generates for eight and a half minutes, the board being
// saved whole 8 minutes into it, then starts a half-hour test run; the
// server crashes 3 minutes into it. Before, the restart took the credited
// silence of eleven minutes for a stall, and the first sweep announced the
// agent stalled, which it never was without the crash.
func TestACrashKeepsAnAgentSilentAtTheSaveFromLookingStalled(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "no crash", true: "crash"}[crash], func(t *testing.T) {
			r := newCrashRun(t)
			r.run(time.Second)
			r.busy(2 * time.Minute)
			r.hook(board.KindPrompt, "alice", "a1", where("alice"))
			r.busy(8*time.Minute + 30*time.Second) // the whole board saved at 5 and at 10 minutes
			r.hook(board.KindToolStart, "alice", "a1", where("alice"))
			r.busy(3 * time.Minute)
			b := r.ts.Board()
			if crash {
				b = r.crash(30 * time.Second)
			}
			r.sweep(b, 5*time.Minute)
			if n := countKind(b.Since(repo, 0), board.ActivitySessionStalled, "alice"); n != 0 {
				t.Errorf("alice's agent 8 minutes into its tool call was announced stalled %d times", n)
			}
			if st, _ := r.sessionState(b, "alice", "a1"); st != board.StateWorking {
				t.Errorf("alice's agent 8 minutes into its tool call is %s", st)
			}
		})
	}
}

// A session that ended in the minutes before a crash stays ended, and the
// claim its end released stays released: the durable part records both
// until the whole board is saved again. Before, the session came back as
// working with the files it had committed: it was announced stalled 10
// minutes on, and bob's edit of those files was bumped for a day.
func TestACrashKeepsAnEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(r *crashRun)
	}{
		{name: "ended with nothing left to commit", end: func(r *crashRun) {
			r.scanned(board.KindSessionEnd, "alice", "a1") // the worktree is clean
		}},
		{name: "committed, stopped, then ended", end: func(r *crashRun) {
			r.hook(board.KindToolStart, "alice", "a1", where("alice"))
			r.scanned(board.KindToolEnd, "alice", "a1") // git commit: the worktree is clean
			r.busy(10 * time.Second)
			r.scanned(board.KindStop, "alice", "a1")
			r.busy(10 * time.Second)
			r.scanned(board.KindSessionEnd, "alice", "a1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCrashRun(t)
			r.hook(board.KindPrompt, "alice", "a1", where("alice"))
			r.hook(board.KindPostEdit, "alice", "a1", where("alice"), "svc/a.go")
			r.run(time.Second) // the whole board is saved with alice's change
			r.busy(time.Minute)
			tc.end(r)
			r.busy(2 * time.Minute)
			b := r.crash(20 * time.Second)
			r.sweep(b, 11*time.Minute)
			if got := r.hookOn(b, board.KindPreEdit, "bob", "b1", where("bob"), "svc/a.go"); got.Decision != board.DecisionAllow {
				t.Errorf("bob's edit of the file alice committed after the crash: %s\n%s", got.Decision, got.Reason)
			}
			if st, _ := r.sessionState(b, "alice", "a1"); st != "" && st != board.StateEnded {
				t.Errorf("alice's session that ended is %s after the crash", st)
			}
			if n := countKind(b.Since(repo, 0), board.ActivitySessionStalled, "alice"); n != 0 {
				t.Errorf("alice's agent that ended was announced stalled %d times", n)
			}
		})
	}
}

// Once the whole board is saved twice, the durable part no longer says what
// ended before the first of those saves: both board.json and the snapshot
// it replaced, kept as board.json.prev, hold it. After one save it still
// does, for a restart that finds board.json damaged and restores
// board.json.prev.
func TestADurablePartForgetsWhatEndedOnlyOnceBothSnapshotsHoldIt(t *testing.T) {
	r := newCrashRun(t)
	r.hook(board.KindPrompt, "alice", "a1", where("alice"))
	r.run(time.Second)
	r.busy(time.Minute)
	r.hook(board.KindSessionEnd, "alice", "a1", where("alice"))
	r.busy(3 * time.Second)
	if n := len(durableIn(t, r.ts).Ended); n != 1 {
		t.Fatalf("the durable part saved after the end says %d sessions ended, want 1", n)
	}
	r.busy(bulkSaveEvery)
	if d := durableIn(t, r.ts); len(d.Ended) != 1 || len(d.Removed) != 1 {
		t.Errorf("after one whole save with the end, the durable part says %v ended and %v were removed, want both "+
			"for board.json.prev", d.Ended, d.Removed)
	}
	r.busy(bulkSaveEvery)
	if d := durableIn(t, r.ts); len(d.Ended) != 0 || len(d.Removed) != 0 {
		t.Errorf("after two whole saves with the end, the durable part says %v ended and %v were removed", d.Ended, d.Removed)
	}
}

// A restart that finds board.json damaged restores board.json.prev, the
// whole save before it, and the durable part: what ended between the two
// saves stays ended. Here alice edits svc/a.go, the board is saved whole,
// she commits and ends, which releases her claim, and the board is saved
// whole again; then board.json is damaged. Before, each whole save made the
// durable part forget the ends that save held, so board.json.prev came back
// with alice's agent working and her claim holding svc/a.go, and bob's edit
// of the file she had committed was bumped.
func TestARestoreOfThePreviousSnapshotKeepsWhatEndedSince(t *testing.T) {
	r := newCrashRun(t)
	r.hook(board.KindPrompt, "alice", "a1", where("alice"))
	r.hook(board.KindPostEdit, "alice", "a1", where("alice"), "svc/a.go")
	r.run(time.Second) // saved whole, alice at work on svc/a.go
	r.busy(time.Minute)
	r.scanned(board.KindSessionEnd, "alice", "a1") // committed: the claim is released
	r.busy(bulkSaveEvery)                          // saved whole again, with the end
	r.busy(10 * time.Second)
	if err := os.WriteFile(r.ts.snapshotPath(), []byte("\x1f\x8b damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := r.crash(20 * time.Second)
	if st, _ := r.sessionState(b, "alice", "a1"); st != "" && st != board.StateEnded {
		t.Errorf("alice's session that ended between the two whole saves is %s", st)
	}
	if got := r.hookOn(b, board.KindPreEdit, "bob", "b1", where("bob"), "svc/a.go"); got.Decision != board.DecisionAllow {
		t.Errorf("bob's edit of the file alice committed and released: %s\n%s", got.Decision, got.Reason)
	}
}

// savedDurable is what a test reads of a durable part.
type savedDurable struct {
	Ended, Removed []string
	Sessions       []json.RawMessage
}

// durableIn is the durable part saved in ts's data directory.
func durableIn(t *testing.T, ts *testServer) savedDurable {
	t.Helper()
	data, err := os.ReadFile(ts.durablePath())
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var d savedDurable
	if err := json.NewDecoder(zr).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A note queued for a claim made since the whole board was last saved, with
// no reservation to keep it on the board, waits after a crash in its
// member's mailbox for the repository, and their next session there hears
// it. Before, the claim came back with the note and nothing else, and the
// first sweep released it, and the note with it.
func TestACrashKeepsANoteForAClaimMadeSince(t *testing.T) {
	r := newCrashRun(t)
	r.run(time.Second)
	r.busy(time.Minute)
	r.hook(board.KindPostEdit, "bob", "b1", where("bob"), "api/x.go") // a claim of bob's made since
	res, err := r.ts.Board().Note(r.ts.now(), board.NoteRequest{Member: "alice", Where: where("alice"), To: "bob",
		Text: "do not touch api/ until I merge", ToMember: true})
	if err != nil || len(res.Delivered) != 1 {
		t.Fatalf("note: %+v %v", res, err)
	}
	r.busy(3 * time.Second)
	b := r.crash(20 * time.Second)
	r.sweep(b, 30*time.Second) // which released the claim the note came back in
	if got := r.hookOn(b, board.KindPrompt, "bob", "b1", where("bob")); !strings.Contains(got.Context, "do not touch api/") {
		t.Errorf("bob's agent, after the crash, was not told alice's note: %q", got.Context)
	}
}

// A durable part saved before an older server's snapshot, which records no
// version of the board, is set aside: a rollback to that server and an
// upgrade again left it, and it would take away the reservations made under
// the older server and bring back those released. One saved after such a
// snapshot is applied: the first save of a server upgraded from it writes
// the durable part before the whole board.
func TestADurablePartOlderThanAnOlderServersSnapshotIsSetAside(t *testing.T) {
	// olderSnapshot writes, as the board's snapshot, one an older server
	// saved at saved, in which bob holds new/** exclusively.
	olderSnapshot := func(t *testing.T, ts *testServer, saved time.Time) {
		t.Helper()
		older := board.New(board.DefaultConfig())
		if _, err := older.Declare(saved, board.DeclareRequest{Member: "bob", Where: where("bob"), Patterns: []string{"new/**"},
			Mode: board.ModeExclusive}); err != nil {
			t.Fatal(err)
		}
		data, _, err := older.Snapshot(saved)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "version") // which an older server does not write
		if data, err = json.Marshal(fields); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ts.snapshotPath(), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	intents := func(s *Server, now time.Time) []string {
		var out []string
		for _, c := range s.Board().View(now, repo).Claims {
			for _, in := range c.Intents {
				out = append(out, c.Member+":"+in.Pattern)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		name    string
		olderAt time.Duration // when the older server saved, after the durable part
		// upgraded is whether the server that saved the durable part ran on
		// from the older server's snapshot, or before it.
		upgraded bool
		want     []string
		aside    bool
	}{
		{name: "rolled back and upgraded again", olderAt: time.Hour, want: []string{"bob:new/**"}, aside: true},
		{name: "upgraded and killed between the first two saves", olderAt: -time.Minute, upgraded: true,
			want: []string{"alice:old/**", "bob:new/**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCrashRun(t)
			r.hook(board.KindPrompt, "bob", "b1", where("bob"))
			if tc.upgraded {
				r.reserve("bob", where("bob"), "new/**")
			}
			r.hook(board.KindPrompt, "alice", "a1", where("alice"))
			r.reserve("alice", where("alice"), "old/**")
			r.run(time.Second) // both saved, the durable part first
			olderSnapshot(t, r.ts, r.ts.now().Add(tc.olderAt))
			r.pass(tc.olderAt)
			s := r.restart(time.Minute)
			if got := intents(s, r.ts.now()); !slices.Equal(got, tc.want) {
				t.Errorf("the intents restored are %v, want %v", got, tc.want)
			}
			stale, _ := filepath.Glob(s.durablePath() + ".stale-*")
			if _, err := os.Stat(s.durablePath()); (len(stale) == 1) != tc.aside || (err == nil) == tc.aside {
				t.Errorf("set aside: %v (%v), want %t", stale, err, tc.aside)
			}
		})
	}
}
