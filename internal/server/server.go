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
	// Streams caps the dashboard streams open at once.
	Streams StreamLimits
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
		hub:        newHub(o.Streams),
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
func (s *Server) sessionMember(cookie string) (memberHash, bool) {
	got, err := hex.DecodeString(cookie)
	if err != nil || len(got) != sha256.Size {
		return memberHash{}, false
	}
	var found memberHash
	for _, m := range *s.members.Load() {
		want, _ := hex.DecodeString(s.session(m))
		if hmac.Equal(got, want) {
			found = m
		}
	}
	return found, found.name != ""
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
		// not keep one open, and the others stay, so adding a member or a
		// touch of the file disconnects nobody. The list is swapped first:
		// handleStream checks a stream that subscribes after this against it.
		s.hub.closeUnless(s.admits)
	}
	return nil
}

// admits reports whether the credential that authorised a request is still
// listed: the same member with the same token, or nobody on a board open to
// all, which the member list does not change.
func (s *Server) admits(who memberHash) bool {
	if who.name == "" {
		return true
	}
	for _, m := range *s.members.Load() {
		if m.name == who.name && subtle.ConstantTimeCompare(m.hash, who.hash) == 1 {
			return true
		}
	}
	return false
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

// memberFrom is the member a request was authorised as; "" for nobody, on a
// board open to all.
func memberFrom(r *http.Request) string { return authorityFrom(r).name }

// authorityFrom is the member and token hash a request was authorised by.
func authorityFrom(r *http.Request) memberHash {
	m, _ := r.Context().Value(ctxKey{}).(memberHash)
	return m
}

// authenticate resolves a token to a member, comparing in constant time.
func (s *Server) authenticate(token string) (memberHash, bool) {
	if token == "" {
		return memberHash{}, false
	}
	got, _ := hex.DecodeString(HashToken(token))
	var found memberHash
	for _, m := range *s.members.Load() {
		if subtle.ConstantTimeCompare(got, m.hash) == 1 {
			found = m
		}
	}
	return found, found.name != ""
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
		who, ok := s.authenticate(bearer(r))
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, who)))
	})
}

// read admits a bearer token or the dashboard cookie, or anyone with PublicRead.
func (s *Server) read(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		who, ok := s.authenticate(token)
		if c, err := r.Cookie(cookieName); !ok && token == "" && err == nil {
			who, ok = s.sessionMember(c.Value)
		}
		// A board open to all still rejects a token it does not know: its
		// owner must find out, not be treated as anonymous.
		if !ok && (!s.publicRead || token != "") {
			writeError(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, who)))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	who, ok := s.authenticate(strings.TrimSpace(r.FormValue("token")))
	if !ok {
		http.Redirect(w, r, "/?login=failed", http.StatusSeeOther)
		return
	}
	session := s.session(who)
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
	repo := board.RepoID(r.URL.Query().Get("repo"))
	if repo == "" {
		writeError(w, http.StatusBadRequest, "repo is required")
		return
	}
	v := s.board.View(s.now(), repo)
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, v.Text()+"\n")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleRepos(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.board.Repos(s.now()))
}

// handleStream sends activities as server-sent events. A client reconnecting
// with Last-Event-ID first receives what it missed.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	repo := board.RepoID(r.URL.Query().Get("repo"))
	who := authorityFrom(r)
	sub, ok := s.hub.subscribe(repo, who)
	if !ok {
		// A dashboard's EventSource gives up on an error status, and the
		// dashboard tries again after its own backoff.
		w.Header().Set("Retry-After", strconv.Itoa(streamRetryAfter))
		writeError(w, http.StatusTooManyRequests, "too many open streams; close a dashboard or try again later")
		return
	}
	defer s.hub.unsubscribe(sub)
	// SetMembers may have swapped the list after this request was
	// authorised, and closed what it no longer admits before this stream
	// subscribed; it is checked here against the new list instead.
	if !s.admits(who) {
		writeError(w, http.StatusUnauthorized, "missing or unknown token")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	out := newStreamWriter(w, repo)
	out.raw("retry: 3000\n: connected\n\n")
	if last, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); last > 0 {
		out.last = last
		for _, a := range s.board.Since(repo, last) {
			if f, ok := activityFrame(a); ok {
				out.frames([]frame{f})
			}
		}
	}
	if out.flush() != nil {
		return
	}

	keep := time.NewTicker(sseKeepAway)
	defer keep.Stop()
	// After each write the stream lingers before it takes more: what is
	// published meanwhile waits in the hub and goes out in the next write.
	linger := time.NewTimer(streamLinger)
	linger.Stop()
	defer linger.Stop()
	pos, wake, lingering := sub.pos, sub.wake, false
	var batches [][]frame
	for {
		next := wake
		if lingering {
			next = nil
		}
		select {
		case <-r.Context().Done():
			// The server is shutting down (requests share Serve's context),
			// or the client has gone and the goodbye goes nowhere.
			out.goodbye(sub.retry)
			return
		case <-s.closing:
			out.goodbye(sub.retry)
			return
		case <-sub.done:
			out.goodbye(sub.retry)
			return
		case <-linger.C:
			lingering = false
		case <-keep.C:
			out.raw(": keep-alive\n\n")
			if out.flush() != nil {
				return
			}
		case <-next:
			var ok bool
			batches, pos, wake, ok = s.hub.since(pos, batches[:0])
			if !ok {
				out.goodbye(sub.retry) // too far behind: the client reconnects and catches up
				return
			}
			for _, b := range batches {
				out.frames(b)
			}
			clear(batches)
			if out.flush() != nil {
				return
			}
			lingering = true
			linger.Reset(streamLinger)
		}
	}
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

// FlushError flushes, and says when the client could not be written to: a
// stream whose write deadline passed ends at once.
func (w *statusWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
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
