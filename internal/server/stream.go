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

// streamPiece is how much of a stream's output is written at once, within
// one deadline, so a client that keeps reading a large catch-up slowly still
// gets all of it.
const streamPiece = 64 << 10

// maxKeptBuffer bounds the buffer a stream keeps between writes; a larger one,
// grown for an activity larger than a piece, is let go.
const maxKeptBuffer = 2 * streamPiece

// streamRetryAfter is the Retry-After, in seconds, of a stream refused for
// being over its limit.
const streamRetryAfter = 30

// streamWriter writes one stream's server-sent events. It gathers what is
// ready and writes it a piece at a time, each within a deadline, then
// flushes it to the client.
type streamWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	repo string // the repository the stream is for; "" for all
	buf  []byte
	// unflushed says pieces were written since the last flush.
	unflushed bool
	// err is the first write that failed: the stream is over.
	err error
	// last is the id of the newest activity written: one replayed on
	// reconnection that also arrives live is not sent twice.
	last uint64
}

func newStreamWriter(w http.ResponseWriter, repo string) *streamWriter {
	return &streamWriter{w: w, rc: http.NewResponseController(w), repo: repo}
}

// add gathers b, writing a piece out once one is full.
func (o *streamWriter) add(b []byte) {
	o.buf = append(o.buf, b...)
	if len(o.buf) >= streamPiece {
		o.write(streamWriteTimeout)
	}
}

// raw adds text as it is: fields and comments that are not events.
func (o *streamWriter) raw(text string) { o.add([]byte(text)) }

// event adds an event without an id. data must be a single line.
func (o *streamWriter) event(name string, data []byte) { o.add(appendEvent(nil, name, 0, data)) }

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
		o.add(f.data)
	}
}

// goodbye tells the client, as the server ends the stream, to reconnect in
// retry milliseconds.
func (o *streamWriter) goodbye(retry int) {
	o.buf = strconv.AppendInt(append(o.buf[:0], "retry: "...), int64(retry), 10)
	o.buf = append(o.buf, "\n\n"...)
	_ = o.flushWithin(goodbyeTimeout)
}

// flush writes what has been gathered and flushes it to the client.
func (o *streamWriter) flush() error { return o.flushWithin(streamWriteTimeout) }

func (o *streamWriter) flushWithin(d time.Duration) error {
	o.write(d)
	if o.err != nil || !o.unflushed {
		return o.err
	}
	if o.err = o.deadline(d); o.err == nil {
		o.err = o.rc.Flush()
	}
	o.unflushed = false
	return o.err
}

// write writes what has been gathered as one piece, within d.
func (o *streamWriter) write(d time.Duration) {
	if len(o.buf) > 0 && o.err == nil {
		if o.err = o.deadline(d); o.err == nil {
			_, o.err = o.w.Write(o.buf)
			o.unflushed = true
		}
	}
	o.buf = o.buf[:0]
	if cap(o.buf) > maxKeptBuffer {
		o.buf = nil
	}
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
