package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"hash/maphash"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// Every open dashboard reloads its repository's board a moment after each of
// the repository's events, so the same answer is asked for by every tab at
// once. Building it for each request would hold the board's lock once per tab
// and encode the same megabytes again and again; instead, requests share
// builds. A request joins the build that has not read the board yet, so no
// answer is older than its request, and builds of one repository are spaced
// out, so a repository costs a few builds a second however many read it.
// ?repo= is anyone's input, so only repositories the board has something of
// have builds of their own. Every name it has nothing of shares one paced
// build of the empty view, which each request's answer then names: made-up
// names take no repository's place, and cost no more however many there are.
// The repository list, which every dashboard asks for on the same timer and
// which walks every claim and session on the board, is shared and paced the
// same way.

const (
	// boardSpacing is the least time between the starts of two builds of one
	// repository's board. A build that read the board for long spaces the
	// next one further: twice as long as its read, so a repository's builds
	// hold the board's lock at most half the time.
	boardSpacing = 250 * time.Millisecond
	// maxBoardEntries bounds the repositories whose builds are paced at once.
	// Only repositories on the board take one, so made-up names cannot use
	// them up; past it, a request builds alone.
	maxBoardEntries = 256
	// nowhere is the repository the shared build of the empty view reads: a
	// request names none, since the server refuses one without a name.
	nowhere = ""
	// allRepos keys the builds of the repository list. No repository's name
	// holds a space (board.RepoID).
	allRepos = "all repositories"
)

// errBoardBuild is what the requests sharing a build get when it failed.
var errBoardBuild = errors.New("building the board failed")

// boardBuild is one read of a repository's board, encoded once and shared,
// read-only, by every request that joined it.
type boardBuild struct {
	done chan struct{} // closed once the fields below are final
	// requests counts the requests sharing the build; guarded by
	// boardBuilds.mu until the build has started.
	requests int
	view     board.View
	json     []byte // the answer, as writeJSON would write it
	gz       []byte // json compressed with gzip.BestSpeed; nil to send json as it is
	etag     string // a weak validator; empty when none could be made
	err      error
	// load is the server's load status, read after the view, while degraded.
	load *LoadStatus
}

// boardEntry paces one repository's builds. Its fields are guarded by
// boardBuilds.mu, except turn.
type boardEntry struct {
	next    *boardBuild   // the build requests join; nil once it reads the board
	turn    chan struct{} // held by the build in progress: one at a time
	pending int           // builds made and not yet finished
	start   time.Time     // when the latest build started reading the board
	took    time.Duration // how long it read
}

// gap is how long after the latest build started the next may start.
func (e *boardEntry) gap() time.Duration { return max(boardSpacing, 2*e.took) }

// boardBuilds coalesces reads of the board, per repository.
type boardBuilds struct {
	view func(repo string) board.View
	// shows reports whether the board has anything of a repository to show;
	// the others share the builds of the empty view, paced by empty.
	shows func(repo string) bool
	// list reads the repository list, for the builds paced by all.
	list func() []board.RepoSummary
	// load reports the server's load status for answers to carry.
	load func() *LoadStatus
	// clock and sleep pace builds. They are the process's own clock, not the
	// board's: pacing is about load, not about what the board shows.
	clock func() time.Time
	sleep func(time.Duration)
	log   *slog.Logger
	// encode makes a build's answer from its view; a field for tests.
	encode func(b *boardBuild, v board.View)
	// slot lets one build read a board at a time across the server, so a
	// hook waits behind at most one read; other reads of a whole repository
	// or of the whole board take it too (reading). Encoding takes no lock
	// and runs outside it. Encodings stay few all the same: each repository
	// encodes one build at a time, and builds start no faster than they can
	// read.
	slot chan struct{}

	mu    sync.Mutex
	repos map[string]*boardEntry
	empty *boardEntry // paces the builds of nowhere, kept apart from repos
	all   *boardEntry // paces the builds of the repository list
}

func newBoardBuilds(view func(repo string) board.View, shows func(repo string) bool, list func() []board.RepoSummary,
	load func() *LoadStatus, log *slog.Logger,
) *boardBuilds {
	return &boardBuilds{
		view:   view,
		shows:  shows,
		list:   list,
		load:   load,
		encode: encodeBoard,
		clock:  time.Now,
		sleep:  time.Sleep,
		log:    log,
		slot:   make(chan struct{}, 1),
		repos:  map[string]*boardEntry{},
		empty:  &boardEntry{turn: make(chan struct{}, 1)},
		all:    &boardEntry{turn: make(chan struct{}, 1)},
	}
}

// get returns a build of repo's board that read the board after get was
// called. Requests that arrive while a build waits for its turn share it. A
// repository the board has nothing of is answered from the shared build of
// the empty view, under its own name.
func (c *boardBuilds) get(ctx context.Context, repo string) (*boardBuild, error) {
	if c.shows(repo) {
		return c.join(ctx, repo)
	}
	b, err := c.join(ctx, nowhere)
	if err != nil {
		return nil, err
	}
	return named(b, repo)
}

// repoList returns a build of the repository list that read the board after
// repoList was called, shared as a repository's builds are.
func (c *boardBuilds) repoList(ctx context.Context) (*boardBuild, error) {
	return c.join(ctx, allRepos)
}

// reading runs read, a read of a whole repository or of the whole board under
// its lock, once no build or other such read is reading.
func (c *boardBuilds) reading(read func()) {
	c.slot <- struct{}{}
	defer func() { <-c.slot }()
	read()
}

// join returns a build of repo's board, or of the list for allRepos, that
// read the board after join was called, shared with the requests that join
// it.
func (c *boardBuilds) join(ctx context.Context, repo string) (*boardBuild, error) {
	c.mu.Lock()
	e := c.entry(repo)
	if b := e.next; b != nil {
		b.requests++
		c.mu.Unlock()
		select {
		case <-b.done:
			return b, b.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b := &boardBuild{done: make(chan struct{}), requests: 1}
	e.next = b
	e.pending++
	c.mu.Unlock()
	// The request that asked first builds, for itself and everyone who joins,
	// even if its own client goes away meanwhile.
	c.run(repo, e, b)
	return b, b.err
}

// entry returns repo's entry, first dropping entries with nothing left to
// pace. When every slot is taken it returns an entry that is not kept.
func (c *boardBuilds) entry(repo string) *boardEntry {
	switch repo {
	case nowhere:
		return c.empty
	case allRepos:
		return c.all
	}
	if e := c.repos[repo]; e != nil {
		return e
	}
	now := c.clock()
	for r, e := range c.repos {
		if e.pending == 0 && now.Sub(e.start) >= e.gap() {
			delete(c.repos, r)
		}
	}
	e := &boardEntry{turn: make(chan struct{}, 1)}
	if len(c.repos) < maxBoardEntries {
		c.repos[repo] = e
	}
	return e
}

// named answers a request for repo, a repository the board has nothing of,
// from b, a build of nowhere: the same empty view under repo's name, encoded
// for this request alone. It is a few hundred bytes, sent as they are.
func named(b *boardBuild, repo string) (*boardBuild, error) {
	v := b.view
	v.Repo = repo
	body, err := answerJSON(v, b.load)
	if err != nil {
		return nil, err
	}
	return &boardBuild{requests: 1, view: v, json: body, etag: boardETag(body), load: b.load}, nil
}

// run makes build b of repo, or of the list for allRepos, once the build
// before it has finished and its gap has passed.
func (c *boardBuilds) run(repo string, e *boardEntry, b *boardBuild) {
	b.err = errBoardBuild // kept if the build panics: its requests then fail, not hang
	defer func() {
		c.mu.Lock()
		e.pending--
		c.mu.Unlock()
		close(b.done)
	}()
	e.turn <- struct{}{}
	defer func() { <-e.turn }()
	c.mu.Lock()
	wait := e.start.Add(e.gap()).Sub(c.clock())
	c.mu.Unlock()
	if wait > 0 {
		c.sleep(wait)
	}
	if repo == allRepos {
		var list []board.RepoSummary
		requests, read := c.read(e, b, func() { list = c.list() })
		b.json, b.err = listJSON(list)
		c.log.Debug("repository list built", "requests", requests, "read", read.Round(time.Microsecond),
			"bytes", len(b.json))
		return
	}
	var v board.View
	requests, read := c.read(e, b, func() { v = c.view(repo) })
	b.load = c.load()
	start := c.clock()
	c.encode(b, v)
	c.log.Debug("board built", "repo", repo, "requests", requests, "read", read.Round(time.Microsecond),
		"encoded", c.clock().Sub(start).Round(time.Microsecond), "bytes", len(b.json), "gzip_bytes", len(b.gz))
}

// read runs fn, which reads the board for build b, once no other build is
// reading, and notes in e when it started and how long it took. b stops
// taking requests just before. It returns the requests sharing b and the
// time the read took.
func (c *boardBuilds) read(e *boardEntry, b *boardBuild, fn func()) (int, time.Duration) {
	c.slot <- struct{}{}
	defer func() { <-c.slot }()
	c.mu.Lock()
	e.next = nil // from here on, requests wait for the next build
	start := c.clock()
	e.start = start
	requests := b.requests
	c.mu.Unlock()

	fn()
	took := c.clock().Sub(start)
	c.mu.Lock()
	e.took = took
	c.mu.Unlock()
	return requests, took
}

// listJSON is the repository list as writeJSON would write it.
func listJSON(list []board.RepoSummary) ([]byte, error) {
	body, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// encodeBoard makes b's answer from v and b.load: the JSON writeJSON would
// write, the same compressed, and a validator.
func encodeBoard(b *boardBuild, v board.View) {
	body, err := answerJSON(v, b.load)
	if err != nil {
		b.err = err
		return
	}
	gz, err := gzipped(body)
	if err != nil {
		b.err = err
		return
	}
	b.view, b.json, b.gz, b.etag, b.err = v, body, gz, boardETag(body), nil
}

// answerJSON is the answer for v and the load status it carries, as writeJSON
// would write it.
func answerJSON(v board.View, load *LoadStatus) ([]byte, error) {
	body, err := json.Marshal(boardAnswer{View: v, Server: load})
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// gzipWriters keeps compressors for reuse: each holds most of a megabyte of
// tables.
var gzipWriters = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed) // the level is valid
	return zw
}}

// gzipped compresses body with gzip.BestSpeed.
func gzipped(body []byte) ([]byte, error) {
	zw, _ := gzipWriters.Get().(*gzip.Writer)
	defer func() {
		zw.Reset(io.Discard) // the pool keeps no answer alive
		gzipWriters.Put(zw)
	}()
	var buf bytes.Buffer
	buf.Grow(len(body) / 4)
	zw.Reset(&buf)
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// etagSeed keys the validators of this process; the epoch in every answer
// already sets them apart from another process's.
var etagSeed = maphash.MakeSeed()

// boardETag returns a weak validator for an encoded view: a hash of all of it
// but the time it was read ("at") and the team-wide event count ("last_seq"),
// which differ from one build to the next. Builds with equal tags show the same
// claims, sessions, feed and counts. It returns "" for an encoding of another
// shape, and the answer then has no validator.
func boardETag(body []byte) string {
	// The view starts {"repo":"<id>","at":"<time>"; a repository id holds no
	// quote or backslash, so the first quote after it ends it.
	open := []byte(`{"repo":"`)
	if !bytes.HasPrefix(body, open) {
		return ""
	}
	i := len(open) + bytes.IndexByte(body[len(open):], '"') + 1
	at := []byte(`,"at":"`)
	if i <= len(open) || !bytes.HasPrefix(body[i:], at) {
		return ""
	}
	j := bytes.IndexByte(body[i+len(at):], '"')
	if j < 0 {
		return ""
	}
	j += i + len(at) + 1
	// last_seq comes after every nested object; a quote inside a string is
	// escaped, so its key cannot occur anywhere else.
	seq := []byte(`,"last_seq":`)
	k := bytes.LastIndex(body, seq)
	if k < j {
		return ""
	}
	l := k + len(seq)
	for l < len(body) && body[l] >= '0' && body[l] <= '9' {
		l++
	}
	var h maphash.Hash
	h.SetSeed(etagSeed)
	_, _ = h.Write(body[:i])
	_, _ = h.Write(body[j:k])
	_, _ = h.Write(body[l:])
	return `W/"` + strconv.FormatUint(h.Sum64(), 36) + `"`
}
