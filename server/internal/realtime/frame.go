package realtime

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// envelopeSchema is the contract every control-plane frame is checked against,
// in both directions.
const envelopeSchema = "daemon-envelope@1"

// NewFrame marshals one control-plane frame and validates it before it goes on
// the wire.
//
// The outbound validation is not defensive programming about our own code: it
// is what stops a backend change from silently emitting something the frozen
// contract does not describe, which a daemon in the field would then have to
// cope with. It costs one schema walk per assignment, and assignments happen
// once per run.
func NewFrame(typ qaschema.EnvelopeType, runID *uuid.UUID, seq int64, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", typ, err)
	}

	envelope := qaschema.Envelope{
		V:       1,
		Type:    typ,
		MsgID:   uuid.NewString(),
		Seq:     int(seq),
		Ts:      time.Now().UTC().Format(time.RFC3339Nano),
		Payload: encoded,
	}
	if runID != nil {
		id := runID.String()
		envelope.RunID = &id
	}

	frame, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal %s envelope: %w", typ, err)
	}
	if err := qaschema.MustBeValid(envelopeSchema, frame); err != nil {
		return nil, fmt.Errorf("outbound %s frame does not match the contract: %w", typ, err)
	}
	return frame, nil
}

// ParseFrame decodes and validates one inbound frame.
//
// This is the trust boundary of the control plane: everything past it has
// already been checked against daemon-envelope@1, so no handler below has to
// re-check that a run.event names a run or that a level is one of four
// strings. What it deliberately does NOT establish is who the frame is about —
// the org and the runtime come from the token, never from here.
func ParseFrame(raw []byte) (qaschema.Envelope, error) {
	if err := qaschema.MustBeValid(envelopeSchema, raw); err != nil {
		return qaschema.Envelope{}, &ProtocolError{Reason: err.Error()}
	}
	var envelope qaschema.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// Unreachable in practice: the validator already parsed this document.
		return qaschema.Envelope{}, &ProtocolError{Reason: "frame is not valid JSON: " + err.Error()}
	}
	return envelope, nil
}

// ProtocolError is a frame the daemon should not have sent. It closes the
// connection: a daemon that speaks a shape we do not understand cannot be
// reasoned with frame by frame, and reconnecting is how it picks up a
// compatible backend after a deploy.
type ProtocolError struct{ Reason string }

func (e *ProtocolError) Error() string { return "protocol error: " + e.Reason }

// FrameRunID returns the run a frame is about. Every run.* frame has one; the
// schema has already enforced that, so a missing id here is a bug rather than
// a client error.
func FrameRunID(envelope qaschema.Envelope) (uuid.UUID, error) {
	if envelope.RunID == nil || *envelope.RunID == "" {
		return uuid.Nil, &ProtocolError{Reason: string(envelope.Type) + " has no runId"}
	}
	id, err := uuid.Parse(*envelope.RunID)
	if err != nil {
		return uuid.Nil, &ProtocolError{Reason: "runId is not a uuid"}
	}
	return id, nil
}

// FrameTime returns the daemon's own timestamp for a frame, which is what
// run_events.ts records: when the event happened on the machine under test,
// not when we managed to store it.
func FrameTime(envelope qaschema.Envelope) (time.Time, error) {
	ts, err := time.Parse(time.RFC3339, envelope.Ts)
	if err != nil {
		return time.Time{}, &ProtocolError{Reason: "ts is not an RFC 3339 timestamp"}
	}
	return ts.UTC(), nil
}

// DecodePayload unmarshals a frame's payload into the type its `type` field
// implies. The schema has already validated the payload's shape.
func DecodePayload[T any](envelope qaschema.Envelope) (T, error) {
	var payload T
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return payload, &ProtocolError{Reason: string(envelope.Type) + " payload is not decodable: " + err.Error()}
	}
	return payload, nil
}
