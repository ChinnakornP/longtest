package runtime

import (
	"sync"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// outbox holds frames the daemon has produced but not yet handed to a live
// connection.
//
// It exists because a run does not stop when the connection does (ADR-002: the
// daemon must survive a backend restart without losing an in-flight run). A
// frame stays at the head until a write succeeds, so a connection that dies
// mid-write replays that frame on the next one — delivery is at-least-once and
// the server deduplicates on (runId, seq).
type outbox struct {
	mu    sync.Mutex
	items []qaschema.Envelope
	limit int

	// signal is closed and replaced whenever an item arrives, so a waiting
	// writer wakes without polling.
	signal chan struct{}

	dropped int
}

func newOutbox(limit int) *outbox {
	if limit <= 0 {
		limit = 2048
	}
	return &outbox{limit: limit, signal: make(chan struct{})}
}

// Push queues a frame. When the queue is full the least important frame is
// dropped rather than the newest: a debug event is expendable, a run.result is
// the only thing that tells the backend the run ended.
func (o *outbox) Push(env qaschema.Envelope) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if len(o.items) >= o.limit {
		if idx := o.evictable(); idx >= 0 {
			o.items = append(o.items[:idx], o.items[idx+1:]...)
			o.dropped++
		} else {
			// Everything queued is a result or an error: refuse the new frame
			// instead, so a backlog of terminal frames is never truncated.
			o.dropped++
			return
		}
	}
	o.items = append(o.items, env)
	o.wake()
}

// evictable finds the oldest frame the daemon can afford to lose: a debug or
// info run.event. It returns -1 when every queued frame matters.
func (o *outbox) evictable() int {
	for i, item := range o.items {
		if item.Type != qaschema.EnvelopeTypeRunEvent {
			continue
		}
		if level, ok := eventLevel(item); ok &&
			(level == qaschema.RunEventPayloadLevelDebug || level == qaschema.RunEventPayloadLevelInfo) {
			return i
		}
	}
	return -1
}

// Head returns the frame to send next without removing it.
func (o *outbox) Head() (qaschema.Envelope, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.items) == 0 {
		return qaschema.Envelope{}, false
	}
	return o.items[0], true
}

// Ack removes the frame a connection confirmed it wrote.
func (o *outbox) Ack(msgID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.items) > 0 && o.items[0].MsgID == msgID {
		o.items = o.items[1:]
	}
}

// Len is how many frames are waiting.
func (o *outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.items)
}

// Dropped is how many frames were discarded because the queue was full. It is
// reported in the log so a silent gap in a run's event stream is explainable.
func (o *outbox) Dropped() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dropped
}

// Wait returns a channel closed when a frame is available.
func (o *outbox) Wait() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.items) > 0 {
		ready := make(chan struct{})
		close(ready)
		return ready
	}
	return o.signal
}

func (o *outbox) wake() {
	close(o.signal)
	o.signal = make(chan struct{})
}

func eventLevel(env qaschema.Envelope) (qaschema.RunEventPayloadLevel, bool) {
	var payload qaschema.RunEventPayload
	if err := decodePayload(env.Payload, &payload); err != nil {
		return "", false
	}
	return payload.Level, true
}
