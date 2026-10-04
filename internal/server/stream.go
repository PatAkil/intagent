package server

import (
	"net/http"
	"time"
)

// streamLinger is how long a stream waits after each write before it takes
// more events. Under a burst each stream then wakes up and writes ten times a
// second, however many activities there are, rather than once per activity;
// the dashboard reloads its board no faster than that anyway. A variable for
// tests.
var streamLinger = 100 * time.Millisecond

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

// flush writes what has been added and flushes it to the client.
func (o *streamWriter) flush() error {
	if len(o.buf) == 0 {
		return nil
	}
	_, err := o.w.Write(o.buf)
	o.buf = o.buf[:0]
	if cap(o.buf) > maxKeptBuffer {
		o.buf = nil
	}
	if err != nil {
		return err
	}
	return o.rc.Flush()
}
