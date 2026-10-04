package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// streamLinger is how long a stream waits after each write before it takes
// more events. Under a burst each stream then wakes up and writes ten times a
// second, however many activities there are, rather than once per activity;
// the dashboard reloads its board no faster than that anyway. A variable for
// tests.
var streamLinger = 100 * time.Millisecond

// streamWriteTimeout bounds how long a piece of a stream's write may wait on
// its client. The server has no WriteTimeout, which would end every stream,
// so without this a dashboard that stops reading (a laptop put to sleep, a
// proxy that stalls) would hold its handler and buffers in a write until TCP
// gave up, which takes about 15 minutes. A variable for tests.
var streamWriteTimeout = 15 * time.Second

// goodbyeTimeout bounds the last write to a stream the server ends, which
// only says when to reconnect: a client that has stopped reading does not
// hold up a shutdown.
const goodbyeTimeout = time.Second

// streamPiece is how much of a write one deadline covers, so a client that
// keeps reading a large catch-up slowly still gets all of it.
const streamPiece = 64 << 10

// streamRetryAfter is the Retry-After, in seconds, of a stream refused for
// being over its limit.
const streamRetryAfter = 30

// maxKeptBuffer bounds the buffer a stream keeps between writes; a larger one,
// grown for a burst, is let go.
const maxKeptBuffer = 64 << 10

// streamWriter writes one stream's server-sent events: it gathers what is
// ready, then writes it at once and flushes it to the client.
type streamWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	repo string // the repository the stream is for; "" for all
	buf  []byte
	// last is the id of the newest activity written: one replayed on
	// reconnection that also arrives live is not sent twice.
	last uint64
}

func newStreamWriter(w http.ResponseWriter, repo string) *streamWriter {
	return &streamWriter{w: w, rc: http.NewResponseController(w), repo: repo}
}

// raw adds text as it is: fields and comments that are not events.
func (o *streamWriter) raw(text string) { o.buf = append(o.buf, text...) }

// event adds an event without an id. data must be a single line.
func (o *streamWriter) event(name string, data []byte) { o.buf = appendEvent(o.buf, name, 0, data) }

// frames adds the frames the stream carries and has not sent yet.
func (o *streamWriter) frames(batch []frame) {
	for _, f := range batch {
		if !carries(o.repo, f) {
			continue
		}
		if f.seq != 0 {
			if f.seq <= o.last {
				continue
			}
			o.last = f.seq
		}
		o.buf = append(o.buf, f.data...)
	}
}

// goodbye tells the client, as the server ends the stream, to reconnect in
// retry milliseconds.
func (o *streamWriter) goodbye(retry int) {
	o.buf = strconv.AppendInt(append(o.buf[:0], "retry: "...), int64(retry), 10)
	o.buf = append(o.buf, "\n\n"...)
	_ = o.flushWithin(goodbyeTimeout)
}

// flush writes what has been added and flushes it to the client, each piece
// within streamWriteTimeout.
func (o *streamWriter) flush() error { return o.flushWithin(streamWriteTimeout) }

func (o *streamWriter) flushWithin(d time.Duration) error {
	if len(o.buf) == 0 {
		return nil
	}
	for b := o.buf; len(b) > 0; {
		k := min(len(b), streamPiece)
		if err := o.deadline(d); err != nil {
			return err
		}
		if _, err := o.w.Write(b[:k]); err != nil {
			return err
		}
		b = b[k:]
	}
	o.buf = o.buf[:0]
	if cap(o.buf) > maxKeptBuffer {
		o.buf = nil
	}
	if err := o.deadline(d); err != nil {
		return err
	}
	return o.rc.Flush()
}

// deadline gives the next write d. A writer that takes no deadline, such as
// a test's recorder, writes without one.
func (o *streamWriter) deadline(d time.Duration) error {
	err := o.rc.SetWriteDeadline(time.Now().Add(d))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
