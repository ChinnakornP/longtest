package realtime

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHubDeliversToEveryWatcherOfARun(t *testing.T) {
	hub := NewHub()
	runID, other := uuid.New(), uuid.New()

	first := hub.Subscribe(runID)
	defer first.Close()
	second := hub.Subscribe(runID)
	defer second.Close()
	elsewhere := hub.Subscribe(other)
	defer elsewhere.Close()

	hub.Publish(runID, Message{Seq: 1, Frame: []byte(`{"seq":1}`)})

	for i, sub := range []*Subscription{first, second} {
		select {
		case msg := <-sub.C():
			if msg.Seq != 1 {
				t.Fatalf("subscriber %d got seq %d, want 1", i, msg.Seq)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}

	// Subscriptions are keyed by run, so a watcher of another run sees nothing.
	select {
	case msg := <-elsewhere.C():
		t.Fatalf("a watcher of another run received %s", msg.Frame)
	case <-time.After(50 * time.Millisecond):
	}
}

// A subscriber that stops reading is dropped rather than buffered without
// bound. A stream with a silent hole in it is worse than a disconnect the
// client knows how to recover from with ?since.
func TestHubDropsASubscriberThatFallsBehind(t *testing.T) {
	hub := NewHub()
	runID := uuid.New()

	sub := hub.Subscribe(runID)
	defer sub.Close()

	for i := range defaultBuffer + 10 {
		hub.Publish(runID, Message{Seq: int64(i), Frame: []byte(`{}`)})
	}

	select {
	case <-sub.Lagged():
	case <-time.After(time.Second):
		t.Fatal("a subscriber that never read was not marked lagged")
	}

	// Nothing further is delivered once the stream has a hole: a later frame
	// would look contiguous to a client that never learned it missed one.
	drain := len(sub.C())
	hub.Publish(runID, Message{Seq: 9999, Frame: []byte(`{}`)})
	if got := len(sub.C()); got != drain {
		t.Fatalf("a lagged subscriber still received frames (%d -> %d)", drain, got)
	}
}

// Close unregisters and is safe to call twice, which is what lets a handler
// both defer it and call it on an error path.
func TestHubCloseIsIdempotent(t *testing.T) {
	hub := NewHub()
	runID := uuid.New()

	sub := hub.Subscribe(runID)
	if got := hub.Subscribers(runID); got != 1 {
		t.Fatalf("got %d subscribers, want 1", got)
	}

	sub.Close()
	sub.Close()

	if got := hub.Subscribers(runID); got != 0 {
		t.Fatalf("got %d subscribers after close, want 0", got)
	}
	// Publishing to a run nobody watches is a no-op, not a panic.
	hub.Publish(runID, Message{Seq: 1, Frame: []byte(`{}`)})
}

// Publish must not send on a closed channel when a subscriber closes
// concurrently. The assertion is the race detector and the absence of a panic,
// so there is nothing for the test to check by hand.
func TestHubPublishRacesWithClose(*testing.T) {
	hub := NewHub()
	runID := uuid.New()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			hub.Publish(runID, Message{Seq: 1, Frame: []byte(`{}`)})
		}
	}()

	for range 200 {
		sub := hub.Subscribe(runID)
		sub.Close()
	}
	<-done
}
