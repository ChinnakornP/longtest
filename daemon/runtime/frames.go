package runtime

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// envelopeSchema is the contract every frame on this connection is validated
// against, in both directions.
const envelopeSchema = "daemon-envelope@1"

// seqAllocator hands out the seq field of the envelope.
//
// The contract calls seq monotonic per (connection, runId) and the server
// deduplicates on (runId, seq). This allocator keeps a run's counter across
// reconnects rather than restarting it at zero, which satisfies "monotonic per
// connection" and also avoids the trap in the weaker reading: a run whose
// events were already stored as seq 0..9 would have its post-reconnect events
// silently deduplicated away if the counter restarted. Runtime-scoped frames
// (hello, heartbeat) carry no runId, so they use a counter that does reset per
// connection — there is nothing for them to collide with.
type seqAllocator struct {
	mu      sync.Mutex
	perRun  map[string]int
	runtime int
}

func newSeqAllocator() *seqAllocator {
	return &seqAllocator{perRun: map[string]int{}}
}

// NextRun returns the next seq for a run-scoped frame.
func (s *seqAllocator) NextRun(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.perRun[runID]
	s.perRun[runID] = next + 1
	return next
}

// NextRuntime returns the next seq for hello or heartbeat.
func (s *seqAllocator) NextRuntime() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.runtime
	s.runtime++
	return next
}

// ResetRuntime restarts the runtime-scoped counter for a new connection.
func (s *seqAllocator) ResetRuntime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = 0
}

// Forget drops a finished run's counter so a long-lived daemon does not keep
// one integer per run it has ever executed.
func (s *seqAllocator) Forget(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.perRun, runID)
}

// newEnvelope builds a frame with a fresh message id and timestamp.
func newEnvelope(frameType qaschema.EnvelopeType, runID *string, seq int, payload any, now time.Time) (qaschema.Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return qaschema.Envelope{}, fmt.Errorf("runtime: encode %s payload: %w", frameType, err)
	}
	return qaschema.Envelope{
		V:       1,
		Type:    frameType,
		MsgID:   uuid.NewString(),
		RunID:   runID,
		Seq:     seq,
		Ts:      now.UTC().Format(time.RFC3339Nano),
		Payload: raw,
	}, nil
}

// decodePayload decodes an envelope payload into a typed struct.
func decodePayload(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return fmt.Errorf("runtime: empty payload")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("runtime: decode payload: %w", err)
	}
	return nil
}

// validateFrame checks a frame against contract D.
//
// Incoming frames are validated because they carry work the daemon is about to
// execute on this operator's network, and the daemon is the last gate before
// a base URL is opened in a browser. Outgoing frames are validated in tests
// rather than on the hot path.
func validateFrame(data []byte) error {
	result, err := qaschema.ValidateJSON(envelopeSchema, data)
	if err != nil {
		return fmt.Errorf("runtime: validate frame: %w", err)
	}
	if !result.Valid {
		return fmt.Errorf("runtime: frame does not match %s: %s", envelopeSchema, result.Errors[0].String())
	}
	return nil
}
