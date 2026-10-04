package server

import (
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"strconv"
	"sync"

	"github.com/patakil/intagent/internal/board"
)

// streamRing is how many publishes the hub keeps for streams that have not
// taken them yet, and streamRingBytes how many bytes of them. A stream takes
// everything new at most every streamLinger plus the time its write takes,
// so it keeps up with several thousand publishes a second; one that falls
// further behind is ended, and its dashboard reconnects and catches up from
// the last event it saw. The bytes bound what the ring holds when activities
// are large: 1024 of 200 long paths each would be 200 MB.
const (
	streamRing      = 1024
	streamRingBytes = 16 << 20
)

// StreamLimits caps the dashboard streams open at once. A stream counts until
// its handler returns, so one stuck writing to a client that stopped reading
// still holds its place. A field left at zero takes its default, and a
// negative one allows no streams.
type StreamLimits struct {
	// PerMember caps one member's streams, one per open dashboard. Default 20.
	PerMember int
	// Anonymous caps the streams opened without a token, on a board anyone
	// may read. Default 100.
	Anonymous int
	// Total caps every stream. Default 500.
	Total int
}

func (l StreamLimits) withDefaults() StreamLimits {
	l.PerMember = limitOr(l.PerMember, 20)
	l.Anonymous = limitOr(l.Anonymous, 100)
	l.Total = limitOr(l.Total, 500)
	return l
}

// limitOr is the cap a StreamLimits field n sets: def for zero, none for
// less.
func limitOr(n, def int) int {
	if n == 0 {
		return def
	}
	return max(n, 0)
}

// hub fans activities out to dashboard streams. Publishing costs the same
// however many streams are open: a publish goes into one ring that every
// stream reads at its own pace, and wakes the streams waiting for it.
type hub struct {
	limits StreamLimits
	mu     sync.Mutex
	ring   [streamRing][]frame
	first  uint64        // the oldest publish the ring keeps, at ring[first%streamRing]
	next   uint64        // publishes so far; the newest is ring[(next-1)%streamRing]
	bytes  int           // the bytes of the publishes the ring keeps
	wake   chan struct{} // closed at the next publish
	subs   map[*subscriber]struct{}
	open   map[string]int // streams whose handler has not returned, by member ("" for anonymous)
	total  int            // and all of them
	conns  uint64         // streams admitted so far
}

// frame is one server-sent event, encoded once for every stream that carries
// it. Frames and their bytes are shared between streams and never changed.
type frame struct {
	seq  uint64 // the event's id, which a stream does not send twice; 0 for none
	repo string // the repository whose streams carry it, besides those for all
	all  bool   // carried by every stream, whatever its repository
	data []byte // the whole event, up to and including its blank line
}

// appendEvent appends one server-sent event to dst. data must be a single
// line, as JSON from encoding/json is.
func appendEvent(dst []byte, event string, id uint64, data []byte) []byte {
	if id > 0 {
		dst = append(dst, "id: "...)
		dst = strconv.AppendUint(dst, id, 10)
		dst = append(dst, '\n')
	}
	dst = append(dst, "event: "...)
	dst = append(dst, event...)
	dst = append(dst, "\ndata: "...)
	dst = append(dst, data...)
	return append(dst, "\n\n"...)
}

// activityFrame encodes an activity as a stream sends it.
func activityFrame(a board.Activity) (frame, bool) {
	data, err := json.Marshal(a)
	if err != nil {
		return frame{}, false
	}
	return frame{seq: a.Seq, repo: a.Repo, data: appendEvent(make([]byte, 0, len(data)+48), "activity", a.Seq, data)}, true
}

type subscriber struct {
	repo string
	// who authorised the stream: a member and their token's hash, or nobody
	// on a board open to all.
	who memberHash
	// retry is the reconnection delay, in milliseconds, the stream gives its
	// client when the server ends it.
	retry int
	// pos is the first publish the stream takes, and wake is closed at it.
	pos  uint64
	wake <-chan struct{}
	// done is closed when the hub ends the stream.
	done chan struct{}
}

// carries reports whether a stream for repo ("" for all) sends f.
func carries(repo string, f frame) bool { return repo == "" || f.all || f.repo == repo }

func newHub(limits StreamLimits) *hub {
	return &hub{limits: limits.withDefaults(), wake: make(chan struct{}), subs: map[*subscriber]struct{}{}, open: map[string]int{}}
}

// subscribe adds a stream, which takes what is published from now on, unless
// the member who opened it (nobody, without a token) or the server already
// has as many as allowed. Every stream it adds must be unsubscribed.
func (h *hub) subscribe(repo string, who memberHash) (*subscriber, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	limit := h.limits.PerMember
	if who.name == "" {
		limit = h.limits.Anonymous
	}
	if h.total >= h.limits.Total || h.open[who.name] >= limit {
		return nil, false
	}
	h.total++
	h.open[who.name]++
	h.conns++
	s := &subscriber{repo: repo, who: who, retry: reconnectDelay(who.name, h.conns), pos: h.next, wake: h.wake, done: make(chan struct{})}
	h.subs[s] = struct{}{}
	return s, true
}

// A stream the server ends tells its client to reconnect after 3 to 8
// seconds, so streams ended together do not all come back, and reload the
// board, at once. A stream starts out with 3 s, for drops the server did not
// choose.
const (
	retryBase   = 3000
	retrySpread = 5000
)

// reconnectDelay is the delay, in milliseconds, for the conn-th stream: the
// same for the same member and connection, spread evenly over all of them.
func reconnectDelay(member string, conn uint64) int {
	f := fnv.New32a()
	_, _ = f.Write([]byte(member))
	_, _ = f.Write(binary.BigEndian.AppendUint64([]byte{0}, conn))
	return retryBase + int(f.Sum32()%retrySpread)
}

// closeUnless ends the streams whose authority keep no longer accepts; their
// clients reconnect and authenticate again.
func (h *hub) closeUnless(keep func(memberHash) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !keep(s.who) {
			h.end(s)
		}
	}
}

// end ends a stream the hub holds; h.mu is held.
func (h *hub) end(s *subscriber) {
	close(s.done)
	delete(h.subs, s)
}

// unsubscribe removes a stream whose handler is returning, and frees its place.
// The last one lets go of what the ring holds, which no stream will read: a
// stream opened later starts after it.
func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
	h.total--
	if h.open[s.who.name]--; h.open[s.who.name] <= 0 {
		delete(h.open, s.who.name)
	}
	if h.total == 0 {
		clear(h.ring[:])
		h.first, h.bytes = h.next, 0
	}
}

// publish encodes activities once, outside the hub's lock, for every stream.
func (h *hub) publish(acts []board.Activity) {
	h.mu.Lock()
	idle := len(h.subs) == 0
	h.mu.Unlock()
	if idle {
		return // a stream opened later starts after these
	}
	frames := make([]frame, 0, len(acts))
	for _, a := range acts {
		if f, ok := activityFrame(a); ok {
			frames = append(frames, f)
		}
	}
	h.send(frames)
}

// send makes frames one publish, which every stream takes in order: a sweep
// that announces a thousand stalls is one place in the ring, not a thousand.
// It never blocks, and costs the same however many streams are open. A frame
// marked all reaches every stream, whatever its repository: the way for an
// event that is not an activity, such as one about the server itself.
func (h *hub) send(frames []frame) {
	if len(frames) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return // a stream opened later starts after this
	}
	if h.next-h.first == streamRing {
		h.dropOldest()
	}
	h.ring[h.next%streamRing] = frames
	h.bytes += framesSize(frames)
	h.next++
	for h.bytes > streamRingBytes && h.next-h.first > 1 {
		h.dropOldest()
	}
	close(h.wake)
	h.wake = make(chan struct{})
}

// dropOldest lets go of the oldest publish the ring keeps; h.mu is held.
func (h *hub) dropOldest() {
	slot := &h.ring[h.first%streamRing]
	h.bytes -= framesSize(*slot)
	*slot = nil
	h.first++
}

// framesSize is the bytes of the events in frames.
func framesSize(frames []frame) (n int) {
	for _, f := range frames {
		n += len(f.data)
	}
	return n
}

// since appends to dst the publishes from pos on, and returns them with the
// position after them and a channel closed at the next publish. ok is false
// when the ring no longer holds the publish at pos.
func (h *hub) since(pos uint64, dst [][]frame) (_ [][]frame, next uint64, wake <-chan struct{}, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pos < h.first {
		return dst, h.next, h.wake, false
	}
	for ; pos < h.next; pos++ {
		dst = append(dst, h.ring[pos%streamRing])
	}
	return dst, h.next, h.wake, true
}
