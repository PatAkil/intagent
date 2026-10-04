package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"hash/maphash"
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

const (
	// boardSpacing is the least time between the starts of two builds of one
	// repository's board. A build that took long spaces the next one further.
	boardSpacing = 250 * time.Millisecond
	// maxBoardEntries bounds the repositories whose builds are paced at once:
	// ?repo= is anyone's input. Past it, a request builds alone.
	maxBoardEntries = 256
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
	gz       []byte // json compressed with gzip.BestSpeed
	etag     string // a weak validator; empty when none could be made
	err      error
}

// boardEntry paces one repository's builds. Its fields are guarded by
// boardBuilds.mu, except turn.
type boardEntry struct {
	next    *boardBuild   // the build requests join; nil once it reads the board
	turn    chan struct{} // held by the build in progress: one at a time
	pending int           // builds made and not yet finished
	start   time.Time     // when the latest build started
	took    time.Duration // how long it took
}

// gap is how long after the latest build the next may start.
func (e *boardEntry) gap() time.Duration { return max(boardSpacing, 2*e.took) }

// boardBuilds coalesces reads of the board, per repository.
type boardBuilds struct {
	view func(repo string) board.View
	// clock and sleep pace builds. They are the process's own clock, not the
	// board's: pacing is about load, not about what the board shows.
	clock func() time.Time
	sleep func(time.Duration)
	log   *slog.Logger
	// slot lets one build run at a time across the server, so a hook waits
	// behind at most one, and gz, used only while holding it, compresses.
	slot chan struct{}
	gz   *gzip.Writer

	mu    sync.Mutex
	repos map[string]*boardEntry
}

func newBoardBuilds(view func(repo string) board.View, log *slog.Logger) *boardBuilds {
	gz, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed) // the level is valid
	return &boardBuilds{
		view:  view,
		clock: time.Now,
		sleep: time.Sleep,
		log:   log,
		slot:  make(chan struct{}, 1),
		gz:    gz,
		repos: map[string]*boardEntry{},
	}
}

// get returns a build of repo's board that read the board after get was
// called. Requests that arrive while a build waits for its turn share it.
func (c *boardBuilds) get(ctx context.Context, repo string) (*boardBuild, error) {
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

// run makes build b of repo once the build before it has finished, its gap
// has passed and no other repository's build is running. b stops taking
// requests just before it reads the board.
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
	c.slot <- struct{}{}
	defer func() { <-c.slot }()

	c.mu.Lock()
	e.next = nil // from here on, requests wait for the next build
	start := c.clock()
	e.start = start
	requests := b.requests
	c.mu.Unlock()

	c.build(repo, b)
	took := c.clock().Sub(start)
	c.mu.Lock()
	e.took = took
	c.mu.Unlock()
	c.log.Debug("board built", "repo", repo, "requests", requests, "took", took.Round(time.Microsecond),
		"bytes", len(b.json), "gzip_bytes", len(b.gz))
}

// build reads the board and encodes the answer. The caller holds c.slot.
func (c *boardBuilds) build(repo string, b *boardBuild) {
	v := c.view(repo)
	body, err := json.Marshal(v)
	if err != nil {
		b.err = err
		return
	}
	body = append(body, '\n')
	var buf bytes.Buffer
	buf.Grow(len(body) / 4)
	c.gz.Reset(&buf)
	if _, err := c.gz.Write(body); err != nil {
		b.err = err
		return
	}
	if err := c.gz.Close(); err != nil {
		b.err = err
		return
	}
	b.view, b.json, b.gz, b.etag, b.err = v, body, buf.Bytes(), boardETag(body), nil
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
