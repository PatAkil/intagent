package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

const repo = "github.com/acme/mono"

type testServer struct {
	*Server
	url    string
	tokens map[string]string
	mu     sync.Mutex
	clock  time.Time
	// The server's request clock reads the same, step after its first
	// reading since wait, however often it is read: a request that arrives
	// then has waited step whenever it asks. With wall set, it is the real
	// clock.
	step, reads atomic.Int64
	wall        atomic.Bool
	// later moves the request clock's base on (passes).
	later atomic.Int64
}

func newTestServer(t *testing.T, mutate ...func(*Options)) *testServer {
	t.Helper()
	ts := &testServer{tokens: map[string]string{}, clock: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	var members []Member
	for _, name := range []string{"alice", "bob"} {
		tok, hash, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		ts.tokens[name] = tok
		members = append(members, Member{Name: name, TokenSHA256: hash})
	}
	o := Options{Members: members, Board: board.DefaultConfig(), Now: ts.now, Dashboard: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "dashboard") })}
	for _, m := range mutate {
		m(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ts.Server = s
	s.clock = ts.requestClock
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	ts.url = hs.URL
	return ts
}

func (ts *testServer) now() time.Time {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.clock
}

func (ts *testServer) requestClock() time.Time {
	if ts.wall.Load() {
		return time.Now()
	}
	after := min(ts.reads.Add(1)-1, 1) // 0 for the first reading, 1 for every later one
	return time.Unix(1_790_000_000, 0).Add(time.Duration(ts.later.Load() + after*ts.step.Load()))
}

// passes moves the request clock on by d, as time passing does for pacing.
func (ts *testServer) passes(d time.Duration) { ts.later.Add(int64(d)) }

// wait makes the next request to read the request clock wait d: it arrives
// at the clock's base, and every later reading is d on.
func (ts *testServer) wait(d time.Duration) {
	ts.step.Store(int64(d))
	ts.reads.Store(0)
}

func (ts *testServer) do(t *testing.T, method, path, member string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.url+path, rd)
	if member != "" {
		req.Header.Set("Authorization", "Bearer "+ts.tokens[member])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func where(member string) board.Where {
	return board.Where{Repo: repo, Host: member + "-host", Worktree: "/w/" + member, Branch: "b-" + member}
}

func hookEv(kind board.Kind, member, session string, paths ...string) board.HookEvent {
	ev := board.HookEvent{Kind: kind, Agent: board.AgentClaudeCode, SessionID: session, Where: where(member), Tool: "Edit"}
	for _, p := range paths {
		ev.Paths = append(ev.Paths, board.PathRef{Path: p})
	}
	return ev
}

func TestAuth(t *testing.T) {
	ts := newTestServer(t)
	if code := ts.do(t, "POST", "/v1/hook", "", hookEv(board.KindPrompt, "alice", "a"), nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/v1/hook", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer ia_wrong")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	// A dashboard session authorises reads...
	session := ts.login(t, "bob", nil)
	do := func(method, path, cookie, token string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.url+path, strings.NewReader(`{}`))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	req, _ = http.NewRequest(http.MethodGet, ts.url+"/v1/whoami", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("session read: %v %d", err, resp.StatusCode)
	}
	var who map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&who)
	if who["member"] != "bob" {
		t.Fatalf("whoami = %v", who)
	}
	// ...never a write, it is not a token, and a token is not a session.
	if code := do(http.MethodPost, "/v1/hook", session, ""); code != http.StatusUnauthorized {
		t.Fatalf("session write: %d", code)
	}
	if code := do(http.MethodGet, "/v1/whoami", "", session); code != http.StatusUnauthorized {
		t.Fatalf("session as a token: %d", code)
	}
	if session == ts.tokens["bob"] || strings.Contains(session, ts.tokens["bob"]) {
		t.Fatal("the session cookie holds the token")
	}
	if code := do(http.MethodGet, "/v1/whoami", ts.tokens["bob"], ""); code != http.StatusUnauthorized {
		t.Fatalf("token as a session: %d", code)
	}
	// The cookie is hex, so a first digit other than its own forges one.
	forged := "1" + session[1:]
	if session[0] == '1' {
		forged = "0" + session[1:]
	}
	if code := do(http.MethodGet, "/v1/whoami", forged, ""); code != http.StatusUnauthorized {
		t.Fatalf("forged session: %d", code)
	}
	if code := ts.do(t, "GET", "/healthz", "", nil, nil); code != http.StatusOK {
		t.Fatalf("healthz: %d", code)
	}
}

func TestPublicRead(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.PublicRead = true })
	if code := ts.do(t, "GET", "/v1/board?repo="+repo, "", nil, nil); code != http.StatusOK {
		t.Fatalf("public read: %d", code)
	}
	if code := ts.do(t, "POST", "/v1/hook", "", hookEv(board.KindPrompt, "alice", "a"), nil); code != http.StatusUnauthorized {
		t.Fatalf("public write: %d", code)
	}
}

// login signs a member in to the dashboard and returns the session cookie.
func (ts *testServer) login(t *testing.T, member string, header http.Header) string {
	t.Helper()
	form := strings.NewReader("token=" + ts.tokens[member])
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/ui/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil || len(resp.Cookies()) != 1 {
		t.Fatalf("login %s: %v %v", member, err, resp)
	}
	return resp.Cookies()[0].Value
}

func TestSessionsSurviveARestartAndEndWithTheToken(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, func(o *Options) { o.DataDir = dir })
	session := ts.login(t, "alice", nil)
	whoami := func(s *Server) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if info, err := os.Stat(filepath.Join(dir, "ui.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ui.key: %v %v", info, err)
	}
	members := []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}
	again, err := New(Options{Members: members, Board: board.DefaultConfig(), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if code := whoami(again); code != http.StatusOK {
		t.Fatalf("after a restart: %d", code)
	}
	_, rotated, _ := NewToken()
	again, err = New(Options{Members: []Member{{Name: "alice", TokenSHA256: rotated}}, Board: board.DefaultConfig(), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if code := whoami(again); code != http.StatusUnauthorized {
		t.Fatalf("after rotating the token: %d", code)
	}
}

func TestLoginSetsCookie(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.url+"/ui/login", map[string][]string{"token": {ts.tokens["alice"]}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 || !resp.Cookies()[0].HttpOnly || resp.Cookies()[0].Secure {
		t.Fatalf("login: %d %v", resp.StatusCode, resp.Cookies())
	}
	// Behind a proxy that terminates TLS the cookie is Secure.
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/ui/login", strings.NewReader("token="+ts.tokens["alice"]))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	if resp, err := client.Do(req); err != nil || len(resp.Cookies()) != 1 || !resp.Cookies()[0].Secure {
		t.Fatalf("login behind TLS: %v %v", err, resp.Cookies())
	}
	resp, _ = client.PostForm(ts.url+"/ui/login", map[string][]string{"token": {"nope"}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "failed") || len(resp.Cookies()) != 0 {
		t.Fatalf("bad login: %s %v", loc, resp.Cookies())
	}
	resp, _ = client.Post(ts.url+"/ui/logout", "", nil)
	if c := resp.Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("logout cookie = %v", c)
	}
}

func TestCollisionOverHTTP(t *testing.T) {
	ts := newTestServer(t)
	var res board.HookResult
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), &res)
	var dec board.DeclareResult
	code := ts.do(t, "POST", "/v1/intents", "alice", board.DeclareRequest{Where: where("alice"), Summary: "retry", Patterns: []string{"svc/pay/**"}, Mode: board.ModeExclusive}, &dec)
	if code != http.StatusOK || len(dec.Accepted) != 1 {
		t.Fatalf("declare: %d %+v", code, dec)
	}
	ts.do(t, "POST", "/v1/hook", "bob", hookEv(board.KindPreEdit, "bob", "b1", "svc/pay/retry.go"), &res)
	if res.Decision != board.DecisionRefuse || !strings.Contains(res.Reason, "alice") {
		t.Fatalf("bob's edit: %+v", res)
	}
	// The member comes from the token: bob cannot pose as alice.
	ev := hookEv(board.KindPreEdit, "alice", "a1", "svc/pay/retry.go")
	ts.do(t, "POST", "/v1/hook", "bob", ev, &res)
	if res.Decision != board.DecisionRefuse {
		t.Fatalf("bob posing as alice was allowed: %+v", res)
	}

	var chk board.CheckResult
	ts.do(t, "POST", "/v1/check", "bob", board.CheckRequest{Where: where("bob"), Paths: []board.PathRef{{Path: "svc/pay/x.go"}}}, &chk)
	if len(chk.Conflicts) != 1 || !strings.Contains(chk.Text, "block") {
		t.Fatalf("check: %+v", chk)
	}
	var rel map[string]int
	ts.do(t, "POST", "/v1/intents/release", "alice", board.ReleaseRequest{Where: where("alice")}, &rel)
	if rel["released"] != 1 {
		t.Fatalf("release: %v", rel)
	}
	var note board.NoteResult
	if code := ts.do(t, "POST", "/v1/notes", "bob", board.NoteRequest{Where: where("bob"), To: "alice", Text: "hi"}, &note); code != http.StatusOK || len(note.Delivered) != 1 {
		t.Fatalf("note: %d %+v", code, note)
	}
}

func TestErrorMapping(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.Board.NotesPerMinute = 1 })
	var e ErrorResponse
	if code := ts.do(t, "POST", "/v1/hook", "alice", board.HookEvent{Kind: board.KindPrompt}, &e); code != http.StatusBadRequest || e.Error == "" {
		t.Fatalf("invalid event: %d %+v", code, e)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/v1/hook", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+ts.tokens["alice"])
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d", resp.StatusCode)
	}
	if code := ts.do(t, "POST", "/v1/notes", "bob", board.NoteRequest{Where: where("bob"), To: "nobody", Text: "x"}, nil); code != http.StatusNotFound {
		t.Fatalf("no target: %d", code)
	}
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPrompt, "alice", "a1"), nil)
	ts.do(t, "POST", "/v1/notes", "bob", board.NoteRequest{Where: where("bob"), To: "alice", Text: "1"}, nil)
	if code := ts.do(t, "POST", "/v1/notes", "bob", board.NoteRequest{Where: where("bob"), To: "alice", Text: "2"}, nil); code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", code)
	}
	if code := ts.do(t, "GET", "/v1/board", "alice", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("board without repo: %d", code)
	}
	big := strings.Repeat("x", maxBody+10)
	req, _ = http.NewRequest(http.MethodPost, ts.url+"/v1/hook", strings.NewReader(`{"prompt":"`+big+`"}`))
	req.Header.Set("Authorization", "Bearer "+ts.tokens["alice"])
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}
}

func TestBoardViews(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	var v board.View
	ts.do(t, "GET", "/v1/board?repo="+repo, "bob", nil, &v)
	if len(v.Claims) != 1 || v.Claims[0].Member != "alice" {
		t.Fatalf("view: %+v", v)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/board?format=text&repo="+repo, nil)
	req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "alice on b-alice") {
		t.Fatalf("text board: %s", body)
	}
	var repos []board.RepoSummary
	ts.do(t, "GET", "/v1/repos", "bob", nil, &repos)
	if len(repos) != 1 || repos[0].Repo != repo {
		t.Fatalf("repos: %+v", repos)
	}
	resp, _ = http.Get(ts.url + "/")
	if b, _ := io.ReadAll(resp.Body); string(b) != "dashboard" {
		t.Fatalf("dashboard not mounted: %q", b)
	}
}

func readEvents(t *testing.T, r *bufio.Reader, n int) []board.Activity {
	t.Helper()
	var out []board.Activity
	for len(out) < n {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended after %d events: %v", len(out), err)
		}
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
			var a board.Activity
			if err := json.Unmarshal([]byte(data), &a); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
	}
	return out
}

func TestStream(t *testing.T) {
	ts := newTestServer(t)
	open := func(lastID string) (*bufio.Reader, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.url+"/v1/stream?repo="+repo, nil)
		req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream: %v %v", err, resp)
		}
		r := bufio.NewReader(resp.Body)
		for { // wait until the server has subscribed us
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, ": connected") {
				break
			}
		}
		return r, func() { cancel(); _ = resp.Body.Close() }
	}
	r, closeStream := open("")
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	evs := readEvents(t, r, 3) // claim.opened, session.started, file.changed
	if evs[2].Kind != "file.changed" || evs[2].Member != "alice" {
		t.Fatalf("events: %+v", evs)
	}
	closeStream()

	// Reconnecting with Last-Event-ID replays what was missed.
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/z.go"), nil)
	r, closeStream = open("2")
	defer closeStream()
	replay := readEvents(t, r, 2)
	if replay[0].Seq != 3 || replay[1].Seq != 4 {
		t.Fatalf("replay seqs = %d, %d", replay[0].Seq, replay[1].Seq)
	}
}

func TestPersistenceAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, func(o *Options) { o.DataDir = dir })
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)
	if err := ts.save(nil); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "board.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot: %v %v", err, fi)
	}
	again, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}}, Board: board.DefaultConfig(), DataDir: dir, Now: ts.now})
	if err != nil {
		t.Fatal(err)
	}
	if v := again.Board().View(ts.now(), repo); len(v.Claims) != 1 || len(v.Claims[0].Files) != 1 {
		t.Fatalf("restored view = %+v", v)
	}
}

func TestServeShutsDownAndSaves(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, func(o *Options) { o.DataDir = dir; o.SweepEvery = 10 * time.Millisecond })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Serve(ctx, ln) }()

	base := "http://" + ln.Addr().String()
	// An open event stream must not hold up shutdown.
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/stream?repo="+repo, nil)
	req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	ts.url = base
	ts.do(t, "POST", "/v1/hook", "alice", hookEv(board.KindPostEdit, "alice", "a1", "x/y.go"), nil)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, err := os.Stat(filepath.Join(dir, "board.json")); err != nil {
		t.Fatalf("no final snapshot: %v", err)
	}
}

func TestConfigFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team.json")
	tok, err := AddMember(path, "alice", false)
	if err != nil || !strings.HasPrefix(tok, "ia_") {
		t.Fatalf("AddMember: %q %v", tok, err)
	}
	if _, err := AddMember(path, "alice", false); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("duplicate member: %v", err)
	}
	tok2, err := AddMember(path, "alice", true)
	if err != nil || tok2 == tok {
		t.Fatalf("rotate: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "team.json")
	_ = os.WriteFile(broken, []byte(`{"members":[],"webhook":{"url":"https://hooks.example.com/x","events":["session.stuck"]}}`), 0o600)
	if _, err := AddMember(broken, "bob", false); err == nil || !strings.Contains(err.Error(), "session.stuck") {
		t.Fatalf("token add to a file the server would refuse: %v", err)
	}
	if _, err := AddMember(path, "Bad Name", false); err == nil {
		t.Fatal("accepted an invalid member name")
	}
	fc, err := LoadFileConfig(path)
	if err != nil || len(fc.Members) != 1 || fc.Members[0].TokenSHA256 != HashToken(tok2) {
		t.Fatalf("LoadFileConfig: %+v %v", fc, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions %v", fi.Mode().Perm())
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	for _, body := range []string{
		`{"members":[{"name":"a","token_sha256":"zz"}]}`,
		`{"members":[{"name":"a","token_sha256":"` + HashToken("x") + `"},{"name":"a","token_sha256":"` + HashToken("y") + `"}]}`,
		`{"policy":{"block":"explode"}}`,
		`{"webhook":{"url":"https://hooks.example.com/x","events":["session.stuck"]}}`,
		`{"stall_after":"soon"}`,
	} {
		_ = os.WriteFile(bad, []byte(body), 0o600)
		if _, err := LoadFileConfig(bad); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	_ = os.WriteFile(bad, []byte(`{"stall_after":"3m","policy":{"overlap":"warn"},"notes_per_minute":5,"max_sessions":900,`+
		`"max_dormant_claims":800,"webhook":{"url":"https://hooks.example.com/x","idle":true}}`), 0o600)
	fc, err = LoadFileConfig(bad)
	if err != nil || !fc.Webhook.Idle {
		t.Fatal(fc.Webhook, err)
	}
	bc := fc.BoardConfig()
	if bc.StallAfter != 3*time.Minute || bc.Policy.Overlap != board.ActionWarn || bc.Policy.Block != board.ActionDeny || bc.NotesPerMinute != 5 ||
		bc.MaxSessions != 900 || bc.MaxDormantClaims != 800 {
		t.Fatalf("BoardConfig = %+v", bc)
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted no members")
	}
}

func TestTeamFileRejectsUnknownSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "team.json")
	if err := os.WriteFile(p, []byte(`{"members":[],"stall-after":"5m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFileConfig(p); err == nil || !strings.Contains(err.Error(), "stall-after") {
		t.Fatalf("err = %v", err)
	}
}

// Rotating a token ends the streams opened with it; their clients must
// authenticate again.
func TestMemberChangesEndOpenStreams(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+ts.tokens["bob"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v %v", resp, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, rotated, _ := NewToken()
	if err := ts.SetMembers([]Member{{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}, {Name: "bob", TokenSHA256: rotated}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream opened with the rotated-out token stayed open")
	}
}

// On a board anyone may read, a token the server does not know is still
// rejected: its owner has to find out.
func TestPublicBoardRejectsUnknownTokens(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.PublicRead = true })
	if code := ts.do(t, http.MethodGet, "/v1/whoami", "", nil, nil); code != http.StatusOK {
		t.Fatalf("anonymous: %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer ia_typo")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestTokenAddRunsDoNotLoseEachOther(t *testing.T) {
	p := filepath.Join(t.TempDir(), "team.json")
	// A crashed run leaves its lock file; no process holds it any more.
	if err := os.WriteFile(p+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := AddMember(p, fmt.Sprintf("m%d", i), false); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	fc, err := LoadFileConfig(p)
	if err != nil || len(fc.Members) != 8 {
		t.Fatalf("members = %d, %v", len(fc.Members), err)
	}
}
