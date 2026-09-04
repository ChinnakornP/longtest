package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// This file implements realtime.ControlPlane. Every method takes the
// auth.RuntimeCaller its connection's token resolved to, and every query below
// binds rc.OrgID. Nothing a daemon sends can widen that: a frame naming
// another tenant's run reaches a query that was given this tenant's org id and
// finds no row.

var _ realtime.ControlPlane = (*Service)(nil)

// Hello records a daemon's capability report.
func (s *Service) Hello(ctx context.Context, rc auth.RuntimeCaller, payload qaschema.HelloPayload) error {
	browsers, err := json.Marshal(payload.Browsers)
	if err != nil {
		return fmt.Errorf("marshal browsers: %w", err)
	}
	agents, err := json.Marshal(payload.Agents)
	if err != nil {
		return fmt.Errorf("marshal agents: %w", err)
	}

	if _, err := s.store.RecordRuntimeHello(ctx, dbgen.RecordRuntimeHelloParams{
		OrgID:    rc.OrgID,
		ID:       rc.RuntimeID,
		Version:  payload.Version,
		Browsers: browsers,
		Agents:   agents,
	}); err != nil {
		return fmt.Errorf("record runtime hello: %w", db.Classify(err))
	}
	return nil
}

// Heartbeat refreshes the runtime's last_seen_at and the lease on every run it
// says it is still working on.
//
// Both writes are advisory rather than transactional: they are liveness, and a
// heartbeat that loses a race with a status change must not roll that change
// back.
func (s *Service) Heartbeat(ctx context.Context, rc auth.RuntimeCaller, payload qaschema.HeartbeatPayload) error {
	if _, err := s.store.TouchRuntime(ctx, dbgen.TouchRuntimeParams{OrgID: rc.OrgID, ID: rc.RuntimeID}); err != nil {
		return fmt.Errorf("touch runtime: %w", db.Classify(err))
	}
	if len(payload.ActiveRuns) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, 0, len(payload.ActiveRuns))
	for _, raw := range payload.ActiveRuns {
		id, err := uuid.Parse(raw)
		if err != nil {
			return &realtime.ProtocolError{Reason: "heartbeat lists a run id that is not a uuid"}
		}
		ids = append(ids, id)
	}

	// One statement for the whole list, and bound to this runtime: a heartbeat
	// naming a run held by a different daemon in the same organization extends
	// nothing.
	if _, err := s.store.HeartbeatRunsForRuntime(ctx, dbgen.HeartbeatRunsForRuntimeParams{
		OrgID: rc.OrgID, RuntimeID: uuid.NullUUID{UUID: rc.RuntimeID, Valid: true}, Ids: ids,
	}); err != nil {
		return fmt.Errorf("refresh run leases: %w", db.Classify(err))
	}
	return nil
}

// eventPayload is run.event as this package reads it. `data` stays raw: it is
// free-form detail the daemon lifted off the application under test, stored as
// data and never interpreted here.
type eventPayload struct {
	Phase      string          `json:"phase"`
	Level      string          `json:"level"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	TestCaseID *string         `json:"testCaseId,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// RunEvent appends one event and, if it was new, forwards it to every browser
// watching the run.
//
// Delivery from the daemon is at-least-once, so this is idempotent on
// (run_id, seq): AppendRunEvent reports how many rows it inserted, and a
// redelivery inserts none and publishes nothing. That is the whole mechanism
// behind "send the same seq a hundred times, get one row and one browser
// event".
func (s *Service) RunEvent(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, seq int64, ts time.Time, raw json.RawMessage) error {
	current, err := s.runForRuntime(ctx, rc, runID)
	if err != nil {
		return err
	}

	var payload eventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return &realtime.ProtocolError{Reason: "run.event payload is not decodable"}
	}
	data := payload.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}

	inserted, err := s.store.AppendRunEvent(ctx, dbgen.AppendRunEventParams{
		OrgID:   rc.OrgID,
		RunID:   runID,
		Seq:     seq,
		Phase:   payload.Phase,
		Level:   dbgen.RunEventLevel(payload.Level),
		Code:    payload.Code,
		Message: payload.Message,
		Data:    data,
		Ts:      pgtype.Timestamptz{Time: ts, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("append run event: %w", db.Classify(err))
	}
	if inserted == 0 {
		// A redelivery. The row is already there and the browsers already saw
		// it; publishing again would put a duplicate on every open stream.
		return nil
	}

	s.hub.Publish(runID, mustEventMessage(runID, EventView{
		Seq: seq, Phase: payload.Phase, Level: payload.Level,
		Code: payload.Code, Message: payload.Message, Data: data, Ts: ts.UTC(),
	}))

	s.advanceProgress(ctx, rc, current, payload.Phase)
	return nil
}

// advanceProgress keeps the run row's status and phase in step with the events
// the daemon is emitting, so the run list is useful without replaying the
// stream. It is best-effort: the event is already durable, and failing the
// frame would make the daemon replay an event we already stored.
func (s *Service) advanceProgress(ctx context.Context, rc auth.RuntimeCaller, current dbgen.Run, phase string) {
	if phase == "" || (current.Status == dbgen.RunStatusRunning && current.Phase == phase) {
		return
	}

	var updated dbgen.Run
	var err error
	if current.Status == dbgen.RunStatusAssigned {
		updated, err = s.store.MarkRunRunning(ctx, dbgen.MarkRunRunningParams{
			OrgID: rc.OrgID, ID: current.ID, Phase: phase,
		})
	} else {
		if _, err = s.store.SetRunPhase(ctx, dbgen.SetRunPhaseParams{
			OrgID: rc.OrgID, ID: current.ID, Phase: phase,
		}); err == nil {
			updated = current
			updated.Phase = phase
		}
	}
	if err != nil {
		if !errors.Is(db.Classify(err), db.ErrNotFound) {
			httpx.LoggerFrom(ctx).WarnContext(ctx, "could not advance run progress", "err", db.Classify(err), "run_id", current.ID)
		}
		return
	}
	if updated.ID != uuid.Nil {
		s.publishStatus(updated)
	}
}

// RuntimeDisconnected deals with the runs a daemon was holding when its
// control-plane connection ended.
//
// It does NOT fail them: a daemon reconnects within seconds after a deploy or a
// blip, and killing its in-flight run would turn every restart into a lost run.
// The lease sweeper is what eventually finishes a run whose daemon never comes
// back, and this only records that the runtime is no longer reporting.
func (s *Service) RuntimeDisconnected(ctx context.Context, rc auth.RuntimeCaller) {
	inFlight, err := s.store.ListInFlightRunsForRuntime(ctx, dbgen.ListInFlightRunsForRuntimeParams{
		OrgID: rc.OrgID, RuntimeID: uuid.NullUUID{UUID: rc.RuntimeID, Valid: true},
	})
	if err != nil {
		httpx.LoggerFrom(ctx).WarnContext(ctx, "could not list in-flight runs for a disconnected runtime",
			"err", db.Classify(err), "runtime_id", rc.RuntimeID)
		return
	}
	if len(inFlight) == 0 {
		return
	}
	httpx.LoggerFrom(ctx).InfoContext(ctx, "runtime disconnected with runs in flight",
		"runtime_id", rc.RuntimeID, "runs", len(inFlight),
		"lease_expires_in", s.cfg.OnlineWithin.String())
}

// runForRuntime resolves a frame's run id to a run this daemon is allowed to
// speak about.
//
// The organization and the runtime both come from the token, and both are in
// the WHERE clause, so a frame naming another tenant's run — or another
// runtime's run in the same tenant — finds nothing. It is a protocol error
// rather than a soft failure: a daemon has no business sending it, there is no
// per-frame error channel in contract D, and closing the connection is how a
// misconfigured or hostile daemon stops.
func (s *Service) runForRuntime(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID) (dbgen.Run, error) {
	found, err := s.store.GetRunForRuntime(ctx, dbgen.GetRunForRuntimeParams{
		OrgID: rc.OrgID, ID: runID, RuntimeID: uuid.NullUUID{UUID: rc.RuntimeID, Valid: true},
	})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return dbgen.Run{}, &realtime.ProtocolError{Reason: "that run is not assigned to this runtime"}
		}
		return dbgen.Run{}, fmt.Errorf("look up run for runtime: %w", db.Classify(err))
	}
	return found, nil
}

func mustEventMessage(runID uuid.UUID, view EventView) realtime.Message {
	frame, err := json.Marshal(streamFrame{Type: FrameEvent, RunID: runID, Event: &view})
	if err != nil {
		// A struct of scalars plus one json.RawMessage that the schema
		// validator already parsed. Unreachable.
		return realtime.Message{Seq: view.Seq, Frame: []byte(`{"type":"run.event"}`)}
	}
	return realtime.Message{Seq: view.Seq, Frame: frame}
}
