package board

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/glob"
)

const snapshotFormat = 1

type snapshot struct {
	Format   int               `json:"format"`
	Saved    time.Time         `json:"saved"`
	Seq      uint64            `json:"seq"`
	Claims   []*claim          `json:"claims"`
	Sessions []*session        `json:"sessions"`
	Recent   []Activity        `json:"recent"`
	Stats    map[string]*Stats `json:"stats,omitempty"`
	// Dropped is nil in a snapshot from a server that did not keep it.
	Dropped *droppedMarks `json:"dropped,omitempty"`
	// Mail is the notes waiting for members (mail.go), by repository and
	// member.
	Mail []*mailbox `json:"mail,omitempty"`
}

// droppedMarks are what the board's feed has let go of, as Board keeps them,
// so a replay after a restart can still tell a dashboard it missed nothing.
type droppedMarks struct {
	Repos map[string]uint64 `json:"repos,omitempty"`
	All   uint64            `json:"all,omitempty"`
	Floor uint64            `json:"floor,omitempty"`
}

// Snapshot is WriteSnapshot into memory.
func (b *Board) Snapshot(now time.Time) ([]byte, uint64, error) {
	var buf bytes.Buffer
	version, err := b.WriteSnapshot(&buf, now)
	return buf.Bytes(), version, err
}

// snapshotBuffer is how much of a snapshot WriteSnapshot holds before it
// writes it out.
const snapshotBuffer = 256 << 10

// WriteSnapshot serialises the board to w for persistence, and returns the
// board's version it holds. The state is copied under the lock and encoded
// outside it, so a large board does not hold up hooks while it is written.
// The copy shares each claim's footprint and alerts with the board, so the
// time the lock is held grows with the claims and sessions, not with the
// files they changed. The claims, in order of ID, and the sessions, in order
// of key, are encoded one at a time, so the encoding takes as much memory
// as the largest of them, not as the board: the bytes are those json.Marshal
// gives the snapshot.
func (b *Board) WriteSnapshot(w io.Writer, now time.Time) (uint64, error) {
	s, version := b.snapshotCopy(now)
	slices.SortFunc(s.Claims, func(x, y *claim) int { return strings.Compare(x.ID, y.ID) })
	slices.SortFunc(s.Sessions, func(x, y *session) int { return strings.Compare(x.Key, y.Key) })
	bw := bufio.NewWriterSize(w, snapshotBuffer)
	sw := &snapshotWriter{w: bw, enc: json.NewEncoder(oneLine{bw})}
	// The fields of snapshot, in its order, as json.Marshal writes them.
	sw.raw(`{"format":`)
	sw.value(s.Format)
	sw.raw(`,"saved":`)
	sw.value(s.Saved)
	sw.raw(`,"seq":`)
	sw.value(s.Seq)
	sw.raw(`,"claims":`)
	writeArray(sw, s.Claims)
	sw.raw(`,"sessions":`)
	writeArray(sw, s.Sessions)
	sw.raw(`,"recent":`)
	sw.value(s.Recent)
	if len(s.Stats) > 0 {
		sw.raw(`,"stats":`)
		sw.value(s.Stats)
	}
	if s.Dropped != nil {
		sw.raw(`,"dropped":`)
		sw.value(s.Dropped)
	}
	if len(s.Mail) > 0 {
		sw.raw(`,"mail":`)
		sw.value(s.Mail)
	}
	sw.raw("}")
	if sw.err != nil {
		return version, sw.err
	}
	return version, bw.Flush()
}

// snapshotWriter writes a snapshot piece by piece, and keeps the first
// error.
type snapshotWriter struct {
	w   *bufio.Writer
	enc *json.Encoder
	err error
}

func (sw *snapshotWriter) raw(s string) {
	if sw.err == nil {
		_, sw.err = sw.w.WriteString(s)
	}
}

func (sw *snapshotWriter) value(v any) {
	if sw.err == nil {
		sw.err = sw.enc.Encode(v)
	}
}

// writeArray writes a slice an element at a time, as json.Marshal writes it
// whole.
func writeArray[T any](sw *snapshotWriter, list []T) {
	if list == nil {
		sw.raw("null")
		return
	}
	sw.raw("[")
	for i, v := range list {
		if i > 0 {
			sw.raw(",")
		}
		sw.value(v)
	}
	sw.raw("]")
}

// oneLine drops the newline json.Encoder ends each value with. Compact JSON
// holds no other: a newline in a string is escaped.
type oneLine struct{ w io.Writer }

func (o oneLine) Write(p []byte) (int, error) {
	n := len(p)
	if n > 0 && p[n-1] == '\n' {
		p = p[:n-1]
	}
	if _, err := o.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// snapshotCopy copies what a snapshot holds, under the lock.
func (b *Board) snapshotCopy(now time.Time) (snapshot, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := snapshot{Format: snapshotFormat, Saved: now, Seq: b.seq, Recent: slices.Clone(b.recent), Stats: map[string]*Stats{},
		Dropped: &droppedMarks{Repos: maps.Clone(b.dropped), All: b.droppedAll, Floor: b.droppedFloor}}
	for repo, st := range b.stats {
		c := *st
		s.Stats[repo] = &c
	}
	if len(b.claims) > 0 { // an empty board's are null, as they always were
		s.Claims = make([]*claim, 0, len(b.claims))
	}
	for _, c := range b.claims {
		s.Claims = append(s.Claims, c.shareFootprint())
	}
	if len(b.sessions) > 0 {
		s.Sessions = make([]*session, 0, len(b.sessions))
	}
	for _, x := range b.sessions {
		s.Sessions = append(s.Sessions, x.clone())
	}
	if len(b.mail) > 0 {
		s.Mail = b.mailCopy()
	}
	return s, b.version
}

// shareFootprint copies a claim for a snapshot, deeply enough to be read
// while the original changes, except for what the claim no longer changes
// in place: its footprint and the touches in it (putTouch), the alerts it
// heard (alert) and its inbox items' paths. The copy reads the claim's own.
// Both are marked shared, so a copy put on a board copies them before it
// changes them too.
func (c *claim) shareFootprint() *claim {
	c.fpShared, c.alertedShared = true, true
	d := *c
	d.Intents = slices.Clone(c.Intents)
	d.Told = maps.Clone(c.Told) // alertOthers counts in it
	d.Dirs = maps.Clone(c.Dirs) // a few hundred at most
	d.Inbox = slices.Clone(c.Inbox)
	for i := range d.Inbox {
		d.Inbox[i].DeliveredTo = maps.Clone(d.Inbox[i].DeliveredTo) // deliverInbox adds to it
	}
	// The copy is not on the board: it has no indexes.
	d.areaAt, d.sortedPaths, d.removed = nil, nil, false
	return &d
}

// alert notes that the claim heard the alert k, and reports whether it had
// not already. When a snapshot shares the claim's alerts, it changes a copy,
// which the claim keeps.
func (c *claim) alert(k string) bool {
	if c.Alerted[k] {
		return false
	}
	switch {
	case c.Alerted == nil:
		c.Alerted = map[string]bool{}
	case c.alertedShared:
		c.Alerted = maps.Clone(c.Alerted)
	}
	c.alertedShared = false
	c.Alerted[k] = true
	return true
}

// clone copies a session deeply enough to be read while the original changes.
func (s *session) clone() *session {
	d := *s
	d.Acked = maps.Clone(s.Acked)
	d.Calls = maps.Clone(s.Calls)
	d.Also = maps.Clone(s.Also)
	d.refused = slices.Clone(s.refused)
	return &d
}

// Restore replaces the board's contents with a snapshot read from r at now,
// and rebuilds what is derived from them. It drops the intents whose
// patterns the board no longer accepts (glob.CleanPattern), and logs each.
//
// The time between the snapshot and now is not counted as silence: a
// server that was down heard from nobody, and its sessions are as live, or
// as stalled, as they were when it was saved. So the agents still at work
// are not announced as stalled by the first sweep, and keep their
// reservations; one that truly died before the outage is announced stalled
// stall_after later than it would have been.
func (b *Board) Restore(r io.Reader, now time.Time) error {
	return b.RestoreAfter(r, now, time.Time{})
}

// RestoreAfter is Restore for a snapshot whose server is known to have run
// on after it saved it, until stopped. A board that does not change is not
// saved again, so its snapshot may be hours older than the stop; the time
// the server ran on, hearing nothing from a session, is that session's
// silence, and only the time since stopped is the server's downtime. A zero
// stopped tells nothing the snapshot does not.
func (b *Board) RestoreAfter(r io.Reader, now, stopped time.Time) error {
	s, err := readSnapshot(r)
	if err != nil {
		return err
	}
	s.shareSessionKeys()
	s.creditDowntime(now, stopped)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claims = map[string]*claim{}
	b.byKey = map[string]string{}
	b.sessions = map[string]*session{}
	b.byRepo = map[string]*repoIndex{}
	b.claimSessions = map[string]map[string]*session{}
	b.alsoSessions = map[string]map[string]*session{}
	b.memberBytes = map[string]int{}
	b.unpruned = map[string]bool{}
	for _, c := range s.Claims {
		if c == nil || c.ID == "" {
			continue
		}
		c.Intents = b.validIntents(c)
		b.addClaim(c)
		// A snapshot from an older server keeps alerts of claims it removed.
		b.unpruned[c.Repo] = true
	}
	for _, x := range s.Sessions {
		if x == nil || x.Key == "" {
			continue
		}
		b.attachSession(x, x.ClaimID)
	}
	b.seq = s.Seq
	b.recent = s.Recent
	b.dropped = map[string]uint64{}
	if d := s.Dropped; d != nil {
		maps.Copy(b.dropped, d.Repos)
		b.droppedAll, b.droppedFloor = d.All, d.Floor
	} else {
		// What the feed let go of before this snapshot is not known by
		// repository: every activity before the feed's first, in any.
		b.droppedFloor = b.seq
		if len(b.recent) > 0 {
			b.droppedFloor = b.recent[0].Seq - 1
		}
		b.droppedAll = b.droppedFloor
	}
	if over := len(b.recent) - b.cfg.KeepActivities; over > 0 {
		b.letGo(b.recent[:over]) // the feed is kept shorter now
		b.recent = b.recent[over:]
	}
	b.stats = s.Stats
	if b.stats == nil {
		b.stats = map[string]*Stats{}
	}
	b.statsAt = map[string]time.Time{}
	b.mail = map[string]*mailbox{}
	for _, m := range s.Mail {
		if m != nil && m.Repo != "" && m.Member != "" && len(m.Items) > 0 {
			b.mail[mailKey(m.Repo, m.Member)] = m
		}
	}
	// What happened before the snapshot is in it, or gone with the process.
	b.notes = map[string][]time.Time{}
	b.pending = nil
	return nil
}

// trimRestored bounds what a claim read from a snapshot remembers of its
// teammates to what alertOthers keeps now. An older server remembered an
// alert for every file of every teammate's claim, and listed every file in
// an alert's item: it keeps, of each teammate's claim, the alerts of the
// first maxToldPaths files by name, counts them in Told, as alertOthers
// does, and lists the first maxToldPaths files of each alert.
func (c *claim) trimRestored() {
	if c == nil {
		return
	}
	for i := range c.Inbox {
		if it := &c.Inbox[i]; it.Kind == "overlap" && len(it.Paths) > maxToldPaths {
			it.Paths = slices.Clone(it.Paths[:maxToldPaths])
		}
	}
	told, over := map[string]int{}, false
	for k := range c.Alerted {
		if id, ok := toldClaim(k); ok {
			told[id]++
			over = over || told[id] > maxToldPaths
		}
	}
	if over {
		keep := map[string]bool{}
		clear(told)
		for _, k := range slices.Sorted(maps.Keys(c.Alerted)) {
			if id, ok := toldClaim(k); ok {
				if told[id] == maxToldPaths {
					continue
				}
				told[id]++
			}
			keep[k] = true
		}
		c.Alerted = keep
	}
	c.Told = nil
	if len(told) > 0 {
		c.Told = told
	}
}

// toldClaim is the teammate's claim an alert of a file it changed names,
// alertOthers' touch|<claim>|<path>, which Told counts.
func toldClaim(k string) (string, bool) {
	if !strings.HasPrefix(k, "touch|") {
		return "", false
	}
	return alertedClaim(k), true
}

// ErrCorruptSnapshot reports a snapshot that is not one: cut short, or
// damaged by something other than the server, which writes it whole. A
// snapshot of a newer format, or one that could not be read, is not
// reported as corrupt.
var ErrCorruptSnapshot = errors.New("the snapshot is damaged")

// readSnapshot decodes a snapshot as WriteSnapshot writes it, or as
// json.Marshal did, a claim, a session and an activity at a time: what it
// holds of r at once is one of them, not the whole file, beside what it
// decoded.
func readSnapshot(r io.Reader) (snapshot, error) {
	var s snapshot
	src := &sourceReader{r: r}
	dec := json.NewDecoder(src)
	err := readObject(dec, func(key string) error {
		switch {
		case strings.EqualFold(key, "format"):
			if err := dec.Decode(&s.Format); err != nil {
				return err
			}
			if s.Format > snapshotFormat {
				// Before its claims, which a newer server may write otherwise.
				return unsupportedFormat(s.Format)
			}
			return nil
		case strings.EqualFold(key, "claims"):
			// Each trimmed as it is read, so that what an older server
			// kept is not all held at once.
			return readArray(dec, &s.Claims, (*claim).trimRestored)
		case strings.EqualFold(key, "sessions"):
			return readArray(dec, &s.Sessions, nil)
		case strings.EqualFold(key, "saved"):
			return dec.Decode(&s.Saved)
		case strings.EqualFold(key, "seq"):
			return dec.Decode(&s.Seq)
		case strings.EqualFold(key, "recent"):
			return readArray(dec, &s.Recent, nil)
		case strings.EqualFold(key, "stats"):
			return dec.Decode(&s.Stats)
		case strings.EqualFold(key, "dropped"):
			return dec.Decode(&s.Dropped)
		case strings.EqualFold(key, "mail"):
			return readArray(dec, &s.Mail, nil)
		}
		var skip json.RawMessage // a field from a newer server
		return dec.Decode(&skip)
	})
	if err == nil {
		// One object, as json.Unmarshal takes: anything after it is damage.
		if _, end := dec.Token(); !errors.Is(end, io.EOF) {
			err = errors.New("data after the snapshot")
		}
	}
	if err == nil && s.Format != snapshotFormat {
		err = fmt.Errorf("snapshot format %d", s.Format) // none, or one no server wrote
	}
	var unsupported unsupportedFormat
	switch {
	case err == nil:
		return s, nil
	case errors.As(err, &unsupported):
		return s, fmt.Errorf("read snapshot: %w", err)
	case src.err != nil:
		return s, fmt.Errorf("read snapshot: %w", src.err)
	}
	return s, fmt.Errorf("%w: %w", ErrCorruptSnapshot, err)
}

// sourceReader keeps the error, other than the end, that reading r gave.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// unsupportedFormat is the format of a snapshot this board cannot read.
type unsupportedFormat int

func (e unsupportedFormat) Error() string {
	return fmt.Sprintf("snapshot format %d is not supported (want %d)", int(e), snapshotFormat)
}

// readObject reads a JSON object from dec, and has field read the value of
// each of its fields.
func readObject(dec *json.Decoder, field func(key string) error) error {
	if err := readDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string) // within an object, the decoder yields only keys here
		if err := field(key); err != nil {
			return err
		}
	}
	return readDelim(dec, '}')
}

// readArray reads a JSON array, or null, into list an element at a time,
// passing each to each, if it is not nil, as it is read.
func readArray[T any](dec *json.Decoder, list *[]T, each func(T)) error {
	tok, err := dec.Token()
	if err != nil || tok == nil {
		*list = nil
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("found %v where an array belongs", tok)
	}
	*list = []T{}
	for dec.More() {
		var v T
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if each != nil {
			each(v)
		}
		*list = append(*list, v)
	}
	return readDelim(dec, ']')
}

func readDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("found %v where %v belongs", tok, want)
	}
	return nil
}

// creditDowntime moves the times sessions were last heard from, and went
// into tools, on by the time the server was down: since it stopped, or
// since the snapshot was saved if that is later, but never past now. When
// neither is known, or the clock went back, it moves nothing.
func (s *snapshot) creditDowntime(now, stopped time.Time) {
	since := s.Saved
	if stopped.After(since) {
		since = stopped
	}
	down := now.Sub(since)
	if since.IsZero() || down <= 0 {
		return
	}
	later := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		if t = t.Add(down); t.After(now) {
			return now
		}
		return t
	}
	for _, x := range s.Sessions {
		if x != nil {
			x.LastSeen, x.ToolSince = later(x.LastSeen), later(x.ToolSince)
		}
	}
}

// shareSessionKeys leaves the touches found by git without the session
// that found them, which nothing reads, as reconcile now leaves them, and
// has every other touch name its session with one string per session
// rather than one per touch. Snapshots from older servers held both. It
// drops a file whose touch is null, which no server writes.
func (s *snapshot) shareSessionKeys() {
	keys := make(map[string]string, len(s.Sessions))
	for _, x := range s.Sessions {
		if x != nil {
			keys[x.Key] = x.Key
		}
	}
	for _, c := range s.Claims {
		if c == nil {
			continue
		}
		for p, t := range c.Footprint {
			switch {
			case t == nil:
				delete(c.Footprint, p) // a file with no change to restore
			case t.FromGit:
				t.Session = "" // decoded just now: no other reader has it
			default:
				k, ok := keys[t.Session]
				if !ok {
					k = t.Session
					keys[k] = k
				}
				t.Session = k
			}
		}
	}
}

// validIntents is c's intents whose patterns the board accepts, cleaned. A
// snapshot from an older server can hold patterns it no longer accepts, such
// as ones too costly to match.
func (b *Board) validIntents(c *claim) []Intent {
	keep := c.Intents[:0]
	for _, in := range c.Intents {
		p, err := glob.CleanPattern(in.Pattern)
		if err != nil {
			b.log.Warn("dropping an intent from the snapshot", "claim", c.ID, "member", c.Member, "err", err)
			continue
		}
		if slices.ContainsFunc(keep, func(k Intent) bool { return k.Pattern == p }) {
			continue
		}
		in.Pattern = p
		keep = append(keep, in)
	}
	return keep
}
