package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// heartbeat is a hook event with a footprint of about size bytes.
func heartbeat(member string, size int) board.HookEvent {
	ev := hookEv(board.KindHeartbeat, member, member+"-s")
	ev.Footprint = &board.Footprint{}
	for i := 0; i*100 < size; i++ {
		ev.Footprint.Files = append(ev.Footprint.Files, board.PathRef{Path: fmt.Sprintf("src/%080d.go", i)})
	}
	return ev
}

// post sends ev as member and returns the status and the Retry-After header.
func (ts *testServer) post(t *testing.T, path, member string, ev any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(ev)
	req, _ := http.NewRequest(http.MethodPost, ts.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+ts.tokens[member])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// A member's large bodies draw on a budget of bytes, charged before they are
// read; small ones, which check edits, never do, and other members keep
// their own budgets.
func TestLargeBodiesAreMetered(t *testing.T) {
	ts := newTestServer(t)
	ts.admit.byteCap = 100 << 10
	big := heartbeat("alice", 40<<10)
	for i := range 2 {
		if code, _ := ts.post(t, "/v1/hook", "alice", big); code != http.StatusOK {
			t.Fatalf("large body %d within the budget: %d", i, code)
		}
	}
	if code, retry := ts.post(t, "/v1/hook", "alice", big); code != http.StatusTooManyRequests || retry != "1" {
		t.Fatalf("large body over the budget: %d, Retry-After %q", code, retry)
	}
	if code, _ := ts.post(t, "/v1/hook", "alice", hookEv(board.KindPreEdit, "alice", "alice-s", "a/b.go")); code != http.StatusOK {
		t.Fatalf("a small hook over the budget: %d", code)
	}
	if code, _ := ts.post(t, "/v1/hook", "bob", heartbeat("bob", 40<<10)); code != http.StatusOK {
		t.Fatalf("another member's large body: %d", code)
	}
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Second)
	ts.mu.Unlock()
	if code, _ := ts.post(t, "/v1/hook", "alice", big); code != http.StatusOK {
		t.Fatalf("large body after a second: %d", code)
	}
}

// reservedEdit has alice reserve svc/pay/** and returns a pre_edit by bob
// that names n files, one of them inside the reservation: a codemod's patch,
// about 90 bytes a file.
func reservedEdit(t *testing.T, ts *testServer, n int) board.HookEvent {
	t.Helper()
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	dec := board.DeclareRequest{Where: where("alice"), Summary: "retry", Patterns: []string{"svc/pay/**"}, Mode: board.ModeExclusive}
	if code := ts.do(t, "POST", "/v1/intents", "alice", dec, nil); code != http.StatusOK {
		t.Fatalf("declare: %d", code)
	}
	ev := hookEv(board.KindPreEdit, "bob", "b1")
	for i := range n {
		ev.Paths = append(ev.Paths, board.PathRef{Path: fmt.Sprintf("services/payments/internal/handlers/v2/file_%04d.go", i)})
	}
	ev.Paths[n/2].Path = "svc/pay/retry.go"
	return ev
}

// A pre_edit over largeBody is never answered 429 or 503. It draws on a
// budget the member's footprints do not spend, and when that budget, or the
// wait for a slot, runs out, it is answered as one the server got to too
// late: allow, marked unchecked, so that a hook under INTAGENT_FAIL=closed
// refuses the edit and any other notes it, with a note for older hooks.
func TestLargeEditsAreNeverRefusedForLoad(t *testing.T) {
	ts := newTestServer(t)
	edit := reservedEdit(t, ts, 300)
	body, _ := json.Marshal(edit)
	if len(body) <= largeBody {
		t.Fatalf("the edit is %d bytes, not a large body", len(body))
	}
	ts.admit.byteCap = 100 << 10
	// bob's footprints spend his budget, as a fleet's session starts would.
	for range 2 {
		ts.post(t, "/v1/hook", "bob", heartbeat("bob", 40<<10))
	}
	if code, _ := ts.post(t, "/v1/hook", "bob", heartbeat("bob", 40<<10)); code != http.StatusTooManyRequests {
		t.Fatalf("a footprint over the budget: %d", code)
	}
	send := func() (int, board.HookResult) {
		var res board.HookResult
		code := ts.do(t, "POST", "/v1/hook", "bob", edit, &res)
		return code, res
	}
	checked := func(what string) {
		t.Helper()
		if code, res := send(); code != http.StatusOK || len(res.Conflicts) == 0 || res.Unchecked || strings.Contains(res.Context, "without a check") {
			t.Fatalf("%s: %d %+v", what, code, res)
		}
	}
	unchecked := func(what string) {
		t.Helper()
		if code, res := send(); code != http.StatusOK || res.Decision != board.DecisionAllow || !res.Unchecked ||
			res.Context != uncheckedEditNote || len(res.Conflicts) != 0 {
			t.Fatalf("%s: %d %+v", what, code, res)
		}
	}
	fits := int(ts.admit.byteCap) / len(body)
	for i := range fits {
		checked(fmt.Sprintf("edit %d of %d within the budget for edits", i+1, fits))
	}
	unchecked("an edit over the budget for edits")
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Second)
	ts.mu.Unlock()
	checked("an edit a second later")

	ts.admit.largeWait = 50 * time.Millisecond
	for range cap(ts.admit.large) {
		ts.admit.large <- struct{}{} // every slot busy with another large body
	}
	unchecked("an edit with no slot free")
	for range cap(ts.admit.large) {
		<-ts.admit.large
	}
	checked("an edit with the slots free")
}

// An edit admission lets through unchecked went ahead unchecked as surely as
// one answered too late, and counts the same towards the server's being
// degraded.
func TestEditsLetThroughForLoadAreCounted(t *testing.T) {
	ts := newTestServer(t)
	edit := reservedEdit(t, ts, 300)
	unchecked := func(what string, want int64) {
		t.Helper()
		var res board.HookResult
		if code := ts.do(t, "POST", "/v1/hook", "bob", edit, &res); code != http.StatusOK || !res.Unchecked || res.Context != uncheckedEditNote {
			t.Fatalf("%s: %d %+v", what, code, res)
		}
		if load := ts.answers.status(ts.requestClock()); load.PreEdits60s != want || load.Unchecked60s != want {
			t.Fatalf("%s: %+v, want %d pre_edits, all unchecked", what, load, want)
		}
	}
	ts.admit.mu.Lock()
	ts.admit.byteCap = 1
	ts.admit.mu.Unlock()
	unchecked("an edit over the budget for edits", 1)
	ts.admit.mu.Lock()
	ts.admit.byteCap = memberByteBurst
	delete(ts.admit.edits, "bob")
	ts.admit.mu.Unlock()
	ts.admit.largeWait = 50 * time.Millisecond
	for range cap(ts.admit.large) {
		ts.admit.large <- struct{}{} // every slot busy with another large body
	}
	unchecked("an edit with no slot free", 2)
}

// A large pre_edit's client waits from when it sent the edit, also while the
// body waits for a slot: an edit whose client has stopped waiting by the time
// a slot frees is not decided, and one whose client leaves while it waits
// went ahead unchecked.
func TestLargeEditWaitsFromItsArrival(t *testing.T) {
	ts := newTestServer(t)
	ts.wall.Store(true)
	body, _ := json.Marshal(reservedEdit(t, ts, 300))
	send := func(client *http.Client) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, ts.url+"/v1/hook", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		req.Header.Set(timeoutHeader, "400") // late after 300 ms
		return client.Do(req)
	}
	ts.admit.largeWait = 5 * time.Second
	for range cap(ts.admit.large) {
		ts.admit.large <- struct{}{} // every slot busy with another large body
	}
	freed := make(chan struct{})
	go func() {
		defer close(freed)
		time.Sleep(600 * time.Millisecond)
		<-ts.admit.large
	}()
	resp, err := send(http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	var res board.HookResult
	err = json.NewDecoder(resp.Body).Decode(&res)
	_ = resp.Body.Close()
	<-freed
	if err != nil || resp.StatusCode != http.StatusOK || !res.Unchecked || res.Decision != board.DecisionAllow {
		t.Fatalf("an edit that waited 600 ms for a slot, its client 300: %d %+v %v; want it answered late", resp.StatusCode, res, err)
	}
	if st := ts.Board().View(ts.now(), repo).Stats; st.Unheard != 1 {
		t.Fatalf("stats: %+v, want the edit unheard", st)
	}

	ts.admit.large <- struct{}{} // every slot busy again
	if _, err := send(&http.Client{Timeout: 600 * time.Millisecond}); err == nil {
		t.Fatal("the client got an answer with every slot busy")
	}
	deadline := time.Now().Add(5 * time.Second)
	for ts.answers.status(time.Now()).Unchecked60s < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if load := ts.answers.status(time.Now()); load.PreEdits60s != 2 || load.Unchecked60s != 2 {
		t.Fatalf("load: %+v, want both edits counted unchecked", load)
	}
}

// A body that starts as a pre_edit, and so draws on the budget for edits,
// must be one.
func TestLargeEditMustBeAPreEdit(t *testing.T) {
	ts := newTestServer(t)
	body, _ := json.Marshal(reservedEdit(t, ts, 300))
	for _, kind := range []string{`"kind"`, `"KIND"`} {
		b := slices.Concat(body[:len(body)-1], []byte(","+kind+`:"heartbeat"}`))
		req, _ := http.NewRequest(http.MethodPost, ts.url+"/v1/hook", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(e.Error, "starts as a pre_edit") {
			t.Fatalf("a pre_edit whose %s is then heartbeat: %d %q", kind, resp.StatusCode, e.Error)
		}
	}
}

// A stop's or a session end's footprint makes a large body, which a member
// over budget is refused, but the same event without it is small, never
// metered, and lands; intagent's hooks send it again so (cli's sendHook).
// Lost, a stop would leave its session working, to be announced as stalled,
// and an end would leave the session live, its reservation refusing
// teammates' edits for hours.
func TestLifecycleEventsWithoutTheirFootprintAreNotMetered(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "bob", hookEv(board.KindPrompt, "bob", "b1"), nil)
	dec := board.DeclareRequest{Where: where("bob"), Summary: "retry", Patterns: []string{"svc/pay/**"}, Mode: board.ModeExclusive}
	if code := ts.do(t, "POST", "/v1/intents", "bob", dec, nil); code != http.StatusOK {
		t.Fatalf("declare: %d", code)
	}
	ts.admit.mu.Lock()
	ts.admit.byteCap = 1 // bob's budget for large bodies is spent
	ts.admit.mu.Unlock()
	send := func(kind board.Kind) {
		t.Helper()
		ev := heartbeat("bob", 30<<10)
		ev.Kind, ev.SessionID = kind, "b1"
		if code, _ := ts.post(t, "/v1/hook", "bob", ev); code != http.StatusTooManyRequests {
			t.Fatalf("%s with a footprint over the budget: %d", kind, code)
		}
		ev.Footprint = nil
		if code, _ := ts.post(t, "/v1/hook", "bob", ev); code != http.StatusOK {
			t.Fatalf("%s without its footprint: %d", kind, code)
		}
	}

	send(board.KindStop)
	ts.mu.Lock()
	ts.clock = ts.clock.Add(11 * time.Minute) // past StallAfter
	ts.mu.Unlock()
	ts.Board().Sweep(ts.now())
	for _, a := range ts.Board().View(ts.now(), repo).Recent {
		if a.Kind == board.ActivitySessionStalled {
			t.Fatalf("a session that stopped is announced as stuck: %+v", a)
		}
	}

	send(board.KindSessionEnd)
	var res board.HookResult
	for range 2 { // bumped once, as by any reservation whose agent has ended
		res = board.HookResult{}
		ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPreEdit, "alice", "a1", "svc/pay/retry.go"), &res)
	}
	if res.Decision != board.DecisionAllow {
		t.Fatalf("alice's second try after bob's agent ended: %s %+v", res.Decision, res.Conflicts)
	}
}

// Large bodies wait for one of a few slots to be decoded and handled, and
// give up with 503 after a while; small ones never wait.
func TestLargeBodiesWaitForASlot(t *testing.T) {
	ts := newTestServer(t)
	ts.admit.largeWait = 50 * time.Millisecond
	for range cap(ts.admit.large) {
		ts.admit.large <- struct{}{} // every slot busy with another large body
	}
	if code, retry := ts.post(t, "/v1/hook", "alice", heartbeat("alice", 40<<10)); code != http.StatusServiceUnavailable || retry != "1" {
		t.Fatalf("large body with no slot free: %d, Retry-After %q", code, retry)
	}
	if code, _ := ts.post(t, "/v1/hook", "alice", hookEv(board.KindPreEdit, "alice", "alice-s", "a/b.go")); code != http.StatusOK {
		t.Fatalf("small hook with no slot free: %d", code)
	}
	<-ts.admit.large
	if code, _ := ts.post(t, "/v1/hook", "alice", heartbeat("alice", 40<<10)); code != http.StatusOK {
		t.Fatalf("large body with a slot free: %d", code)
	}
	if n := len(ts.admit.large); n != cap(ts.admit.large)-1 {
		t.Fatalf("%d slots taken after the request, want %d", n, cap(ts.admit.large)-1)
	}
}

// A large body sent slowly holds its connection until the read timeout, but
// no slot: other large bodies go on being handled.
func TestSlowLargeBodyHoldsNoSlot(t *testing.T) {
	ts := newTestServer(t)
	ts.admit.largeWait = 50 * time.Millisecond
	addr := ts.serve(t)
	for range cap(ts.admit.large) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		fmt.Fprintf(c, "POST /v1/hook HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
			ts.tokens["alice"], 40<<10, strings.Repeat(" ", 1000))
	}
	// Wait until the server has charged them all, and so is reading them.
	charged := func() bool {
		ts.admit.mu.Lock()
		defer ts.admit.mu.Unlock()
		b := ts.admit.bytes["alice"]
		return b != nil && b.tokens <= ts.admit.byteCap-float64(cap(ts.admit.large)*(40<<10))
	}
	for deadline := time.Now().Add(5 * time.Second); !charged(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the slow bodies never reached the server")
		}
	}
	if code, _ := ts.post(t, "/v1/hook", "bob", heartbeat("bob", 40<<10)); code != http.StatusOK {
		t.Fatalf("large body while others are sent slowly: %d", code)
	}
}

func checkReq(member, worktree string, paths int) board.CheckRequest {
	w := where(member)
	w.Worktree = worktree
	req := board.CheckRequest{Where: w}
	for i := range paths {
		req.Paths = append(req.Paths, board.PathRef{Path: fmt.Sprintf("docs/%d.md", i)})
	}
	return req
}

// Checks and declarations from one worktree are paced: a burst, then a few a
// second. Other worktrees, other members and hooks are not held back.
func TestChecksArePacedPerWorktree(t *testing.T) {
	ts := newTestServer(t)
	for i := range callBurst {
		if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusOK {
			t.Fatalf("check %d: %d", i, code)
		}
	}
	if code, retry := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusTooManyRequests || retry != "1" {
		t.Fatalf("check past the burst: %d, Retry-After %q", code, retry)
	}
	declare := board.DeclareRequest{Where: checkReq("alice", "/w/a", 0).Where, Patterns: []string{"a/**"}}
	if code, _ := ts.post(t, "/v1/intents", "alice", declare); code != http.StatusTooManyRequests {
		t.Fatalf("declare past the burst: %d", code)
	}
	for _, c := range []struct{ member, worktree string }{{"alice", "/w/a2"}, {"bob", "/w/a"}} {
		if code, _ := ts.post(t, "/v1/check", c.member, checkReq(c.member, c.worktree, 1)); code != http.StatusOK {
			t.Fatalf("%s's check from %s: %d", c.member, c.worktree, code)
		}
	}
	pre := hookEv(board.KindPreEdit, "alice", "a1", "a/b.go")
	pre.Where.Worktree = "/w/a"
	if code, _ := ts.post(t, "/v1/hook", "alice", pre); code != http.StatusOK {
		t.Fatalf("a hook from the paced worktree: %d", code)
	}
	ts.mu.Lock()
	ts.clock = ts.clock.Add(time.Second)
	ts.mu.Unlock()
	for i := range callRate {
		if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusOK {
			t.Fatalf("check %d a second later: %d", i, code)
		}
	}
	if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 1)); code != http.StatusTooManyRequests {
		t.Fatalf("check past the rate: %d", code)
	}
}

// One check or declaration at a time from a worktree.
func TestOneCheckAtATimePerWorktree(t *testing.T) {
	ts := newTestServer(t)
	req := checkReq("alice", "/w/a", 1)
	release, why := ts.admit.call("alice\x00"+req.Where.Host+"\x00/w/a", ts.now())
	if why != "" {
		t.Fatal(why)
	}
	var e ErrorResponse
	if code := ts.do(t, "POST", "/v1/check", "alice", req, &e); code != http.StatusTooManyRequests || !strings.Contains(e.Error, "still running") {
		t.Fatalf("check while another runs: %d %q", code, e.Error)
	}
	release()
	if code, _ := ts.post(t, "/v1/check", "alice", req); code != http.StatusOK {
		t.Fatalf("check after the other ended: %d", code)
	}
}

// Paced worktrees that have refilled are dropped once there are many: the
// buckets kept are at most about twice those in use.
func TestPacingForgetsIdleWorktrees(t *testing.T) {
	a := newAdmission()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	call := func(key string, at time.Time) {
		release, why := a.call(key, at)
		if why != "" {
			t.Fatal(why)
		}
		release()
	}
	for i := range 3 * minPrune {
		call(fmt.Sprint("early", i), now)
	}
	most := 0
	for i := range 3 * minPrune {
		call(fmt.Sprint("late", i), now.Add(time.Minute))
		most = max(most, len(a.calls))
	}
	if n := len(a.calls); n != 3*minPrune {
		t.Fatalf("%d buckets after the early ones refilled, want %d", n, 3*minPrune)
	}
	if most > 2*3*minPrune {
		t.Fatalf("up to %d buckets for %d in use", most, 3*minPrune)
	}
}

// A check names at most 200 paths, and is refused rather than cut short.
func TestOversizedCheckIsRefused(t *testing.T) {
	ts := newTestServer(t)
	if code, _ := ts.post(t, "/v1/check", "alice", checkReq("alice", "/w/a", 200)); code != http.StatusOK {
		t.Fatalf("200 paths: %d", code)
	}
	var e ErrorResponse
	if code := ts.do(t, "POST", "/v1/check", "alice", checkReq("alice", "/w/a", 201), &e); code != http.StatusBadRequest || !strings.Contains(e.Error, "at most 200") {
		t.Fatalf("201 paths: %d %q", code, e.Error)
	}
}
