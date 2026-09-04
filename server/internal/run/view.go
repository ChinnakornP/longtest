package run

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// Counters are a run's execution tallies, kept on the run row so listing runs
// does not aggregate executions per row.
type Counters struct {
	Total   int32 `json:"total"`
	Passed  int32 `json:"passed"`
	Failed  int32 `json:"failed"`
	Skipped int32 `json:"skipped"`
	Errored int32 `json:"errored"`
}

// Failure is the domain error a run finished with. It is never a driver error
// and never a SQL fragment: the only values that reach it are the codes in
// daemon-envelope@1's RunError plus the ones this package sets itself.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// View is a run as the API renders it.
type View struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID uuid.UUID  `json:"projectId"`
	RuntimeID *uuid.UUID `json:"runtimeId"`
	Mode      string     `json:"mode"`
	Status    string     `json:"status"`
	// Phase is a free-form progress label ("discover", "plan", …). It is not
	// an enum here because the pipeline grows steps faster than a migration
	// can follow.
	Phase      string     `json:"phase"`
	Counters   Counters   `json:"counters"`
	Error      *Failure   `json:"error,omitempty"`
	Attempts   int32      `json:"attempts"`
	CreatedAt  time.Time  `json:"createdAt"`
	AssignedAt *time.Time `json:"assignedAt,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// NewView renders a run row.
func NewView(r dbgen.Run) View {
	view := View{
		ID:        r.ID,
		ProjectID: r.ProjectID,
		Mode:      string(r.Mode),
		Status:    string(r.Status),
		Phase:     r.Phase,
		Counters: Counters{
			Total:   r.TotalCount,
			Passed:  r.PassedCount,
			Failed:  r.FailedCount,
			Skipped: r.SkippedCount,
			Errored: r.ErrorCount,
		},
		Attempts:   r.Attempts,
		CreatedAt:  timeOf(r.CreatedAt),
		AssignedAt: optionalTime(r.AssignedAt),
		StartedAt:  optionalTime(r.StartedAt),
		FinishedAt: optionalTime(r.FinishedAt),
	}
	if r.RuntimeID.Valid {
		id := r.RuntimeID.UUID
		view.RuntimeID = &id
	}
	if r.ErrorCode != "" || r.ErrorMessage != "" {
		view.Error = &Failure{Code: r.ErrorCode, Message: r.ErrorMessage}
	}
	return view
}

// EventView is one entry of a run's event stream.
type EventView struct {
	Seq     int64           `json:"seq"`
	Phase   string          `json:"phase"`
	Level   string          `json:"level"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Ts      time.Time       `json:"ts"`
}

// NewEventView renders a stored event.
func NewEventView(e dbgen.RunEvent) EventView {
	data := e.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return EventView{
		Seq:     e.Seq,
		Phase:   e.Phase,
		Level:   string(e.Level),
		Code:    e.Code,
		Message: e.Message,
		Data:    data,
		Ts:      timeOf(e.Ts),
	}
}

// Frame types on the browser WebSocket. They are a small closed set so a
// client can switch on `type` and ignore what it does not know.
const (
	// FrameSnapshot is the first frame of every subscription: the run as it
	// stands, so a client that attaches to a finished run renders immediately.
	FrameSnapshot = "run.snapshot"
	// FrameEvent carries one run event, in sequence order.
	FrameEvent = "run.event"
	// FrameStatus is a run whose status, phase or counters changed.
	FrameStatus = "run.status"
)

// streamFrame is the wire shape of the browser stream.
type streamFrame struct {
	Type  string     `json:"type"`
	RunID uuid.UUID  `json:"runId"`
	Run   *View      `json:"run,omitempty"`
	Event *EventView `json:"event,omitempty"`
	// LastSeq is the highest event sequence stored when a snapshot was taken,
	// so a client knows what to pass as ?since if it has to reconnect.
	LastSeq *int64 `json:"lastSeq,omitempty"`
}

func timeOf(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time.UTC()
}

func optionalTime(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}
