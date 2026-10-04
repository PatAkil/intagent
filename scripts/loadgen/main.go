// Command loadgen drives a running intagent server the way a team does, and
// reports how it held up.
//
// Members' agents, each in its own worktree, run sessions against POST
// /v1/hook: session_start with the worktree's footprint, prompts, pre_edit
// then post_edit on files drawn from a skewed distribution over areas (so
// agents collide, are bumped and are refused), tool_start and tool_end around
// shell calls (tool_end with a footprint at most every 15 s, as the hook
// does), stop and session_end, then a new session. By default half the tool
// calls are edits and half are shell commands, so pre_edit, post_edit,
// tool_start and tool_end are a quarter each of the hooks besides the
// sessions' boundaries, as coding agents send them (-edits). Some sessions check paths,
// declare shared or exclusive intents and send notes. Agents pause between
// events for exponential times whose mean makes them send -rate events a
// second together. Each hook request opens a new connection, as a hook is a
// new process each time.
//
// Dashboards sign in with a member's token, hold /v1/stream open on one
// repository and reload /v1/board as internal/web/static/app.js does: after an
// event, at most once per 300 ms and one request at a time; every 15 s, with
// /v1/repos; and after a reconnect. The other -streams only listen. A reload
// also runs app.js's check for a restarted server: a view whose epoch differs
// from the last one's, or, from a server that sends no epoch (or with
// -epoch=false, for tabs still running the older app.js), a view whose
// last_seq is below an event the dashboard already has. Either starts the
// feed over and opens a new stream, which reloads the board once more; the
// report counts these resets. -throttle, -jitter and -fetch-factor model a
// slower reload throttle.
//
// The first run adds the members to the team file with 'intagent token add'
// and keeps their tokens in -tokens; serve that team file (a running server
// picks new members up within seconds):
//
//	go build -o /tmp/lg/intagent ./cmd/intagent
//	go run ./scripts/loadgen -setup -intagent /tmp/lg/intagent -team /tmp/lg/team.json -tokens /tmp/lg/tokens.json
//	/tmp/lg/intagent serve --config /tmp/lg/team.json --data /tmp/lg/data --addr 127.0.0.1:7400 & echo $! >/tmp/lg/pid
//	go run ./scripts/loadgen -tokens /tmp/lg/tokens.json -pidfile /tmp/lg/pid -rate 200 -duration 60s
//
// The defaults are the scale intagent is meant for: 200 members with 5 agents
// each, 30 repositories with 30% of the agents in one, 100 dashboards, 300
// streams. Every session's plan comes from -seed, so runs with the same seed
// issue the same requests; with -react an agent also retries a refused edit
// and drops the edit if refused again, which depends on the server's answers.
//
// The report gives p50, p95, p99 and max latency per endpoint and hook kind,
// errors, requests over 2 s (a hook gives up then, and a pre_edit goes
// unchecked), what pre_edits were told, events per stream and their delivery
// lag, and the server's RSS and CPU from /proc (-pid or -pidfile).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// hookLimit is how long a hook waits for the server before its agent goes on
// unchecked: INTAGENT_TIMEOUT's default.
const hookLimit = 2 * time.Second

// The dashboard's timings, from internal/web/static/app.js, and how often a
// shell command's end rescans the worktree, from internal/cli/hook.go.
const (
	refreshEvery = 15 * time.Second
	fetchTimeout = 15 * time.Second
	streamRetry  = 3 * time.Second
	scanEvery    = 15 * time.Second
)

var (
	baseURL    = flag.String("url", "http://127.0.0.1:7400", "the intagent server")
	binary     = flag.String("intagent", "intagent", "intagent binary, for 'token add'")
	teamFile   = flag.String("team", "team.json", "team file to add the members to")
	tokensFile = flag.String("tokens", "loadgen-tokens.json", "members' tokens, created on first use")
	setup      = flag.Bool("setup", false, "only create the members' tokens")
	members    = flag.Int("members", 200, "members")
	perMember  = flag.Int("agents", 5, "agents per member")
	repos      = flag.Int("repos", 30, "repositories")
	busy       = flag.Float64("busy", 0.3, "share of agents in the busiest repository")
	areas      = flag.Int("areas", 200, "areas per repository; area 0 is a library all of them use")
	files      = flag.Int("files", 500, "files per area")
	footprint  = flag.Int("footprint", 20, "mean changed files in a worktree at the start")
	big        = flag.Float64("big", 0.01, "share of worktrees with -cap changed files")
	maxFiles   = flag.Int("cap", 2000, "changed files in a big worktree (the hook's cap)")
	rate       = flag.Float64("rate", 200, "hook events a second, all agents together")
	duration   = flag.Duration("duration", time.Minute, "how long the agents run")
	ramp       = flag.Duration("ramp", 0, "start the agents spread over this (0: each after one pause)")
	warmup     = flag.Duration("warmup", 0, "leave requests started in the run's first part out of the report")
	seed       = flag.Uint64("seed", 1, "random seed")
	intentP    = flag.Float64("intents", 0.1, "share of sessions that check paths and declare intents")
	exclusiveP = flag.Float64("exclusive", 0.3, "share of declarations that are exclusive")
	noteP      = flag.Float64("notes", 0.05, "share of sessions that send a note")
	dashboards = flag.Int("dashboards", 100, "dashboards, each with a stream")
	streams    = flag.Int("streams", 300, "event streams, the dashboards' included")
	editP      = flag.Float64("edits", 0.5, "share of tool calls that are edits (pre_edit, post_edit); the rest are shell commands (tool_start, tool_end)")
	trustEpoch = flag.Bool("epoch", true, "dashboards trust the view's epoch when the server sends one; false models the older app.js")
	reloadMin  = flag.Duration("throttle", 300*time.Millisecond, "a dashboard's least time from an event to its reload")
	jitter     = flag.Duration("jitter", 0, "random time added to each reload's throttle")
	fetchMult  = flag.Float64("fetch-factor", 0, "a reload also waits this many times the last reload's duration")
	keepAlive  = flag.Bool("keepalive", false, "reuse hook connections, which a real hook cannot")
	react      = flag.Bool("react", false, "retry a refused edit, and drop it if refused again")
	timeout    = flag.Duration("timeout", hookLimit, "agents' request timeout")
	pid        = flag.Int("pid", 0, "the server's process, for RSS and CPU")
	pidFile    = flag.String("pidfile", "", "file holding the server's pid")
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("loadgen: ")
	flag.Parse()
	if err := start(); err != nil {
		log.Fatal(err)
	}
}

func start() error {
	if *members < 1 || *perMember < 1 || *repos < 1 || *areas < 3 || *files < 3 || *rate <= 0 {
		return errors.New("-members, -agents and -repos must be at least 1, -areas and -files at least 3, and -rate positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	tokens, err := loadTokens(ctx)
	if err != nil {
		return err
	}
	if *setup {
		fmt.Printf("%d members' tokens in %s\n", len(tokens), *tokensFile)
		return nil
	}
	if b, err := os.ReadFile(*pidFile); *pid == 0 && err == nil {
		*pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	// A running server reads new members from the team file within seconds.
	for i := 0; ; i++ {
		_, err := call(ctx, http.DefaultClient, http.MethodGet, "/v1/whoami", bearer(tokens[memberName(*members-1)]), nil, nil)
		if err == nil {
			break
		}
		if i == 30 {
			return fmt.Errorf("the server does not accept the members' tokens: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	run(ctx, tokens)
	return nil
}

func memberName(i int) string { return fmt.Sprintf("lg-%03d", i) }

// loadTokens reads the members' tokens, adding missing members to the team
// file with 'intagent token add'.
func loadTokens(ctx context.Context) (map[string]string, error) {
	tokens := map[string]string{}
	if b, err := os.ReadFile(*tokensFile); err == nil {
		if err := json.Unmarshal(b, &tokens); err != nil {
			return nil, fmt.Errorf("%s: %w", *tokensFile, err)
		}
	}
	n := len(tokens)
	for i := range *members {
		name := memberName(i)
		if tokens[name] != "" {
			continue
		}
		out, err := exec.CommandContext(ctx, *binary, "token", "add", name, "--config", *teamFile).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("intagent token add %s: %w: %s", name, err, out)
		}
		if i := slices.IndexFunc(strings.Fields(string(out)), func(f string) bool { return strings.HasPrefix(f, "ia_") }); i >= 0 {
			tokens[name] = strings.Fields(string(out))[i]
		} else {
			return nil, fmt.Errorf("no token in the output of 'intagent token add %s'", name)
		}
	}
	if len(tokens) == n {
		return tokens, nil
	}
	b, _ := json.MarshalIndent(tokens, "", "  ")
	return tokens, os.WriteFile(*tokensFile, b, 0o600)
}

// --- requests and measurements ------------------------------------------------

type statusError int

func (e statusError) Error() string { return "HTTP " + strconv.Itoa(int(e)) }

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// call sends a request and decodes the answer into out, if given. It returns
// the answer's size.
func call(ctx context.Context, hc *http.Client, method, path string, auth func(*http.Request), in, out any) (int64, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, *baseURL+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	auth(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil || resp.StatusCode != http.StatusOK {
		n, err := io.Copy(io.Discard, resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = statusError(resp.StatusCode)
		}
		return n, err
	}
	data, err := io.ReadAll(resp.Body)
	if raw, ok := out.(*[]byte); ok && err == nil {
		*raw = data // left to the caller, which reads a few fields of a large answer
	} else if err == nil {
		err = json.Unmarshal(data, out)
	}
	return int64(len(data)), err
}

type series struct {
	lat        []time.Duration
	errs, slow int // failed; took over hookLimit
	unchecked  int // either: a hook would have let its edit through unchecked
	bytes      int64
}

// recorder collects latencies per endpoint and counts outcomes.
type recorder struct {
	mu      sync.Mutex
	from    time.Time // requests started before this are warm-up
	skipped int
	series  map[string]*series
	counts  map[string]int
}

// add records a request that started at start. A note nobody can receive
// (404) or one over the rate limit (429) is an answer, not an error.
func (r *recorder) add(key string, start time.Time, n int64, err error) {
	d := time.Since(start)
	r.mu.Lock()
	defer r.mu.Unlock()
	if start.Before(r.from) {
		r.skipped++
		return
	}
	s := r.series[key]
	if s == nil {
		s = &series{}
		r.series[key] = s
	}
	s.lat, s.bytes = append(s.lat, d), s.bytes+n
	var se statusError
	switch {
	case errors.As(err, &se) && (se == http.StatusNotFound || se == http.StatusTooManyRequests):
		r.counts[key+" answered "+se.Error()]++
		err = nil
	case errors.Is(err, context.DeadlineExceeded):
		r.counts["error: "+key+": timed out"]++
	case err != nil:
		msg := err.Error()
		r.counts["error: "+key+": "+msg[max(0, len(msg)-50):]]++
	}
	if err != nil {
		s.errs++
	}
	if d > hookLimit {
		s.slow++
	}
	if err != nil || d > hookLimit {
		s.unchecked++
	}
}

// measure sends a request with a time limit and records it.
func (r *recorder) measure(hc *http.Client, auth func(*http.Request), limit time.Duration, key, method, path string, in, out any) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	n, err := call(ctx, hc, method, path, auth, in, out)
	r.add(key, start, n, err)
}

func (r *recorder) count(key string) {
	r.mu.Lock()
	r.counts[key]++
	r.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- agents --------------------------------------------------------------

var agentKinds = []board.Agent{board.AgentClaudeCode, board.AgentCodex, board.AgentCursor, board.AgentCopilot, board.AgentGemini}

func repoName(i int) string { return fmt.Sprintf("github.com/loadgen/repo%02d", i) }

func areaName(a int) string {
	if a == 0 {
		return "libs/common"
	}
	return fmt.Sprintf("services/s%03d", a)
}

func fileRef(a, f int) board.PathRef {
	return board.PathRef{Path: fmt.Sprintf("%s/f%03d.go", areaName(a), f), Area: areaName(a)}
}

// agent is one coding agent in its own worktree, running one session after another.
type agent struct {
	id, repo, home int // home is the area its tasks are in
	member         string
	auth           func(*http.Request)
	kind           board.Agent
	where          board.Where
	rng            *rand.Rand
	zfile          *rand.Zipf
	pause, start   time.Duration // the mean pause between events, and the first
	clock, scanned time.Duration // the plan's own time, and its last footprint
	tools          int
	changed        []board.PathRef
	has            map[string]bool
	hc             *http.Client
	rec            *recorder
}

// step is one planned request: a hook event, or a request to path.
type step struct {
	wait  time.Duration
	kind  board.Kind
	paths []board.PathRef
	tool  string
	id    string
	text  string
	fp    bool // the event carries the footprint
	path  string
	body  any
}

func newAgent(i int, tokens map[string]string, hc *http.Client, rec *recorder) *agent {
	rng := rand.New(rand.NewPCG(*seed, uint64(i)+1)) //nolint:gosec // a load model wants repeatable randomness, not secure randomness
	m := memberName(i / *perMember)
	a := &agent{
		id: i, member: m, auth: bearer(tokens[m]), kind: agentKinds[i%len(agentKinds)], rng: rng, hc: hc, rec: rec,
		pause: time.Duration(float64(*members**perMember) / *rate * float64(time.Second)), has: map[string]bool{},
	}
	a.repo = 1 + rng.IntN(max(1, *repos-1))
	if rng.Float64() < *busy || *repos == 1 {
		a.repo = 0
	}
	// Agents crowd into some areas and some files: that is where they meet.
	a.home = 1 + int(rand.NewZipf(rng, 1.2, 5, uint64(*areas-2)).Uint64()) //nolint:gosec // -areas is at least 3, and the draw below it
	a.zfile = rand.NewZipf(rng, 1.3, 2, uint64(*files-1))                  //nolint:gosec // -files is at least 3
	a.where = board.Where{Repo: repoName(a.repo), Host: m + "-host", Branch: fmt.Sprintf("%s/task-%d", m, i),
		Worktree: fmt.Sprintf("/work/%s/repo%02d/wt%d", m, a.repo, i%*perMember)}
	a.start = time.Duration(rng.ExpFloat64() * float64(a.pause))
	if *ramp > 0 {
		a.start = time.Duration(rng.Float64() * float64(*ramp))
	}
	switch r := rng.Float64(); {
	case r < *big:
		for range *maxFiles {
			a.change(fileRef(rng.IntN(*areas), rng.IntN(*files)))
		}
	case r > *big+0.3:
		for range int(rng.ExpFloat64() * float64(*footprint)) {
			a.change(fileRef(a.home, rng.IntN(*files)))
		}
	}
	return a
}

func (a *agent) change(p board.PathRef) {
	if !a.has[p.Path] {
		a.has[p.Path] = true
		a.changed = append(a.changed, p)
	}
}

// pick draws a file to edit: hot files first, mostly in the agent's own area,
// sometimes in the library.
func (a *agent) pick() board.PathRef {
	area := a.home
	if a.rng.Float64() < 0.15 {
		area = 0
	}
	return fileRef(area, int(a.zfile.Uint64())) //nolint:gosec // the draw is below -files
}

// event plans a hook event after a pause. A session's boundaries carry the
// footprint, and so does a shell command's end 15 s after the last scan.
func (a *agent) event(kind board.Kind, tool, id string, paths ...board.PathRef) step {
	d := time.Duration(a.rng.ExpFloat64() * float64(a.pause))
	a.clock += d
	fp := kind == board.KindSessionStart || kind == board.KindStop || kind == board.KindSessionEnd ||
		(kind == board.KindToolEnd && a.clock-a.scanned >= scanEvery)
	if fp {
		a.scanned = a.clock
	}
	return step{wait: d, kind: kind, tool: tool, id: id, paths: paths, fp: fp}
}

// plan lays out one session's requests.
func (a *agent) plan(n int) []step {
	r, area := a.rng, areaName(a.home)
	steps := []step{a.event(board.KindSessionStart, "", "")}
	declare, note := r.Float64() < *intentP, r.Float64() < *noteP
	turns := 1 + r.IntN(4)
	for t := range turns {
		p := a.event(board.KindPrompt, "", "")
		p.text = fmt.Sprintf("Task %d.%d: rework %s\nwith more detail", n, t, area)
		steps = append(steps, p)
		if t == 0 && declare {
			d := board.DeclareRequest{Where: a.where, Summary: "Rework " + area, Mode: board.ModeShared, Patterns: []string{area + "/**"}}
			if r.Float64() < *exclusiveP {
				d.Mode = board.ModeExclusive
			}
			if r.Float64() < 0.5 {
				d.Patterns = []string{a.pick().Path, a.pick().Path}
			}
			check := board.CheckRequest{Where: a.where, Paths: []board.PathRef{a.pick(), a.pick(), a.pick()}}
			steps = append(steps, step{path: "/v1/check", body: check}, step{path: "/v1/intents", body: d})
		}
		k := 2 + r.IntN(10)
		for i := range k {
			if t == 0 && note && i == k/2 {
				to := a.pick().Path // whoever works on it, or a member, who may not
				if r.Float64() < 0.5 {
					to = memberName(r.IntN(*members))
				}
				steps = append(steps, step{path: "/v1/notes", body: board.NoteRequest{Where: a.where, To: to, Text: "About to change " + to}})
			}
			a.tools++
			id := fmt.Sprintf("tu-%d-%d", a.id, a.tools)
			if r.Float64() < *editP {
				f := a.pick()
				steps = append(steps, a.event(board.KindPreEdit, "Edit", id, f), a.event(board.KindPostEdit, "Edit", id, f))
			} else {
				steps = append(steps, a.event(board.KindToolStart, "Bash", id), a.event(board.KindToolEnd, "Bash", id))
			}
		}
		steps = append(steps, a.event(board.KindStop, "", ""))
	}
	if declare {
		steps = append(steps, step{path: "/v1/intents/release", body: board.ReleaseRequest{Where: a.where}})
	}
	return append(steps, a.event(board.KindSessionEnd, "", ""))
}

func (a *agent) run(ctx context.Context) {
	ok := sleep(ctx, a.start)
	for n := 0; ok; n++ {
		ok = a.session(ctx, fmt.Sprintf("lg%d-%04d-%d", *seed, a.id, n), a.plan(n))
	}
}

// session plays a plan until it ends or the run does.
func (a *agent) session(ctx context.Context, sid string, plan []step) bool {
	for i := 0; i < len(plan); i++ {
		st := plan[i]
		if !sleep(ctx, st.wait) {
			return false
		}
		if st.path == "/v1/intents" {
			var res board.DeclareResult
			a.post("POST "+st.path, st.path, st.body, &res)
			a.rec.count(fmt.Sprintf("declare %s: accepted %t, rejected %t", st.body.(board.DeclareRequest).Mode,
				len(res.Accepted) > 0, len(res.Rejected) > 0))
			continue
		} else if st.path != "" {
			a.post("POST "+st.path, st.path, st.body, nil)
			continue
		}
		res := a.hook(sid, st)
		if st.kind != board.KindPreEdit {
			continue
		}
		a.rec.count("pre_edit told: " + told(res))
		if *react && res.Decision == board.DecisionRefuse {
			if !sleep(ctx, time.Second) {
				return false
			}
			if res = a.hook(sid, st); res.Decision == board.DecisionRefuse {
				a.rec.count("pre_edit refused again: edit dropped")
				i++ // the post_edit
			}
		}
	}
	return true
}

// told names a pre_edit's answer and the worst conflict behind it.
func told(res board.HookResult) string {
	worst := board.SeverityNone
	for _, c := range res.Conflicts {
		worst = max(worst, c.Severity)
	}
	switch {
	case res.Decision == "":
		return "nothing (an error)"
	case res.Decision == board.DecisionAllow && res.Context != "":
		return "allow with a warning, worst conflict " + worst.String()
	}
	return string(res.Decision) + ", worst conflict " + worst.String()
}

func (a *agent) hook(sid string, st step) board.HookResult {
	ev := board.HookEvent{Kind: st.kind, Agent: a.kind, SessionID: sid, Where: a.where, Tool: st.tool, ToolUseID: st.id, Paths: st.paths}
	if st.kind == board.KindPrompt {
		ev.Prompt = st.text
	}
	if st.kind == board.KindPostEdit {
		for _, p := range st.paths {
			a.change(p)
		}
	}
	if st.fp {
		ev.Footprint = &board.Footprint{Files: slices.Clone(a.changed)}
	}
	var res board.HookResult
	a.post("hook "+string(st.kind), "/v1/hook", ev, &res)
	return res
}

func (a *agent) post(key, path string, in, out any) {
	a.rec.measure(a.hc, a.auth, *timeout, key, http.MethodPost, path, in, out)
}

// --- dashboards ----------------------------------------------------------

// stream is one open /v1/stream; a dashboard's also reloads the board.
type stream struct {
	repo               string
	auth               func(*http.Request)
	hc, sc             *http.Client
	rec                *recorder
	dashboard          bool
	rng                *rand.Rand
	events, reconnects int
	resets             int // feeds started over because a view looked like a restarted server's
	lag                []time.Duration

	mu                     sync.Mutex
	timer, inflight, again bool
	lastFetch              time.Duration
	maxSeq                 uint64             // the newest event in the dashboard's feed
	epoch                  string             // the last view's epoch, if the server sends one
	dropConn               context.CancelFunc // closes the open stream
	fresh                  bool               // the next stream starts without Last-Event-ID
}

func (s *stream) run(ctx context.Context) {
	if s.dashboard {
		go s.refreshLoop(ctx)
	}
	last, backoff, opened := "", 0, false
	for ctx.Err() == nil {
		cctx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.dropConn = cancel
		if s.fresh {
			// app.js opens a new EventSource, which sends no Last-Event-ID.
			last, s.fresh = "", false
		}
		s.mu.Unlock()
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, *baseURL+"/v1/stream?repo="+url.QueryEscape(s.repo), nil)
		s.auth(req)
		if last != "" {
			req.Header.Set("Last-Event-ID", last)
		}
		start := time.Now()
		resp, err := s.sc.Do(req)
		if err == nil && resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			err = statusError(resp.StatusCode)
		}
		if cctx.Err() != nil && ctx.Err() == nil {
			cancel()
			continue // a reset closed it before it opened
		}
		s.rec.add("GET /v1/stream (connect)", start, 0, err)
		if err != nil {
			cancel()
			// The browser gives up on an error status, and app.js tries again later.
			backoff = min(backoff+1, 4)
			sleep(ctx, 1*time.Second<<backoff)
			continue
		}
		if backoff = 0; opened {
			s.reconnects++
			s.schedule(ctx)
		}
		opened = true
		last = s.read(ctx, resp.Body, last)
		_ = resp.Body.Close()
		reset := cctx.Err() != nil
		cancel()
		if !reset {
			sleep(ctx, streamRetry)
		}
	}
}

// read takes a stream's events until it ends, and returns the last event's id.
func (s *stream) read(ctx context.Context, body io.Reader, last string) string {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var event, data string
	var seq uint64
	for sc.Scan() {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, "id: "):
			last = line[4:]
			seq, _ = strconv.ParseUint(last, 10, 64)
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		case strings.HasPrefix(line, "data: "):
			data = line[6:]
		case line == "" && event == "activity":
			var a struct{ At time.Time }
			if json.Unmarshal([]byte(data), &a) == nil {
				s.lag = append(s.lag, time.Since(a.At))
			}
			s.events++
			s.mu.Lock()
			s.maxSeq = max(s.maxSeq, seq)
			s.mu.Unlock()
			s.schedule(ctx)
			event, data = "", ""
		}
	}
	return last
}

// schedule throttles reloads as app.js does: the first event starts a timer,
// and the events before it fires ride along.
func (s *stream) schedule(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dashboard || s.timer {
		return
	}
	s.timer = true
	wait := *reloadMin
	if *jitter > 0 {
		wait += time.Duration(s.rng.Int64N(int64(*jitter)))
	}
	wait = max(wait, time.Duration(*fetchMult*float64(s.lastFetch)))
	time.AfterFunc(wait, func() {
		s.mu.Lock()
		s.timer = false
		s.mu.Unlock()
		s.refresh(ctx)
	})
}

// refresh reloads the board, one request at a time; one asked for meanwhile
// is scheduled when it ends.
func (s *stream) refresh(ctx context.Context) {
	s.mu.Lock()
	if ctx.Err() != nil || s.inflight {
		s.again = s.inflight
		s.mu.Unlock()
		return
	}
	s.inflight = true
	s.mu.Unlock()
	start := time.Now()
	view := s.getBoard()
	s.mu.Lock()
	s.lastFetch = time.Since(start)
	if view != nil {
		s.checkRestart(view)
	}
	again := s.again
	s.inflight, s.again = false, false
	s.mu.Unlock()
	if again {
		s.schedule(ctx)
	}
}

// checkRestart is app.js's test for a restarted server after a reload: a new
// epoch or, without one, a view older than an event the feed already has.
// Either starts the feed over and opens a new stream. The caller holds s.mu.
func (s *stream) checkRestart(view []byte) {
	lastSeq, hasSeq := jsonUint(view, `"last_seq":`)
	epoch, hasEpoch := jsonString(view, `"epoch":`)
	var restarted bool
	if hasEpoch && *trustEpoch {
		restarted = s.epoch != "" && epoch != s.epoch
		s.epoch = epoch
	} else {
		restarted = hasSeq && lastSeq < s.maxSeq
	}
	// The view's own recent activities join the feed too, but they are no
	// newer than its last_seq, so they cannot set off the next reload's test.
	if !restarted {
		return
	}
	s.resets++
	s.maxSeq = 0
	s.fresh = true
	if s.dropConn != nil {
		s.dropConn()
	}
}

// jsonUint finds a number field of the top-level object by its key, such as
// "last_seq": a key cannot occur inside a string, where its quotes would be
// escaped, so the last occurrence is the top level's.
func jsonUint(doc []byte, key string) (uint64, bool) {
	i := bytes.LastIndex(doc, []byte(key))
	if i < 0 {
		return 0, false
	}
	rest := doc[i+len(key):]
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	v, err := strconv.ParseUint(string(rest[:n]), 10, 64)
	return v, err == nil
}

// jsonString finds a string field of the top-level object by its key.
func jsonString(doc []byte, key string) (string, bool) {
	i := bytes.LastIndex(doc, []byte(key))
	if i < 0 {
		return "", false
	}
	var v string
	dec := json.NewDecoder(bytes.NewReader(doc[i+len(key):]))
	return v, dec.Decode(&v) == nil
}

func (s *stream) refreshLoop(ctx context.Context) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		go s.refresh(ctx)
		s.get("GET /v1/repos", "/v1/repos")
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *stream) get(key, path string) {
	s.rec.measure(s.hc, s.auth, fetchTimeout, key, http.MethodGet, path, nil, nil)
}

// getBoard reloads the board and returns the view's JSON, or nil.
func (s *stream) getBoard() []byte {
	var raw []byte
	s.rec.measure(s.hc, s.auth, fetchTimeout, "GET /v1/board", http.MethodGet, "/v1/board?repo="+url.QueryEscape(s.repo), nil, &raw)
	return raw
}

// login signs in as the dashboard does and returns the session cookie.
func login(ctx context.Context, hc http.Client, token string, rec *recorder) (*http.Cookie, error) {
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *baseURL+"/ui/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	start := time.Now()
	resp, err := hc.Do(req)
	rec.add("POST /ui/login", start, 0, err)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "intagent_session" {
			return c, nil
		}
	}
	return nil, fmt.Errorf("login answered %s without a session cookie", resp.Status)
}

// --- the run -------------------------------------------------------------

func client(keep bool, conns int) *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: !keep, MaxIdleConns: conns, MaxIdleConnsPerHost: conns}}
}

func run(ctx context.Context, tokens map[string]string) {
	began, self := time.Now(), cpuTime(os.Getpid())
	rec := &recorder{from: began.Add(*warmup), series: map[string]*series{}, counts: map[string]int{}}
	hooks := client(*keepAlive, *members**perMember)
	agents := make([]*agent, *members**perMember)
	for i := range agents {
		agents[i] = newAgent(i, tokens, hooks, rec)
	}

	// Half the streams watch the busiest repository.
	fetches, listens := client(true, *dashboards), client(true, *streams)
	sctx, stopStreams := context.WithCancel(ctx)
	var watchers []*stream
	var swg sync.WaitGroup
	rng := rand.New(rand.NewPCG(*seed, 1<<40)) //nolint:gosec // repeatable, not secure, randomness
	for i := range *streams {
		repo := 0
		if rng.Float64() >= 0.5 {
			repo = rng.IntN(*repos)
		}
		c, err := login(ctx, *fetches, tokens[memberName(i%*members)], rec)
		if err != nil {
			log.Printf("stream %d: %v", i, err)
			continue
		}
		s := &stream{repo: repoName(repo), auth: func(r *http.Request) { r.AddCookie(c) }, hc: fetches, sc: listens, rec: rec,
			dashboard: i < *dashboards, rng: rand.New(rand.NewPCG(*seed, uint64(i)+1<<41))} //nolint:gosec // repeatable, not secure, randomness
		watchers = append(watchers, s)
		swg.Add(1)
		go func() { defer swg.Done(); s.run(sctx) }()
	}

	actx, stopAgents := context.WithTimeout(ctx, *duration)
	defer stopAgents()
	proc := &procStats{pid: *pid}
	go proc.watch(actx)
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func() { defer wg.Done(); a.run(actx) }()
	}
	wg.Wait()
	elapsed := time.Since(began)
	stopStreams()
	swg.Wait()
	proc.sample()
	report(rec, watchers, agents, proc, elapsed)
	// The latencies above are the client's: they include its own waits for a CPU.
	fmt.Printf("loadgen itself: %.2f cores on average\n", (cpuTime(os.Getpid())-self)/time.Since(began).Seconds())
}

// procStats samples the server's resident memory, CPU time and disk writes from /proc.
type procStats struct {
	pid        int
	rss        []int64 // kB, a sample a second
	cpu0, cpu1 float64 // seconds
	io0, io1   int64   // bytes written to storage
	t0, t1     time.Time
}

func (p *procStats) watch(ctx context.Context) {
	for p.sample(); p.pid > 0 && sleep(ctx, time.Second); {
		p.sample()
	}
}

func (p *procStats) sample() {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.pid))
	pio, _ := os.ReadFile(fmt.Sprintf("/proc/%d/io", p.pid))
	if p.pid == 0 || err != nil {
		return
	}
	for line := range strings.Lines(string(status) + string(pio)) {
		if f := strings.Fields(line); len(f) > 1 && (f[0] == "VmRSS:" || f[0] == "write_bytes:") {
			if n, _ := strconv.ParseInt(f[1], 10, 64); f[0] == "VmRSS:" {
				p.rss = append(p.rss, n)
			} else {
				p.io1 = n
			}
		}
	}
	if p.cpu1, p.t1 = cpuTime(p.pid), time.Now(); p.t0.IsZero() {
		p.cpu0, p.io0, p.t0 = p.cpu1, p.io1, p.t1
	}
}

// cpuTime reads a process's user and system time, in Linux's usual 100 ticks a second.
func cpuTime(pid int) float64 {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))
	u, _ := strconv.ParseFloat(f[11], 64)
	k, _ := strconv.ParseFloat(f[12], 64)
	return (u + k) / 100
}

// --- the report ----------------------------------------------------------

func pct(lat []time.Duration, q float64) string {
	if len(lat) == 0 {
		return "-"
	}
	d := lat[max(0, int(math.Ceil(q*float64(len(lat))))-1)]
	return strconv.FormatFloat(float64(d)/1e6, 'f', 1, 64)
}

func report(rec *recorder, watchers []*stream, agents []*agent, proc *procStats, elapsed time.Duration) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	busiest := len(slices.DeleteFunc(slices.Clone(agents), func(a *agent) bool { return a.repo != 0 }))
	window := elapsed - *warmup
	fmt.Printf("loadgen: %d members x %d agents, %d repos (busiest: %d agents), %d dashboards, %d streams, seed %d\n",
		*members, *perMember, *repos, busiest, *dashboards, len(watchers), *seed)
	fmt.Printf("%.0f hook events/s, ramp %s, ran %s, measured %s after a %s warm-up (%d requests left out), keepalive %t, react %t\n\n",
		*rate, *ramp, elapsed.Round(time.Millisecond), window.Round(time.Millisecond), *warmup, rec.skipped, *keepAlive, *react)

	all := &series{}
	keys := []string{"POST /v1/hook, all kinds"}
	for _, k := range []board.Kind{board.KindSessionStart, board.KindPrompt, board.KindPreEdit, board.KindPostEdit,
		board.KindToolStart, board.KindToolEnd, board.KindStop, board.KindSessionEnd} {
		keys = append(keys, "hook "+string(k))
		if s := rec.series["hook "+string(k)]; s != nil {
			all.lat, all.errs, all.slow, all.bytes = append(all.lat, s.lat...), all.errs+s.errs, all.slow+s.slow, all.bytes+s.bytes
			all.unchecked += s.unchecked
		}
	}
	rec.series[keys[0]] = all
	others := slices.DeleteFunc(slices.Sorted(maps.Keys(rec.series)), func(k string) bool { return slices.Contains(keys, k) })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "endpoint\tn\tper s\terrors\tover 2s\tp50 ms\tp95 ms\tp99 ms\tmax ms\tavg KB\t")
	for _, k := range append(keys, others...) {
		if s := rec.series[k]; s != nil {
			slices.Sort(s.lat)
			n := len(s.lat)
			fmt.Fprintf(tw, "%s\t%d\t%.1f\t%d\t%d\t%s\t%s\t%s\t%s\t%.1f\t\n", k, n, float64(n)/window.Seconds(), s.errs, s.slow,
				pct(s.lat, .5), pct(s.lat, .95), pct(s.lat, .99), pct(s.lat, 1), float64(s.bytes)/1024/float64(max(n, 1)))
		}
	}
	_ = tw.Flush()
	if s := rec.series["hook pre_edit"]; s != nil {
		fmt.Printf("\npre_edits a real hook let through unchecked (error or over 2 s): %d of %d (%.2f%%)\n\n",
			s.unchecked, len(s.lat), 100*float64(s.unchecked)/float64(max(1, len(s.lat))))
	}
	for _, k := range slices.Sorted(maps.Keys(rec.counts)) {
		fmt.Printf("%-70s %d\n", k, rec.counts[k])
	}

	var events []int
	var lag []time.Duration
	total, reconnects, resets := 0, 0, 0
	for _, s := range watchers {
		events, lag = append(events, s.events), append(lag, s.lag...)
		total, reconnects, resets = total+s.events, reconnects+s.reconnects, resets+s.resets
	}
	slices.Sort(events)
	slices.Sort(lag)
	if len(events) > 0 {
		fmt.Printf("\nstreams: %d, events received %d (per stream min %d, p50 %d, max %d), reconnects %d\n",
			len(events), total, events[0], events[len(events)/2], events[len(events)-1], reconnects)
		fmt.Printf("event delivery lag ms: p50 %s, p95 %s, p99 %s, max %s\n", pct(lag, .5), pct(lag, .95), pct(lag, .99), pct(lag, 1))
	}
	if s := rec.series["GET /v1/board"]; s != nil {
		fmt.Printf("dashboard feeds started over, the view taken for a restarted server's: %d of %d reloads (%.1f%%), epoch trusted %t\n",
			resets, len(s.lat), 100*float64(resets)/float64(max(1, len(s.lat))), *trustEpoch)
	}
	if len(proc.rss) > 0 {
		secs := proc.t1.Sub(proc.t0).Seconds()
		fmt.Printf("\nserver pid %d: RSS start %d MB, peak %d MB, end %d MB; CPU %.1f s, %.2f cores on average; "+
			"wrote %.0f MB to storage, %.1f MB/s\n", proc.pid, proc.rss[0]>>10, slices.Max(proc.rss)>>10, proc.rss[len(proc.rss)-1]>>10,
			proc.cpu1-proc.cpu0, (proc.cpu1-proc.cpu0)/secs, float64(proc.io1-proc.io0)/1e6, float64(proc.io1-proc.io0)/1e6/secs)
	}
}
