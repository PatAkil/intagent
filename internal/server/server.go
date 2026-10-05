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
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// MaxConnections caps the connections open at once; more are closed as
	// soon as they are accepted. Zero takes DefaultMaxConnections.
	MaxConnections int
	// TLS serves HTTPS with this configuration; nil serves HTTP. Serve adds
	// TLS to the listener it is given, which must not have it already.
	TLS *tls.Config
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
	// clock measures how long requests wait. It is not the board's time,
	// which Options.Now may replace; tests replace this one too.
	clock func() time.Time
	// answers follows whether agents get their answers in time.
	answers *answers
	tls     *tls.Config
	// maxConns, readTimeout and handshakeTimeout bound what clients can
	// hold: connections, and the time to send a request or finish a TLS
	// handshake. As the server stops, a connection that has not sent a
	// whole request is closed once it has sent nothing for unusedAfter.
	maxConns         int
	readTimeout      time.Duration
	handshakeTimeout time.Duration
	unusedAfter      time.Duration
	// admit meters what members ask of the server.
	admit *admission
	// saves follows the board's snapshots (persist.go). saveFile writes
	// one, and slowSave is how long one may take before it is logged;
	// tests replace both.
	saves    saves
	saveFile func(path string, perm fs.FileMode, fill func(io.Writer) error) error
	slowSave time.Duration
}

type memberHash struct {
	name string
	hash []byte
	// session is the member's dashboard cookie, computed when the list is.
	session [sha256.Size]byte
}

const (
	maxBody     = 1 << 20
	cookieName  = "intagent_session"
	sseKeepAway = 20 * time.Second
	// readTimeout is how long a client may take to send a whole request; a
	// hook's is sent in milliseconds, and its client waits two seconds.
	readTimeout = 15 * time.Second
	// maxHeaderBytes is far above what intagent's clients and browsers send.
	maxHeaderBytes = 16 << 10
	// maxToken bounds a bearer token, far above the 51 bytes of a real one,
	// so a request cannot make the server hash a megabyte to reject it.
	maxToken = 256
)

// New builds a server and restores its board from DataDir.
func New(o Options) (*Server, error) {
	s := &Server{
		hub:        newHub(o.Streams),
		log:        o.Logger,
		now:        o.Now,
		clock:      time.Now,
		answers:    newAnswers(),
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
	s.tls, s.maxConns, s.readTimeout, s.handshakeTimeout = o.TLS, o.MaxConnections, readTimeout, handshakeTimeout
	s.unusedAfter = unusedAfter
	if s.maxConns <= 0 {
		s.maxConns = DefaultMaxConnections
	}
	s.admit = newAdmission()
	s.saveFile, s.slowSave = fsutil.WriteFileFunc, slowSave
	// The key comes first: SetMembers computes each member's dashboard cookie
	// with it.
	key, err := loadUIKey(s.dataDir)
	if err != nil {
		return nil, err
	}
	s.uiKey = key
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
	s.board = board.New(o.Board, board.WithNotify(publish), board.WithLogger(s.log))
	if err := s.load(); err != nil {
		return nil, err
	}
	epoch, err := randomKey()
	if err != nil {
		return nil, err
	}
	s.epoch = hex.EncodeToString(epoch[:8])
	s.boards = newBoardBuilds(s.boardView, s.boardLoad, s.log)
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

// sessionMAC is a member's dashboard cookie: a MAC of their token's hash. It
// grants reading only, ends when the token is rotated, and cannot be used as a
// token or derived from the team file.
func sessionMAC(key, tokenHash []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("intagent dashboard session\x00"))
	mac.Write(tokenHash)
	var out [sha256.Size]byte
	mac.Sum(out[:0])
	return out
}

// session returns a member's dashboard cookie.
func (s *Server) session(m memberHash) string { return hex.EncodeToString(m.session[:]) }

// sessionMember finds the member a dashboard cookie belongs to. It compares
// with every member's cookie, computed when the list was, in constant time:
// anyone who can reach the server can send a cookie, so checking one must not
// cost a MAC per member.
func (s *Server) sessionMember(cookie string) (memberHash, bool) {
	var got [sha256.Size]byte
	if len(cookie) != hex.EncodedLen(len(got)) {
		return memberHash{}, false
	}
	if _, err := hex.Decode(got[:], []byte(cookie)); err != nil {
		return memberHash{}, false
	}
	var found memberHash
	for _, m := range *s.members.Load() {
		if subtle.ConstantTimeCompare(got[:], m.session[:]) == 1 {
			found = m
		}
	}
	return found, found.name != ""
}

// SetMembers replaces who may use the server: new members can sign in, and a
// removed or rotated token stops working at once.
func (s *Server) SetMembers(ms []Member) error {
	if len(s.uiKey) == 0 {
		// A cookie computed without the key could be made from the team file.
		return errors.New("the dashboard key must be loaded before the members")
	}
	list := make([]memberHash, 0, len(ms))
	for _, m := range ms {
		h, err := hex.DecodeString(m.TokenSHA256)
		if err != nil || !ValidMemberName(m.Name) {
			return fmt.Errorf("member %q is not valid", m.Name)
		}
		list = append(list, memberHash{name: m.Name, hash: h, session: sessionMAC(s.uiKey, h)})
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

// isMember reports whether name is a member of the team.
func (s *Server) isMember(name string) bool {
	for _, m := range *s.members.Load() {
		if m.name == name {
			return true
		}
	}
	return false
}

// Board exposes the board, for tests and embedding.
func (s *Server) Board() *board.Board { return s.board }

// hookRoute is the route of hook events, whose large bodies are metered by
// kind.
const hookRoute = "POST /v1/hook"

// Handler returns the server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle(hookRoute, s.write(s.handleHook))
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

// Serve runs the server on ln until ctx is cancelled, then shuts down cleanly,
// writes a final snapshot and sends the webhook what is still waiting. With
// Options.TLS, it serves HTTPS on ln. It returns the final snapshot's error,
// if it could not be written.
//
// Open ln just before: a hook that waited in its queue before Serve ran is
// taken to have arrived when Serve reads it, and may be decided after its
// client has stopped waiting for the answer.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Requests keep their context while the server stops: Shutdown waits
		// for them, and a hook whose context ended would be taken for one
		// whose agent had stopped waiting.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		// A body sent a byte at a time, with a token or without one, would
		// otherwise hold its connection for as long as it keeps sending.
		ReadTimeout:    s.readTimeout,
		MaxHeaderBytes: maxHeaderBytes,
	}
	// The limit counts TCP connections, below TLS, so that the HTTP server
	// still sees each TLS connection as one, for HTTP/2.
	ln = newLimitListener(ln, s.maxConns, s.log)
	if s.tls != nil {
		ln = &tlsListener{Listener: ln, config: s.tls, timeout: s.handshakeTimeout}
	}
	srv.RegisterOnShutdown(func() { s.closeOnce.Do(func() { close(s.closing) }) })
	fresh := &freshConns{conns: map[net.Conn]struct{}{}, quiet: s.unusedAfter}
	srv.ConnState = fresh.track
	srv.RegisterOnShutdown(fresh.closeAll)
	// The sweeper and the persister stop as the server starts to stop: no
	// sweep runs while it does, and a save in flight is abandoned for the
	// final one, made once the requests are done.
	maintainCtx, stopMaintain := context.WithCancel(context.WithoutCancel(ctx))
	swept, persisted, watched := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { defer close(swept); s.sweep(maintainCtx) }()
	go func() { defer close(persisted); s.persist(maintainCtx) }()
	go func() { defer close(watched); s.watchLoad(maintainCtx) }()
	// The notifier stops last, so its final message carries everything
	// recorded before the requests, the sweeper and the load watcher stopped.
	notifyCtx, stopNotify := context.WithCancel(context.WithoutCancel(ctx))
	notified := make(chan struct{})
	go func() {
		defer close(notified)
		if s.notifier != nil {
			s.notifier.run(notifyCtx)
		}
	}()

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
	<-swept
	<-persisted
	<-watched
	stopNotify()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if serr := s.save(nil); serr != nil {
		// A clean stop that could not save loses what changed since the
		// last save: serve must not exit as if it had not.
		err = errors.Join(err, fmt.Errorf("final snapshot: %w", serr))
	}
	if s.dataDir != "" {
		// The snapshot holds the board since its last change; the next
		// start counts as downtime only the time from here.
		s.markAlive()
	}
	<-notified
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
	if token == "" || len(token) > maxToken {
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

// write admits only bearer tokens, so a browser cookie can never cause a write,
// and meters large bodies.
func (s *Server) write(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Pattern == hookRoute {
			r = s.arrived(r)
		}
		who, ok := s.authenticate(bearer(r))
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		release, edit, ok := s.admitBody(w, r, who.name)
		if !ok {
			return
		}
		defer release()
		ctx := context.WithValue(r.Context(), ctxKey{}, who)
		if edit {
			ctx = context.WithValue(ctx, editKey{}, true)
		}
		h(w, r.WithContext(ctx))
	})
}

// editKey marks a request whose body was admitted as a pre_edit's.
type editKey struct{}

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

// health is what /healthz reports. It answers 200 while the server runs,
// degraded or not: a probe that restarted it then would turn a slow server
// into one that answers nothing. With ?strict=1 a degraded server, or one
// that cannot save its board, answers 503, for monitors.
type health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	LoadStatus
	// Snapshot says whether the board is saved, on a server that saves it.
	// Saves that fail answer 503 with ?strict=1 too.
	Snapshot *SnapshotStatus `json:"snapshot,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := health{OK: true, Version: s.version, LoadStatus: s.answers.status(s.clock())}
	if s.dataDir != "" {
		h.Snapshot = s.saves.status()
	}
	status := http.StatusOK
	if (h.Degraded || (h.Snapshot != nil && !h.Snapshot.OK)) && r.URL.Query().Get("strict") == "1" {
		h.OK, status = false, http.StatusServiceUnavailable
	}
	writeJSON(w, status, h)
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, board.Whoami{Member: memberFrom(r), Version: s.version, Policy: s.board.Config().Policy, Demo: s.demo})
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	caller := s.hookWaiter(r)
	var ev board.HookEvent
	if !decode(w, r, &ev) {
		return
	}
	if edit, _ := r.Context().Value(editKey{}).(bool); edit && ev.Kind != board.KindPreEdit {
		// It started as a pre_edit, and so drew on the budget for edits, but
		// a later field gave it another kind.
		writeError(w, http.StatusBadRequest, "invalid JSON body: it starts as a pre_edit and is not one")
		return
	}
	ev.Member = memberFrom(r)
	ev.Late = caller.late
	if ev.Kind == board.KindPreEdit {
		stop := s.watchCall(caller)
		defer stop()
	}
	ev.Waited = caller.waited()
	res, err := s.board.Hook(s.now(), ev)
	switch {
	case errors.Is(err, board.ErrAbandoned):
		// Its agent went ahead without the answer, which can only be the
		// allow it acted on; a client still waiting learns it was unchecked.
		if ev.Kind == board.KindPreEdit {
			s.uncheckedCall(caller)
		}
		s.log.Debug("hook not answered: its agent stopped waiting", "member", ev.Member, "kind", ev.Kind, "waited", caller.waited())
	case err != nil:
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
	release, ok := s.admitCall(w, req.Member, req.Where)
	if !ok {
		return
	}
	defer release()
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
	release, ok := s.admitCall(w, req.Member, req.Where)
	if !ok {
		return
	}
	defer release()
	now := s.now()
	res, err := s.board.Check(now, req)
	if err != nil {
		writeBoardError(w, err)
		return
	}
	if res.Conflicts == nil {
		res.Conflicts = []board.Conflict{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	var req board.NoteRequest
	if !decode(w, r, &req) {
		return
	}
	req.Member = memberFrom(r)
	// A note to a teammate with no claim in the repository yet waits for
	// their first session there.
	req.ToMember = s.isMember(strings.TrimSpace(req.To))
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

// boardAnswer is a repository's view as /v1/board sends it. While agents'
// edits go ahead unchecked, Server says so, for the dashboard's banner; it is
// left out otherwise, since a healthy server's counts change with every edit
// in any repository and would make every answer differ from the one a
// dashboard holds (ETag).
type boardAnswer struct {
	board.View
	Server *LoadStatus `json:"server,omitempty"`
}

// boardLoad is the load status a board answer carries: only while degraded.
func (s *Server) boardLoad() *LoadStatus {
	if st := s.answers.status(s.clock()); st.Degraded {
		return &st
	}
	return nil
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
// with Last-Event-ID first receives what it missed, after a gap event when the
// board no longer holds all of it.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// A stream reads nothing after its request, so the time limit on reading
	// one does not apply to it.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
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
	// Read after subscribing: a change from now on reaches the stream through
	// the hub, so none falls between the two.
	status := s.answers.status(s.clock())

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	out := newStreamWriter(w, repo)
	out.raw("retry: 3000\n: connected\n\n")
	if status.Degraded {
		out.add(statusEvent(status))
	}
	if last, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); last > 0 {
		out.last = last
		acts, complete := s.board.Replay(repo, last)
		if !complete {
			// The board no longer holds all the client missed. A dashboard
			// marks the hole and reloads the board; older ones listen only for
			// activities and ignore this.
			out.event("gap", fmt.Appendf(nil, `{"after":%d}`, last))
		}
		for _, a := range acts {
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
			// The client has gone, and the goodbye goes nowhere, or an
			// http.Server other than Serve's ends its requests as it stops.
			out.goodbye(sub.retry)
			return
		case <-s.closing:
			// Serve is stopping. Its requests keep their context while it
			// does, so this, not the context, ends the stream.
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
		// A 503 answers load, which admission logs in summary (loadLog), as
		// a strict probe of a degraded server is answered.
		if sw.status >= 500 && sw.status != http.StatusServiceUnavailable {
			lvl = slog.LevelError
		}
		s.log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "took", time.Since(start).Round(time.Microsecond))
	})
}
