package runtime

import (
	"testing"
	"time"
)

// The acceptance criterion is that a daemon reconnects within 30 seconds of
// the backend coming back, so no single wait may approach that.
func TestBackoffStaysInsideTheReconnectBudget(t *testing.T) {
	b := DefaultBackoff()
	for i := range 20 {
		wait := b.Next()
		if wait <= 0 {
			t.Fatalf("attempt %d waited %s", i, wait)
		}
		if wait > 15*time.Second {
			t.Fatalf("attempt %d waited %s, which breaks the 30s reconnect budget", i, wait)
		}
	}
}

func TestBackoffGrowsThenCaps(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 8 * time.Second, Factor: 2}

	// With full jitter every wait is bounded by the window, so growth is
	// asserted on the bound rather than on one sample.
	bounds := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, bound := range bounds {
		wait := b.Next()
		if wait > bound {
			t.Fatalf("attempt %d waited %s, past its %s window", i, wait, bound)
		}
		if wait < 500*time.Millisecond {
			t.Fatalf("attempt %d waited %s, below the floor", i, wait)
		}
	}
}

func TestBackoffJitters(t *testing.T) {
	seen := map[time.Duration]int{}
	for range 50 {
		b := Backoff{Min: time.Second, Max: 30 * time.Second, Factor: 2}
		b.Next()
		b.Next()
		seen[b.Next()]++
	}
	// Without jitter, every daemon in an organization reconnects in the same
	// millisecond after a backend restart.
	if len(seen) < 10 {
		t.Fatalf("only %d distinct waits across 50 schedules; the jitter is not working", len(seen))
	}
}

func TestBackoffReset(t *testing.T) {
	b := DefaultBackoff()
	for range 5 {
		b.Next()
	}
	if b.Attempt() != 5 {
		t.Fatalf("attempt = %d", b.Attempt())
	}
	b.Reset()
	if b.Attempt() != 0 {
		t.Fatalf("attempt after reset = %d", b.Attempt())
	}
	if wait := b.Next(); wait > time.Second {
		t.Fatalf("first wait after reset = %s, want the floor again", wait)
	}
}

func TestBackoffZeroValueIsUsable(t *testing.T) {
	var b Backoff
	if wait := b.Next(); wait <= 0 {
		t.Fatalf("zero-value backoff returned %s", wait)
	}
}
