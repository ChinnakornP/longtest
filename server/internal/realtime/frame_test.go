package realtime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Every frame this backend emits is validated against daemon-envelope@1 before
// it goes on the wire, so a change here that the contract does not describe
// fails the build rather than a daemon in the field.
func TestNewFrameProducesAContractValidEnvelope(t *testing.T) {
	runID := uuid.New()

	frame, err := NewFrame(qaschema.EnvelopeTypeRunCancel, &runID, 0, qaschema.RunCancelPayload{
		Reason: qaschema.RunCancelPayloadReasonUserRequested,
	})
	if err != nil {
		t.Fatalf("build run.cancel: %v", err)
	}
	if err := qaschema.MustBeValid("daemon-envelope@1", frame); err != nil {
		t.Fatalf("the frame does not match the contract: %v", err)
	}

	envelope, err := ParseFrame(frame)
	if err != nil {
		t.Fatalf("parse the frame back: %v", err)
	}
	if envelope.Type != qaschema.EnvelopeTypeRunCancel {
		t.Fatalf("got type %q, want run.cancel", envelope.Type)
	}
	if envelope.RunID == nil || *envelope.RunID != runID.String() {
		t.Fatalf("got runId %v, want %s", envelope.RunID, runID)
	}
}

// A payload the contract does not describe must fail here rather than reach a
// daemon.
func TestNewFrameRefusesAPayloadTheContractRejects(t *testing.T) {
	runID := uuid.New()
	if _, err := NewFrame(qaschema.EnvelopeTypeRunCancel, &runID, 0, map[string]any{
		"reason": "because-i-said-so",
	}); err == nil {
		t.Fatal("built a run.cancel with a reason the contract does not define")
	}
}

func TestParseFrameRejectsWhatTheContractDoesNot(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "not json", raw: `{`},
		{name: "no version", raw: `{"type":"hello","msgId":"` + uuid.NewString() + `","seq":0,"ts":"2026-09-04T00:00:00Z","payload":{}}`},
		{name: "unknown type", raw: `{"v":1,"type":"run.explode","msgId":"` + uuid.NewString() + `","seq":0,"ts":"2026-09-04T00:00:00Z","payload":{}}`},
		{name: "negative seq", raw: `{"v":1,"type":"heartbeat","msgId":"` + uuid.NewString() + `","seq":-1,"ts":"2026-09-04T00:00:00Z","payload":{"uptimeSec":1}}`},
		{name: "a run frame with no runId", raw: `{"v":1,"type":"run.event","msgId":"` + uuid.NewString() + `","seq":0,"ts":"2026-09-04T00:00:00Z","payload":{"phase":"discover","level":"info","code":"x","message":"y"}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFrame([]byte(tc.raw))
			if err == nil {
				t.Fatalf("accepted %s", tc.raw)
			}
			// A frame the contract does not describe ends the connection, so
			// it has to be distinguishable from a storage failure.
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("got %T, want a *ProtocolError", err)
			}
		})
	}
}

// The daemon's own timestamp is what run_events.ts records: when the event
// happened on the machine under test, not when we managed to store it.
func TestFrameTimeUsesTheDaemonsClock(t *testing.T) {
	runID := uuid.New()
	frame, err := NewFrame(qaschema.EnvelopeTypeRunEvent, &runID, 7, qaschema.RunEventPayload{
		Phase: qaschema.RunEventPayloadPhaseDiscover, Level: qaschema.RunEventPayloadLevelInfo,
		Code: "page_discovered", Message: "found /login",
	})
	if err != nil {
		t.Fatalf("build run.event: %v", err)
	}

	envelope, err := ParseFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ts, err := FrameTime(envelope)
	if err != nil {
		t.Fatalf("frame time: %v", err)
	}
	if time.Since(ts) > time.Minute {
		t.Fatalf("got ts %s, want something close to now", ts)
	}

	payload, err := DecodePayload[qaschema.RunEventPayload](envelope)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Code != "page_discovered" {
		t.Fatalf("got code %q, want page_discovered", payload.Code)
	}
}

// The payload survives the round trip byte for byte, which is what lets the
// backend store documents it does not own without rewriting them.
func TestFramePayloadIsCarriedVerbatim(t *testing.T) {
	runID := uuid.New()
	original := json.RawMessage(`{"phase":"discover","level":"info","code":"x","message":"y","data":{"z":[1,2,3]}}`)

	frame, err := NewFrame(qaschema.EnvelopeTypeRunEvent, &runID, 1, original)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	envelope, err := ParseFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(envelope.Payload) != string(original) {
		t.Fatalf("payload changed:\n got: %s\nwant: %s", envelope.Payload, original)
	}
}
