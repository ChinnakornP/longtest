package realtime

import (
	"sync"

	"github.com/google/uuid"
)

// Message is one frame on a browser's run stream.
type Message struct {
	// Seq is the run-event sequence number this frame carries. It is
	// NoSequence for a frame that is not part of the numbered stream, such as
	// a status change, which therefore is never suppressed by the resume
	// filter.
	Seq int64
	// Frame is the already-marshalled JSON written to the socket verbatim.
	Frame []byte
}

// NoSequence marks a message that is not part of a run's numbered event
// stream.
const NoSequence int64 = -1

// defaultBuffer is how far one browser may fall behind before it is dropped.
//
// A run emits events at human-readable rates, so a subscriber this far behind
// is not slow, it is gone (a suspended laptop, a dead TCP connection the OS
// has not noticed yet). Dropping it and letting it resume from ?since is
// bounded work; buffering for it is not.
const defaultBuffer = 256

// Hub fans a run's events out to every browser watching it.
//
// Subscriptions are keyed by run id rather than by organization: a subscriber
// only ever exists because a handler already checked that its caller may read
// that run, so the hub itself does no authorization and cannot be asked to.
type Hub struct {
	mu     sync.RWMutex
	byRun  map[uuid.UUID]map[*Subscription]struct{}
	buffer int
}

// NewHub returns an empty hub. One is created per process at start-up.
func NewHub() *Hub {
	return &Hub{byRun: map[uuid.UUID]map[*Subscription]struct{}{}, buffer: defaultBuffer}
}

// Subscription is one browser's view of one run.
type Subscription struct {
	hub   *Hub
	runID uuid.UUID
	ch    chan Message
	// lagged is closed instead of ch when the subscriber could not keep up, so
	// the reader can tell "the run ended" from "you missed frames".
	lagged    chan struct{}
	closeOnce sync.Once
	// closed is "no further sends", set by Close and by the first send that
	// could not keep up. It is guarded by the hub's mutex, which is what makes
	// "close the channel" and "send on the channel" mutually exclusive: without
	// that, a Publish holding a stale snapshot of the subscriber set would send
	// on a channel Close had already closed.
	closed bool
}

// Subscribe registers for a run's stream.
//
// Subscribe before reading the backlog, not after: an event published between
// a backlog read and a subscribe would otherwise be lost, and the resume
// filter in the handler already discards the overlap the other way round.
func (h *Hub) Subscribe(runID uuid.UUID) *Subscription {
	sub := &Subscription{
		hub:    h,
		runID:  runID,
		ch:     make(chan Message, h.buffer),
		lagged: make(chan struct{}),
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	subs, ok := h.byRun[runID]
	if !ok {
		subs = map[*Subscription]struct{}{}
		h.byRun[runID] = subs
	}
	subs[sub] = struct{}{}
	return sub
}

// C is the stream of frames. It is closed when the subscription is closed.
func (s *Subscription) C() <-chan Message { return s.ch }

// Lagged is closed when this subscriber fell too far behind and its stream now
// has a hole. The reader must disconnect; the client resumes with ?since.
func (s *Subscription) Lagged() <-chan struct{} { return s.lagged }

// Close unsubscribes. It is safe to call twice, which is what lets a handler
// defer it and still close early on an error path.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		if subs, ok := s.hub.byRun[s.runID]; ok {
			delete(subs, s)
			if len(subs) == 0 {
				delete(s.hub.byRun, s.runID)
			}
		}
		s.closed = true
		close(s.ch)
	})
}

// Publish delivers msg to everyone watching runID.
//
// It never blocks: a subscriber whose buffer is full is marked lagged and
// receives nothing further, because a stream with a silent hole in it is worse
// than a disconnect the client knows how to recover from.
func (h *Hub) Publish(runID uuid.UUID, msg Message) {
	h.mu.RLock()
	subs := make([]*Subscription, 0, len(h.byRun[runID]))
	for sub := range h.byRun[runID] {
		subs = append(subs, sub)
	}
	h.mu.RUnlock()

	for _, sub := range subs {
		sub.send(msg)
	}
}

func (s *Subscription) send(msg Message) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- msg:
	default:
		s.closed = true
		close(s.lagged)
	}
}

// CloseAll ends every browser subscription, which makes each stream handler
// return. See Handler.Shutdown for why shutdown needs this.
func (h *Hub) CloseAll() {
	h.mu.RLock()
	subs := make([]*Subscription, 0, len(h.byRun))
	for _, byRun := range h.byRun {
		for sub := range byRun {
			subs = append(subs, sub)
		}
	}
	h.mu.RUnlock()

	for _, sub := range subs {
		sub.Close()
	}
}

// Subscribers reports how many browsers are watching a run. It exists for the
// tests and for a future metric; nothing in the request path branches on it.
func (h *Hub) Subscribers(runID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byRun[runID])
}
