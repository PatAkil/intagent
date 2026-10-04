// Package server is intagent's team server: an HTTP API over a board, a live
// event stream for the dashboard, and the loops that sweep and persist it.
package server

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/fsutil"
)

// Options configures a Server.
type Options struct {
	// Members may connect; there must be at least one.
	Members []Member
	// Version is reported by /healthz and /v1/whoami. Empty reports "dev".
	Version string
	// Board holds timings and policy.
	Board board.Config
	// DataDir keeps the board's snapshot. Empty keeps everything in memory.
	DataDir string
	// PublicRead lets anyone who can reach the server read the board and the
	// dashboard without a token. Writes still need one.
	PublicRead bool
	// Demo marks a server whose members nobody can sign in as (intagent demo).
	Demo bool
	// Logger receives operational logs. Nil discards them.
	Logger *slog.Logger
	// Now replaces the clock, for tests.
	Now func() time.Time
	// SweepEvery is how often stalled sessions are looked for. Default 15s.
	SweepEvery time.Duration
	// Dashboard serves the web UI. Nil serves no UI.
	Dashboard http.Handler
	// Webhook sends selected activities to an endpoint. Zero sends nothing.
	Webhook WebhookConfig
}

// Server serves one team's board.
type Server struct {
	board      *board.Board
	members    atomic.Pointer[[]memberHash] // swapped whole when team.json changes
	hub        *hub
	log        *slog.Logger
	now        func() time.Time
	dataDir    string
	publicRead bool
	demo       bool
	version    string
	sweepEvery time.Duration
	dashboard  http.Handler
	saved      uint64
	notifier   *notifier
	closing    chan struct{}
	closeOnce  sync.Once
	// uiKey signs dashboard sessions, so a session cookie is never a token.
	uiKey []byte
	// epoch names this process to readers. A dashboard that sees it change
	// knows the server restarted, and its event numbers may have started over.
	epoch string
	// boards shares board answers among the requests that ask at once.
	boards *boardBuilds
}

type memberHash struct {
	name string
	hash []byte
}

const (
	maxBody     = 1 << 20
	cookieName  = "intagent_session"
	sseKeepAway = 20 * time.Second
)

// New builds a server and restores its board from DataDir.
func New(o Options) (*Server, error) {
	s := &Server{
		hub:        newHub(),
		log:        o.Logger,
		now:        o.Now,
		dataDir:    o.DataDir,
		publicRead: o.PublicRead,
		demo:       o.Demo,
		version:    cmp.Or(o.Version, "dev"),
		sweepEvery: o.SweepEvery,
		dashboard:  o.Dashboard,
		closing:    make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sweepEvery <= 0 {
		s.sweepEvery = 15 * time.Second
	}
	if err := s.SetMembers(o.Members); err != nil {
		return nil, err
	}
	publish := s.hub.publish
	if o.Webhook.URL != "" {
		s.notifier = newNotifier(o.Webhook, s.log)
		publish = func(acts []board.Activity) {
			s.hub.publish(acts)
			s.notifier.enqueue(acts)
		}
	}
	s.board = board.New(o.Board, board.WithNotify(publish))
	if err := s.load(); err != nil {
		return nil, err
	}
	epoch, err := randomKey()
	if err != nil {
		return nil, err
	}
	s.epoch = hex.EncodeToString(epoch[:8])
	s.boards = newBoardBuilds(s.boardView, s.log)
	key, err := loadUIKey(s.dataDir)
	if err != nil {
		return nil, err
	}
	s.uiKey = key
	return s, nil
}

// loadUIKey reads the key that signs dashboard sessions from the data
// directory, creating it on first start, so sessions survive a restart. Without
// a data directory the key lives as long as the process.
func loadUIKey(dir string) ([]byte, error) {
	if dir == "" {
		return randomKey()
	}
	p := filepath.Join(dir, "ui.key")
	if b, err := os.ReadFile(p); err == nil && len(b) == 32 {
		return b, nil
	}
	key, err := randomKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return key, fsutil.WriteFile(p, key, 0o600)
}

func randomKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// session is a member's dashboard cookie: a MAC of their token's hash. It
// grants reading only, ends when the token is rotated, and cannot be used as a
// token or derived from the team file.
func (s *Server) session(m memberHash) string {
	mac := hmac.New(sha256.New, s.uiKey)
	mac.Write([]byte("intagent dashboard session\x00"))
	mac.Write(m.hash)
	return hex.EncodeToString(mac.Sum(nil))
}

// sessionMember finds the member a dashboard cookie belongs to.
func (s *Server) sessionMember(cookie string) (string, bool) {
	got, err := hex.DecodeString(cookie)
	if err != nil || len(got) != sha256.Size {
		return "", false
	}
	name := ""
	for _, m := range *s.members.Load() {
		want, _ := hex.DecodeString(s.session(m))
		if hmac.Equal(got, want) {
			name = m.name
		}
	}
	return name, name != ""
}

// SetMembers replaces who may use the server: new members can sign in, and a
// removed or rotated token stops working at once.
func (s *Server) SetMembers(ms []Member) error {
	list := make([]memberHash, 0, len(ms))
	for _, m := range ms {
		h, err := hex.DecodeString(m.TokenSHA256)
		if err != nil || !ValidMemberName(m.Name) {
			return fmt.Errorf("member %q is not valid", m.Name)
		}
		list = append(list, memberHash{name: m.Name, hash: h})
	}
	if len(list) == 0 {
		return errors.New("no members configured: add one with 'intagent token add <name>'")
	}
	if s.members.Swap(&list) != nil {
		// Streams were authorised against the old list: a revoked token must
		// not keep one open.
		s.hub.closeAll()
	}
	return nil
}

// Board exposes the board, for tests and embedding.
func (s *Server) Board() *board.Board { return s.board }

// Handler returns the server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("POST /v1/hook", s.write(s.handleHook))
	mux.Handle("POST /v1/intents", s.write(s.handleDeclare))
	mux.Handle("POST /v1/intents/release", s.write(s.handleRelease))
	mux.Handle("POST /v1/check", s.write(s.handleCheck))
	mux.Handle("POST /v1/notes", s.write(s.handleNote))
	mux.Handle("GET /v1/board", s.read(s.handleBoard))
	mux.Handle("GET /v1/repos", s.read(s.handleRepos))
	mux.Handle("GET /v1/stream", s.read(s.handleStream))
	mux.Handle("GET /v1/whoami", s.read(s.handleWhoami))
	mux.HandleFunc("POST /ui/login", s.handleLogin)
	mux.HandleFunc("POST /ui/logout", s.handleLogout)
	if s.dashboard != nil {
		mux.Handle("GET /", s.dashboard)
	}
	return s.logRequests(mux)
}

// Serve runs the server on ln until ctx is cancelled, then shuts down cleanly
// and writes a final snapshot.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	srv.RegisterOnShutdown(func() { s.closeOnce.Do(func() { close(s.closing) }) })
	maintainCtx, stopMaintain := context.WithCancel(context.WithoutCancel(ctx))
	maintained := make(chan struct{})
	go func() { defer close(maintained); s.maintain(maintainCtx) }()
	if s.notifier != nil {
		go s.notifier.run(maintainCtx)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = srv.Shutdown(shutdownCtx)
		cancel()
		<-errc
	}
	stopMaintain()
	<-maintained
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// --- auth ------------------------------------------------------------------

type ctxKey struct{}

func memberFrom(r *http.Request) string {
	m, _ := r.Context().Value(ctxKey{}).(string)
	return m
}

// authenticate resolves a token to a member, comparing in constant time.
func (s *Server) authenticate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	got, _ := hex.DecodeString(HashToken(token))
	name := ""
	for _, m := range *s.members.Load() {
		if subtle.ConstantTimeCompare(got, m.hash) == 1 {
			name = m.name
		}
	}
	return name, name != ""
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// write admits only bearer tokens, so a browser cookie can never cause a write.
func (s *Server) write(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := s.authenticate(bearer(r))
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, name)))
	})
}

// read admits a bearer token or the dashboard cookie, or anyone with PublicRead.
func (s *Server) read(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		name, ok := s.authenticate(token)
		if c, err := r.Cookie(cookieName); !ok && token == "" && err == nil {
			name, ok = s.sessionMember(c.Value)
		}
		// A board open to all still rejects a token it does not know: its
		// owner must find out, not be treated as anonymous.
		if !ok && (!s.publicRead || token != "") {
			writeError(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, name)))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	name, ok := s.authenticate(strings.TrimSpace(r.FormValue("token")))
	if !ok {
		http.Redirect(w, r, "/?login=failed", http.StatusSeeOther)
		return
	}
	var session string
	for _, m := range *s.members.Load() {
		if m.name == name {
			session = s.session(m)
		}
	}
	// Behind a proxy that terminates TLS, the proxy says so; a client that
	// claims it can only make its own cookie stricter.
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secure, MaxAge: 30 * 24 * 3600,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version})
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, board.Whoami{Member: memberFrom(r), Version: s.version, Policy: s.board.Config().Policy, Demo: s.demo})
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	var ev board.HookEvent
	if !decode(w, r, &ev) {
		return
	}
	ev.Member = memberFrom(r)
	res, err := s.board.Hook(s.now(), ev)
	if err != nil {
		s.log.Warn("hook rejected", "member", ev.Member, "kind", ev.Kind, "err", err)
		writeBoardError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeclare(w http.ResponseWriter, r *http.Request) {
	var req board.DeclareRequest
	if !decode(w, r, &req) {
		return
	}
	req.Member = memberFrom(r)
	res, err := s.board.Declare(s.now(), req)
	if err != nil {
		writeBoardError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req board.ReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	req.Member = memberFrom(r)
	n, err := s.board.Release(s.now(), req)
	if err != nil {
		writeBoardError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, board.ReleaseResult{Released: n})
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req board.CheckRequest
	if !decode(w, r, &req) {
		return
	}
	req.Member = memberFrom(r)
	now := s.now()
	cs, err := s.board.Check(now, req)
	if err != nil {
		writeBoardError(w, err)
		return
	}
	if cs == nil {
		cs = []board.Conflict{}
	}
	writeJSON(w, http.StatusOK, board.CheckResult{Conflicts: cs, Text: board.RenderConflicts(now, cs)})
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	var req board.NoteRequest
	if !decode(w, r, &req) {
		return
	}
	req.Member = memberFrom(r)
	res, err := s.board.Note(s.now(), req)
	if err != nil {
		writeBoardError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo := board.RepoID(q.Get("repo"))
	if repo == "" {
		writeError(w, http.StatusBadRequest, "repo is required")
		return
	}
	text := q.Get("format") == "text"
	if text && q.Has("limit") {
		s.handleAgentBoard(w, r, repo)
		return
	}
	b, err := s.boards.get(r.Context(), repo)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error("board answer failed", "repo", repo, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if text {
		writeText(w, b.view.Text()+"\n")
		return
	}
	writeBoard(w, r, b)
}

// boardView is the view a board build reads.
func (s *Server) boardView(repo string) board.View {
	v := s.board.View(s.now(), repo)
	v.Epoch = s.epoch
	return v
}

// writeBoard sends a shared board answer, compressed when the client takes
// gzip, or 304 when the client already holds an equivalent one.
func writeBoard(w http.ResponseWriter, r *http.Request, b *boardBuild) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Vary", "Accept-Encoding")
	h.Set("Cache-Control", "private, no-cache")
	if b.etag != "" {
		h.Set("ETag", b.etag)
		if etagMatch(r.Header.Get("If-None-Match"), b.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	body := b.json
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = b.gz
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = progress(w).Write(body)
}

// handleAgentBoard answers an agent asking about its teammates' work: the
// claims nearest the caller's first, at most limit of them.
func (s *Server) handleAgentBoard(w http.ResponseWriter, r *http.Request, repo string) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		writeError(w, http.StatusBadRequest, "limit must be a whole number above zero")
		return
	}
	where := board.Where{Repo: repo, Host: q.Get("host"), Worktree: q.Get("worktree")}
	writeText(w, s.board.AgentText(s.now(), where, memberFrom(r), limit)+"\n")
}

func (s *Server) handleRepos(w http.ResponseWriter, _ *http.Request) {
	repos := s.board.Repos(s.now())
	for i := range repos {
		repos[i].Epoch = s.epoch
	}
	writeJSON(w, http.StatusOK, repos)
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip: named
// with a quality above zero, or covered by a "*" that is.
func acceptsGzip(header string) bool {
	gzip, star := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				} else {
					q = 0
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "gzip", "x-gzip":
			gzip = max(gzip, q)
		case "*":
			star = max(star, q)
		}
	}
	if gzip >= 0 {
		return gzip > 0
	}
	return star > 0
}

// etagMatch reports whether an If-None-Match header names tag, compared
// weakly as RFC 9110 has it for GET.
func etagMatch(header, tag string) bool {
	if header == "" || tag == "" {
		return false
	}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == strings.TrimPrefix(tag, "W/") {
			return true
		}
	}
	return false
}

// handleStream sends activities as server-sent events. A client reconnecting
// with Last-Event-ID first receives what it missed.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	repo := board.RepoID(r.URL.Query().Get("repo"))
	sub := s.hub.subscribe(repo)
	defer s.hub.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, "retry: 3000\n: connected\n\n")
	last, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if last > 0 {
		for _, a := range s.board.Since(repo, last) {
			if !sendEvent(w, a) {
				return
			}
			last = a.Seq
		}
	}
	flusher.Flush()

	keep := time.NewTicker(sseKeepAway)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-sub.dropped:
			return
		case <-keep.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case a := <-sub.ch:
			if a.Seq <= last {
				continue
			}
			if !sendEvent(w, a) {
				return
			}
			last = a.Seq
			flusher.Flush()
		}
	}
}

func sendEvent(w io.Writer, a board.Activity) bool {
	data, err := json.Marshal(a)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: activity\ndata: %s\n\n", a.Seq, data)
	return err == nil
}

// --- plumbing ------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// writeTimeout bounds how long an answer may go without progress: each piece
// of it must reach the client's connection within this time, as nginx's
// send_timeout has it. The server has no WriteTimeout, which would end event
// streams, so without this a client that stopped reading (a laptop asleep
// mid-download, a paused pipe) would hold its handler and the answer's memory
// until TCP gave up, while a slow client that keeps reading still gets
// everything. A variable for tests.
var writeTimeout = 15 * time.Second

// progressChunk is how much of an answer one write deadline covers.
const progressChunk = 64 << 10

// progressWriter re-arms the write deadline before each piece of an answer.
type progressWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func progress(w http.ResponseWriter) progressWriter {
	return progressWriter{w: w, rc: http.NewResponseController(w)}
}

func (p progressWriter) Write(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		k := min(len(b), progressChunk)
		// A writer that takes no deadline (a test recorder) writes without one.
		if err := p.rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return n, err
		}
		m, err := p.w.Write(b[:k])
		n += m
		if err != nil {
			return n, err
		}
		b = b[k:]
	}
	return n, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(progress(w)).Encode(v)
}

func writeText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(progress(w), text)
}

// ErrorResponse is the body of every error.
type ErrorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func writeBoardError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, board.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, board.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, board.ErrNoTarget):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush lets server-sent events through the logging wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		lvl := slog.LevelDebug
		if sw.status >= 500 {
			lvl = slog.LevelError
		}
		s.log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "took", time.Since(start).Round(time.Microsecond))
	})
}
