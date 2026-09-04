package runtime

import (
	"math/rand/v2"
	"time"
)

// Backoff is the reconnect schedule: exponential growth with full jitter,
// capped.
//
// Jitter is not decoration. Every daemon in an organization loses its
// connection at the same instant when the backend restarts, and an unjittered
// schedule would bring all of them back in the same millisecond, repeatedly.
// The cap is what keeps the acceptance criterion true: a daemon must be back
// within 30 seconds of the backend returning, so the longest wait between two
// attempts stays below that.
type Backoff struct {
	Min    time.Duration
	Max    time.Duration
	Factor float64

	attempt int
}

// DefaultBackoff reconnects quickly at first and settles at 15 seconds, which
// bounds worst-case reconnection at under 30 seconds.
func DefaultBackoff() Backoff {
	return Backoff{Min: 500 * time.Millisecond, Max: 15 * time.Second, Factor: 2}
}

// Next returns the next wait and advances the schedule.
func (b *Backoff) Next() time.Duration {
	minWait, maxWait := b.Min, b.Max
	if minWait <= 0 {
		minWait = 500 * time.Millisecond
	}
	if maxWait < minWait {
		maxWait = minWait
	}
	factor := b.Factor
	if factor < 1 {
		factor = 2
	}

	window := float64(minWait)
	for range b.attempt {
		window *= factor
		if window >= float64(maxWait) {
			window = float64(maxWait)
			break
		}
	}
	b.attempt++

	// Full jitter over [min/2, window): a retry storm needs the spread more
	// than any individual daemon needs a predictable delay.
	span := window - float64(minWait)/2
	if span <= 0 {
		return minWait
	}
	return time.Duration(float64(minWait)/2 + rand.Float64()*span) //nolint:gosec // jitter, not a secret
}

// Reset returns the schedule to its first step. It is called after a
// connection has been up long enough to count as healthy, so a daemon that
// reconnects every hour does not inherit yesterday's backoff.
func (b *Backoff) Reset() { b.attempt = 0 }

// Attempt is how many waits have been handed out since the last reset.
func (b *Backoff) Attempt() int { return b.attempt }
