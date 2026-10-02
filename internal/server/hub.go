package server

import (
	"sync"

	"github.com/patakil/intagent/internal/board"
)

// hub fans activities out to dashboard streams.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	repo string
	ch   chan board.Activity
	// dropped is closed when the subscriber fell too far behind.
	dropped chan struct{}
	once    sync.Once
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe(repo string) *subscriber {
	s := &subscriber{repo: repo, ch: make(chan board.Activity, 256), dropped: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// publish never blocks: a subscriber whose buffer is full is dropped, and its
// dashboard reconnects and catches up from the last event it saw.
func (h *hub) publish(acts []board.Activity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		for _, a := range acts {
			if s.repo != "" && a.Repo != s.repo {
				continue
			}
			select {
			case s.ch <- a:
			default:
				s.once.Do(func() { close(s.dropped) })
				delete(h.subs, s)
			}
		}
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
