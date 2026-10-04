package board

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// Snapshots share the claims' footprints with the board, and are encoded
// after the lock is released, so a footprint is copied on write. Eight
// writers change footprints in every way a hook can (a new file, a newer
// touch, an area that moves, a footprint replaced) while snapshots are taken
// and restored; under -race the detector must find nothing.
func TestSnapshotsShareFootprintsWithoutRaces(t *testing.T) {
	shareFootprintsUnderLoad(t, "")
}

// raceControl names the environment variable that has
// TestSnapshotSharingControl play the control in a process of its own.
const raceControl = "INTAGENT_TEST_RACE_CONTROL"

// The control for the test above: there, a claim that forgot its footprint
// was shared, and so changed it in place as a missed copy on write would,
// must make the race detector report it. Otherwise the test above could not
// fail. The control runs in a process of its own, which must fail.
func TestSnapshotSharingControl(t *testing.T) {
	if forget := os.Getenv(raceControl); forget != "" {
		shareFootprintsUnderLoad(t, forget)
		return
	}
	if !raceEnabled {
		t.Skip("the control needs the race detector (go test -race)")
	}
	for forget, writer := range map[string]string{"footprint": "putTouch", "alerted": "alert"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSnapshotSharingControl$", "-test.count=1")
		cmd.Env = append(os.Environ(), raceControl+"="+forget)
		out, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(out, []byte("WARNING: DATA RACE")) || !bytes.Contains(out, []byte("(*claim)."+writer+"(")) {
			t.Errorf("writing a shared %s in place went unreported (err %v):\n%s", forget, err, out)
		}
	}
}

// shareFootprintsUnderLoad runs eight writers against snapshots and
// restores. With forget set, a ninth goroutine keeps clearing every claim's
// mark that a snapshot shares its footprint, or its alerts.
func shareFootprintsUnderLoad(t *testing.T, forget string) {
	h := newHarness(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := fmt.Sprintf("m%d", g)
			for i := range 150 {
				now := t0.Add(time.Duration(i) * time.Second)
				file := PathRef{Path: fmt.Sprintf("p/%d.go", i%9), Area: fmt.Sprintf("p%d", i%4)}
				_, _ = h.b.Hook(now, HookEvent{Kind: KindPostEdit, Member: m, Agent: AgentCodex, SessionID: "s", Where: whereOf(m),
					Paths: []PathRef{file}})
				if i%10 == 9 {
					_, _ = h.b.Hook(now, HookEvent{Kind: KindHeartbeat, Member: m, Agent: AgentCodex, SessionID: "s", Where: whereOf(m),
						Footprint: &Footprint{Files: []PathRef{file, {Path: "q/1.go", Area: "q"}}}})
				}
			}
		}()
	}
	if forget != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.b.mu.Lock()
				for _, c := range h.b.claims {
					if forget == "footprint" {
						c.fpShared = false
					} else {
						c.alertedShared = false
					}
				}
				h.b.mu.Unlock()
			}
		}()
	}
	for i := range 60 {
		data, _, err := h.b.Snapshot(t0)
		if err != nil {
			t.Fatal(err)
		}
		if i%10 == 9 {
			if err := h.b.Restore(bytes.NewReader(data), t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if err := h.b.checkIndexes(); err != nil {
		t.Fatal(err)
	}
}

// The copy a snapshot takes under the lock does not grow with the files the
// claims changed: it shares their footprints, the alerts they heard about
// the files and the files their inbox items name. It allocated a touch for
// each file, and copied the rest.
func TestSnapshotCopyDoesNotGrowWithFootprints(t *testing.T) {
	allocs := func(files int) float64 {
		h := newHarness(t)
		for m := range 20 {
			fp := &Footprint{}
			for f := range files {
				fp.Files = append(fp.Files, PathRef{Path: fmt.Sprintf("a%d/f%d.go", f%7, f), Area: fmt.Sprintf("a%d", f%7)})
			}
			// The same files for all, so each tells the others about all of them.
			member := fmt.Sprintf("m%d", m)
			if _, err := h.b.Hook(t0, HookEvent{Kind: KindHeartbeat, Member: member, Agent: AgentCodex, SessionID: "s",
				Where: whereOf(member), Footprint: fp}); err != nil {
				t.Fatal(err)
			}
		}
		return testing.AllocsPerRun(5, func() { h.b.snapshotCopy(t0) })
	}
	if one, many := allocs(1), allocs(500); many > one {
		t.Fatalf("a snapshot's copy allocates %.0f times with 500 files a claim, %.0f with one", many, one)
	}
}

// A snapshot written a claim and a session at a time is, byte for byte,
// the old snapshot marshalled whole, on an empty board, a kit board with
// intents, inboxes, alerts and dormant claims, and a board whose feed
// dropped activities; and a board restored from it writes it again.
func TestWriteSnapshotMatchesMarshal(t *testing.T) {
	small := New(DefaultConfig(), withIDs(func(prefix string) string { return prefix + "1" }))
	kit := buildBoard(t, smallShape())
	dropped := newHarness(t, func(c *Config) { c.KeepActivities = 3 })
	for i := range 8 {
		dropped.hook(KindPostEdit, "alice", "a1", fmt.Sprintf("x/%d.go", i))
	}
	if _, err := dropped.b.Note(t0, NoteRequest{Member: "alice", Where: whereOf("alice"), To: "bob", Text: "hi", ToMember: true}); err != nil {
		t.Fatal(err) // a note held for bob, whom the board does not know yet
	}
	for name, b := range map[string]*Board{"empty": small, "kit": kit.Board, "dropped": dropped.b} {
		want, wantVersion, err := b.oldSnapshot(t0)
		if err != nil {
			t.Fatal(err)
		}
		got, version, err := b.Snapshot(t0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) || version != wantVersion {
			t.Fatalf("%s: the snapshot differs (version %d, want %d):\n got %.600s\nwant %.600s", name, version, wantVersion, got, want)
		}
		restored := New(DefaultConfig())
		if err := restored.Restore(bytes.NewReader(got), t0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, _, err := restored.Snapshot(t0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, got) {
			t.Fatalf("%s: a restored board writes another snapshot:\n got %.600s\nwant %.600s", name, again, got)
		}
	}
}

// A write that fails ends the snapshot with its error.
func TestWriteSnapshotReportsWriteErrors(t *testing.T) {
	b := buildBoard(t, smallShape())
	broken := errors.New("disk full")
	if _, err := b.WriteSnapshot(failingWriter{after: snapshotBuffer, err: broken}, t0); !errors.Is(err, broken) {
		t.Fatalf("WriteSnapshot = %v, want the write's error", err)
	}
}

type failingWriter struct {
	after int
	err   error
}

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }

// Writing a snapshot holds a small part of it in memory at once: one claim
// or session is encoded at a time, into a buffer that is written out when
// full. Marshalled whole, all of it was held, and several times its size
// allocated. What is held is measured at each write, after a collection.
func TestWriteSnapshotHoldsLittleOfIt(t *testing.T) {
	sh := smallShape()
	sh.files, sh.dist = 1000, filesFixed
	b := buildBoard(t, sh)
	held := func(write func(w io.Writer)) (size int, extra uint64) {
		var lw liveWriter
		lw.collect()
		base := lw.m.HeapAlloc
		write(&lw)
		return lw.n, lw.peak - min(base, lw.peak)
	}
	size, streamed := held(func(w io.Writer) { _, _ = b.WriteSnapshot(w, t0) })
	_, whole := held(func(w io.Writer) {
		data, _, _ := b.oldSnapshot(t0)
		_, _ = w.Write(data)
	})
	runtime.KeepAlive(b) // the board is live throughout, in both
	t.Logf("a snapshot of %d bytes: %d bytes held at once streamed, %d marshalled whole", size, streamed, whole)
	if 4*streamed > uint64(size) || 2*whole < uint64(size) {
		t.Fatalf("writing a snapshot of %d bytes held %d at once (%d marshalled whole)", size, streamed, whole)
	}
}

// liveWriter discards what it is written, and notes the most heap in use
// that a write finds, after a collection.
type liveWriter struct {
	m    runtime.MemStats
	n    int
	peak uint64
}

func (l *liveWriter) Write(p []byte) (int, error) {
	l.collect()
	l.n += len(p)
	l.peak = max(l.peak, l.m.HeapAlloc)
	runtime.KeepAlive(p) // held by the writer's caller, as by a file's
	return len(p), nil
}

// collect reads the heap in use after a collection, twice over so that what
// sync.Pool keeps for one more is gone too.
func (l *liveWriter) collect() {
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&l.m)
}

// A snapshot decoded a claim at a time is what json.Unmarshal made of it:
// the same snapshot from valid ones, written by WriteSnapshot or by hand
// with unknown, repeated, null and differently cased fields; and an error
// from every one json.Unmarshal refused, including every prefix of a valid
// snapshot, as a crash or a full disk would leave it.
func TestReadSnapshotMatchesUnmarshal(t *testing.T) {
	data, _, err := buildBoard(t, smallShape()).Snapshot(t0)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		string(data),
		`{"format":1}`,
		`{"format":1,"claims":null,"sessions":[],"recent":null}`,
		`{"Format":1,"CLAIMS":[{"id":"c1","repo":"r"},null],"later":{"x":[1,2]},"seq":4}`,
		`{"format":1,"claims":[{"id":"c1"}],"claims":[{"id":"c2"},{"id":"c3"}]}`,
		` {"format":1,"stats":{"r":{"edits":2}},"dropped":{"all":3}} ` + "\n",
		`{"format":2,"claims":[{"id":"c1"}]}`,
		`{"format":0}`,
		`{"claims":[]}`,
		`{"format":1,"claims":{}}`,
		`{"format":1,"claims":[1]}`,
		`{"format":1,"seq":"x"}`,
		`{"format":1}{}`,
		`{"format":1} x`,
		`[]`,
		`null`,
		``,
		`not json`,
	}
	for n := 1; n < len(data); n += 1 + n/7 {
		cases = append(cases, string(data[:n]))
	}
	for _, c := range cases {
		want, wantErr := oldReadSnapshot([]byte(c))
		got, err := readSnapshot(strings.NewReader(c))
		if (err != nil) != (wantErr != nil) {
			t.Fatalf("%.80q: error %v, json.Unmarshal's %v", c, err, wantErr)
		}
		if err == nil && jsonOf(got) != jsonOf(want) {
			t.Fatalf("%.80q: decoded\n%s\nwant\n%s", c, jsonOf(got), jsonOf(want))
		}
	}
}

// Restore reads a snapshot in small pieces: what it holds of the file at
// once is about one claim. Read whole, as it was, a read asked for a fifth
// of the file and more.
func TestRestoreReadsInPieces(t *testing.T) {
	sh := smallShape()
	sh.claims, sh.busy, sh.sessions, sh.repos, sh.files, sh.dist = 200, 20, 200, 40, 40, filesFixed
	data, _, err := buildBoard(t, sh).Snapshot(t0)
	if err != nil {
		t.Fatal(err)
	}
	r := &readSizes{r: bytes.NewReader(data)}
	if err := New(DefaultConfig()).Restore(r, t0); err != nil {
		t.Fatal(err)
	}
	t.Logf("a snapshot of %d bytes restored in reads of at most %d", len(data), r.largest)
	if r.largest*20 > len(data) {
		t.Fatalf("a read of %d bytes, of a snapshot of %d", r.largest, len(data))
	}
}

// readSizes notes the largest read asked of it.
type readSizes struct {
	r       io.Reader
	largest int
}

func (r *readSizes) Read(p []byte) (int, error) {
	r.largest = max(r.largest, len(p))
	return r.r.Read(p)
}

// A file found in git names no session, which nothing reads, and so a
// snapshot does not carry one for each: a third of its size. A file a hook
// reported names its session, which tells whether another session of the
// worktree made the change.
func TestGitTouchesNameNoSession(t *testing.T) {
	h := newHarness(t)
	if _, err := h.b.Hook(h.now, HookEvent{Kind: KindHeartbeat, Member: "alice", Agent: AgentCodex, SessionID: "a1",
		Where: whereOf("alice"), Footprint: &Footprint{Files: refs("a/git.go")}}); err != nil {
		t.Fatal(err)
	}
	h.edit("alice", "a1", "a/hook.go")
	c := h.b.findClaim("alice", whereOf("alice"))
	if git, hook := c.Footprint["a/git.go"], c.Footprint["a/hook.go"]; git.Session != "" || hook.Session == "" {
		t.Fatalf("sessions: git %q, hook %q", git.Session, hook.Session)
	}
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	claims := data[bytes.Index(data, []byte(`"claims":`)):bytes.Index(data, []byte(`"sessions":`))]
	if n := bytes.Count(claims, []byte(`"session":`)); n != 1 {
		t.Fatalf("the snapshot's claims name a session %d times, want once:\n%s", n, claims)
	}
}

// A snapshot from an older server, whose touches found in git name the
// session that found them, is restored without them, and with one string
// for each session however many touches name it; a null touch is dropped.
func TestRestoreSharesSessionKeys(t *testing.T) {
	key := sessionKey("alice", AgentCodex, "a1")
	old := fmt.Sprintf(`{"format":1,"claims":[{"id":"c1","repo":%q,"member":"alice","host":"h","worktree":"/w","footprint":{`+
		`"a/git.go":{"at":"2026-10-02T09:00:00Z","session":%q,"from_git":true},`+
		`"a/one.go":{"at":"2026-10-02T09:00:00Z","session":%q},"a/two.go":{"at":"2026-10-02T09:00:00Z","session":%q},`+
		`"a/gone.go":{"at":"2026-10-02T09:00:00Z","session":"swept"},"a/null.go":null}}],`+
		`"sessions":[{"key":%q,"id":"a1","member":"alice","agent":"codex","claim_id":"c1","last_seen":"2026-10-02T09:00:00Z","phase":"working"}]}`,
		repo, key, key, key, key)
	b := New(DefaultConfig())
	if err := b.Restore(strings.NewReader(old), t0); err != nil {
		t.Fatal(err)
	}
	fp := b.claims["c1"].Footprint
	s := b.sessions[key]
	if fp["a/git.go"].Session != "" || fp["a/gone.go"].Session != "swept" || len(fp) != 4 {
		t.Fatalf("restored footprint: %s", jsonOf(fp))
	}
	for _, p := range []string{"a/one.go", "a/two.go"} {
		if got := fp[p].Session; got != s.Key || unsafe.StringData(got) != unsafe.StringData(s.Key) {
			t.Errorf("%s names its session with a string of its own", p)
		}
	}
	if err := b.checkIndexes(); err != nil {
		t.Fatal(err)
	}
}

// A snapshot cut short or damaged is reported as corrupt, which a server may
// set aside; a newer format, or a read that failed, is not, and a server
// must stop rather than lose it. Either way the board is left as it was.
func TestRestoreTellsDamageFromOtherErrors(t *testing.T) {
	h := newHarness(t)
	h.edit("alice", "a1", "a/b.go")
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig())
	if err := b.Restore(bytes.NewReader(data), h.now); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("input/output error")
	for _, c := range []struct {
		name    string
		r       io.Reader
		corrupt bool
	}{
		{"cut short", bytes.NewReader(data[:len(data)/2]), true},
		{"garbage", strings.NewReader("\x00\x00\x00"), true},
		{"empty", strings.NewReader(""), true},
		{"no format", strings.NewReader(`{"claims":[]}`), true},
		{"format 0", strings.NewReader(`{"format":0}`), true},
		{"two objects", strings.NewReader(`{"format":1}{"format":1}`), true},
		{"newer", strings.NewReader(`{"format":2,"claims":"anything"}`), false},
		{"unreadable", io.MultiReader(bytes.NewReader(data[:100]), failingReader{broken}), false},
	} {
		err := b.Restore(c.r, h.now)
		if err == nil || errors.Is(err, ErrCorruptSnapshot) != c.corrupt {
			t.Errorf("%s: %v, want corrupt %t", c.name, err, c.corrupt)
		}
		if c.name == "unreadable" && !errors.Is(err, broken) {
			t.Errorf("%s: %v does not carry the read's error", c.name, err)
		}
	}
	if len(b.claims) != 1 || len(b.sessions) != 1 {
		t.Fatalf("a failed restore changed the board: %d claims, %d sessions", len(b.claims), len(b.sessions))
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// A board restored 50 minutes after its snapshot does not count the outage
// as its agents' silence: the first sweep announces nobody, and a working
// agent's reservation still refuses a teammate's edit. An agent that never
// comes back is announced stalled once stall_after has passed since the
// restore. Before, the first sweep announced every working session, and
// their reservations only bumped a teammate's edit, once.
func TestRestoreCreditsTheDowntime(t *testing.T) {
	h := newHarness(t)
	h.hook(KindPrompt, "alice", "a1")
	h.declare("alice", ModeExclusive, "retry", "svc/**")
	data, _, err := h.b.Snapshot(h.now)
	if err != nil {
		t.Fatal(err)
	}
	var acts []Activity
	b := New(DefaultConfig(), WithNotify(func(a []Activity) { acts = append(acts, a...) }))
	restart := h.now.Add(50 * time.Minute)
	if err := b.Restore(bytes.NewReader(data), restart); err != nil {
		t.Fatal(err)
	}
	b.Sweep(restart.Add(15 * time.Second))
	res, err := b.Hook(restart.Add(20*time.Second), HookEvent{Kind: KindPreEdit, Member: "bob", Agent: AgentCodex, SessionID: "b1",
		Where: whereOf("bob"), Paths: refs("svc/a.go")})
	if err != nil || res.Decision != DecisionRefuse || len(res.Conflicts) == 0 || res.Conflicts[0].Severity != SeverityBlock {
		t.Fatalf("bob's edit inside alice's reservation after the restart: %s %v %+v", res.Decision, err, res.Conflicts)
	}
	stall := DefaultConfig().StallAfter
	b.Sweep(restart.Add(stall - time.Second))
	for _, a := range acts {
		if a.Kind == ActivitySessionStalled || a.Kind == ActivitySessionGone {
			t.Fatalf("announced %s %s before stall_after had passed since the restart", a.Member, a.Kind)
		}
	}
	b.Sweep(restart.Add(stall + time.Second))
	if n := len(slices.DeleteFunc(acts, func(a Activity) bool { return a.Kind != ActivitySessionStalled })); n != 1 {
		t.Fatalf("%d stalls announced once stall_after had passed, want alice's", n)
	}
}

// The credit for downtime moves a session's times by the time since the
// server stopped, or since the snapshot if that is later, never past the
// restore; it moves nothing when neither is known, or the clock went back.
func TestDowntimeCreditBounds(t *testing.T) {
	saved := t0.Add(time.Hour)
	for _, c := range []struct {
		name                string
		saved, stopped, now time.Time
		seen, toolSince     time.Time
		wantSeen            time.Time
		wantTool            time.Time
	}{
		{"downtime", saved, time.Time{}, saved.Add(time.Hour), t0, t0.Add(10 * time.Minute), t0.Add(time.Hour), t0.Add(70 * time.Minute)},
		{"ran on after the save", saved, saved.Add(50 * time.Minute), saved.Add(time.Hour), t0, t0, t0.Add(10 * time.Minute),
			t0.Add(10 * time.Minute)},
		{"stopped before the save", saved, saved.Add(-time.Minute), saved.Add(time.Hour), t0, t0, t0.Add(time.Hour), t0.Add(time.Hour)},
		{"not past now", saved, time.Time{}, saved.Add(time.Minute), saved.Add(5 * time.Minute), time.Time{}, saved.Add(time.Minute),
			time.Time{}},
		{"no saved time", time.Time{}, time.Time{}, saved, t0, t0, t0, t0},
		{"only the stop", time.Time{}, saved, saved.Add(time.Hour), t0, t0, t0.Add(time.Hour), t0.Add(time.Hour)},
		{"clock went back", saved, time.Time{}, saved.Add(-time.Hour), t0, t0, t0, t0},
		{"clock went back past the stop", saved, saved.Add(time.Hour), saved.Add(time.Minute), t0, t0, t0, t0},
	} {
		s := snapshot{Saved: c.saved, Sessions: []*session{{Key: "k", LastSeen: c.seen, ToolSince: c.toolSince}, nil}}
		s.creditDowntime(c.now, c.stopped)
		if x := s.Sessions[0]; !x.LastSeen.Equal(c.wantSeen) || !x.ToolSince.Equal(c.wantTool) {
			t.Errorf("%s: last seen %s, in tools since %s; want %s and %s", c.name, x.LastSeen, x.ToolSince, c.wantSeen, c.wantTool)
		}
	}
}
