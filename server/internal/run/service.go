package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/project"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
)

// Config is the run service's operational configuration.
type Config struct {
	// OnlineWithin is how long a runtime stays "online" after its last
	// heartbeat. The contract fixes it at 30 seconds: past that a runtime is
	// reported offline and the runs it was holding are failed.
	OnlineWithin time.Duration
	// MaxAttempts is how many times a run may be handed out before a lost
	// runtime finishes it as an error rather than requeueing it. One means
	// "never silently re-run somebody's application", which is the right
	// default for a tool that clicks buttons in production-shaped systems.
	MaxAttempts int32
	// PresignBaseURL is the public origin a daemon reaches this API on. It is
	// what makes the artifactUpload.presignedPutBase in a run.assign frame
	// resolvable from inside the customer's network.
	PresignBaseURL string
	// EventPageLimit bounds GET /runs/{id}/events.
	EventPageLimit int32
}

// DefaultConfig is the shape a deployment gets without tuning anything.
func DefaultConfig() Config {
	return Config{
		OnlineWithin:   30 * time.Second,
		MaxAttempts:    1,
		EventPageLimit: 500,
	}
}

// Service is the domain layer for runs. It is the only place that decides a
// run's status, and (per ADR-005) the only place that reads runs.status to
// make a scheduling decision.
type Service struct {
	store     auth.Store
	projects  *project.Service
	hub       *realtime.Hub
	registry  *realtime.Registry
	artifacts *artifact.Service
	cfg       Config

	// wake nudges the scheduler when there is new work. It is set by
	// NewScheduler, and a nil one simply means "no scheduler in this process",
	// which is what the service-level tests run with.
	wake func()
}

// NewService wires the run service.
func NewService(store auth.Store, projects *project.Service, hub *realtime.Hub, registry *realtime.Registry, artifacts *artifact.Service, cfg Config) *Service {
	defaults := DefaultConfig()
	if cfg.OnlineWithin <= 0 {
		cfg.OnlineWithin = defaults.OnlineWithin
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaults.MaxAttempts
	}
	if cfg.EventPageLimit <= 0 {
		cfg.EventPageLimit = defaults.EventPageLimit
	}
	if artifacts == nil {
		artifacts = artifact.Disabled()
	}
	return &Service{store: store, projects: projects, hub: hub, registry: registry, artifacts: artifacts, cfg: cfg}
}

// CreateInput is POST /api/v1/runs, already parsed.
type CreateInput struct {
	ProjectID uuid.UUID
	// RuntimeID pins the run to one runtime. Absent means "any online runtime
	// in this organization", and the scheduler decides.
	RuntimeID *uuid.UUID
	Mode      string
	// TestCaseIDs is an explicit selection for an execute/full run. Absent
	// means "every approved case in the project".
	TestCaseIDs []uuid.UUID
	// IdempotencyKey makes a retried POST return the original run instead of
	// starting a second browser session against the customer's application.
	IdempotencyKey string
}

// Created is the outcome of Create. Existing distinguishes "we started a run"
// (201) from "you already did" (200), which is the only thing an idempotent
// create owes its caller.
type Created struct {
	Run      dbgen.Run
	Existing bool
}

// Create enqueues a run.
//
// Everything that could leave a half-built run behind — the run row and its
// pre-seeded executions — happens in one transaction, so a failure between the
// two cannot produce a run that will execute nothing and never say why.
func (s *Service) Create(ctx context.Context, scope auth.OrgScope, in CreateInput) (Created, error) {
	mode, err := parseMode(in.Mode)
	if err != nil {
		return Created{}, err
	}
	if err := validateIdempotencyKey(in.IdempotencyKey); err != nil {
		return Created{}, err
	}

	if in.IdempotencyKey != "" {
		existing, found, err := s.byIdempotencyKey(ctx, scope.OrgID, in.IdempotencyKey)
		if err != nil {
			return Created{}, err
		}
		if found {
			return Created{Run: existing, Existing: true}, nil
		}
	}

	var created dbgen.Run
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		project, err := q.GetProject(ctx, dbgen.GetProjectParams{OrgID: scope.OrgID, ID: in.ProjectID})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				return httpx.NotFound("project not found")
			}
			return fmt.Errorf("look up project: %w", db.Classify(err))
		}
		if project.ArchivedAt.Valid {
			return httpx.Conflict("that project is archived")
		}

		runtimeID, err := s.resolveRuntime(ctx, q, scope.OrgID, in.RuntimeID)
		if err != nil {
			return err
		}

		created, err = q.CreateRun(ctx, dbgen.CreateRunParams{
			OrgID:          scope.OrgID,
			ProjectID:      in.ProjectID,
			RuntimeID:      runtimeID,
			Mode:           mode,
			IdempotencyKey: textOrNull(in.IdempotencyKey),
			CreatedBy:      uuid.NullUUID{UUID: scope.UserID, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("create run: %w", db.Classify(err))
		}

		return s.seedExecutions(ctx, q, scope.OrgID, created, in)
	})
	if err != nil {
		// A concurrent POST with the same Idempotency-Key won the unique
		// index. That is the retry this key exists for, so return its run
		// rather than a conflict the client cannot act on.
		if in.IdempotencyKey != "" && errors.Is(err, db.ErrConflict) {
			existing, found, lookupErr := s.byIdempotencyKey(ctx, scope.OrgID, in.IdempotencyKey)
			if lookupErr == nil && found {
				return Created{Run: existing, Existing: true}, nil
			}
		}
		return Created{}, err
	}

	s.notifyScheduler()
	s.publishStatus(created)
	return Created{Run: created}, nil
}

// resolveRuntime validates an explicitly named runtime. A run with no runtime
// is legal and stays queued until the scheduler finds one: per the contract, a
// run that cannot be placed waits rather than failing.
func (s *Service) resolveRuntime(ctx context.Context, q *dbgen.Queries, orgID uuid.UUID, requested *uuid.UUID) (uuid.NullUUID, error) {
	if requested == nil {
		return uuid.NullUUID{}, nil
	}
	runtime, err := q.GetRuntime(ctx, dbgen.GetRuntimeParams{OrgID: orgID, ID: *requested})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return uuid.NullUUID{}, httpx.NotFound("runtime not found")
		}
		return uuid.NullUUID{}, fmt.Errorf("look up runtime: %w", db.Classify(err))
	}
	if runtime.DisabledAt.Valid {
		return uuid.NullUUID{}, httpx.Conflict("that runtime is disabled")
	}
	return uuid.NullUUID{UUID: runtime.ID, Valid: true}, nil
}

// seedExecutions pre-inserts the run's work list.
//
// The rows exist before the daemon ever sees the run, which is what makes
// UNIQUE (run_id, test_case_id) the dedup gate for a redelivered result and
// keeps the selection referentially intact — there is no uuid[] column that
// can end up pointing at a deleted case.
func (s *Service) seedExecutions(ctx context.Context, q *dbgen.Queries, orgID uuid.UUID, created dbgen.Run, in CreateInput) error {
	if created.Mode != dbgen.RunModeExecute && created.Mode != dbgen.RunModeFull {
		if len(in.TestCaseIDs) > 0 {
			return httpx.InvalidField("testCaseIds", "is only meaningful for an execute or full run")
		}
		return nil
	}

	cases, err := s.selectTestCases(ctx, q, orgID, in)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		if created.Mode == dbgen.RunModeExecute {
			return httpx.Conflict("that project has no approved test cases to execute")
		}
		// A full run plans before it executes, so an empty suite is the normal
		// first run rather than a mistake.
		return nil
	}

	ids := make([]uuid.UUID, len(cases))
	for i, c := range cases {
		ids[i] = c.ID
	}
	if _, err := q.CreateExecutionsForRun(ctx, dbgen.CreateExecutionsForRunParams{
		OrgID: orgID, RunID: created.ID, TestCaseIds: ids,
	}); err != nil {
		return fmt.Errorf("seed executions: %w", db.Classify(err))
	}
	return nil
}

func (s *Service) selectTestCases(ctx context.Context, q *dbgen.Queries, orgID uuid.UUID, in CreateInput) ([]dbgen.TestCase, error) {
	if len(in.TestCaseIDs) == 0 {
		cases, err := q.ListApprovedTestCases(ctx, dbgen.ListApprovedTestCasesParams{OrgID: orgID, ProjectID: in.ProjectID})
		if err != nil {
			return nil, fmt.Errorf("list approved test cases: %w", db.Classify(err))
		}
		return cases, nil
	}

	cases, err := q.ListTestCasesByIDs(ctx, dbgen.ListTestCasesByIDsParams{
		OrgID: orgID, ProjectID: in.ProjectID, Ids: dedupeIDs(in.TestCaseIDs),
	})
	if err != nil {
		return nil, fmt.Errorf("look up test cases: %w", db.Classify(err))
	}
	if len(cases) != len(dedupeIDs(in.TestCaseIDs)) {
		// Which id was wrong is deliberately not reported: an id belonging to
		// another project or another tenant must not be distinguishable from
		// one that does not exist at all.
		return nil, httpx.NotFound("one or more of those test cases do not exist in this project")
	}
	return cases, nil
}

func (s *Service) byIdempotencyKey(ctx context.Context, orgID uuid.UUID, key string) (dbgen.Run, bool, error) {
	existing, err := s.store.GetRunByIdempotencyKey(ctx, dbgen.GetRunByIdempotencyKeyParams{
		OrgID: orgID, IdempotencyKey: textOrNull(key),
	})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return dbgen.Run{}, false, nil
		}
		return dbgen.Run{}, false, fmt.Errorf("look up idempotency key: %w", db.Classify(err))
	}
	return existing, true, nil
}

// Get returns one run. A run in another organization is a 404, not a 403.
func (s *Service) Get(ctx context.Context, scope auth.OrgScope, runID uuid.UUID) (dbgen.Run, error) {
	found, err := s.store.GetRun(ctx, dbgen.GetRunParams{OrgID: scope.OrgID, ID: runID})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return dbgen.Run{}, httpx.NotFound("run not found")
		}
		return dbgen.Run{}, fmt.Errorf("look up run: %w", db.Classify(err))
	}
	return found, nil
}

// Listed is one page of runs plus the total, so a UI can render a pager
// without a second request.
type Listed struct {
	Runs  []dbgen.Run
	Total int64
}

// List returns a page of an organization's runs, newest first.
func (s *Service) List(ctx context.Context, scope auth.OrgScope, projectID *uuid.UUID, page httpx.Page) (Listed, error) {
	filter := uuid.NullUUID{}
	if projectID != nil {
		filter = uuid.NullUUID{UUID: *projectID, Valid: true}
	}

	runs, err := s.store.ListRuns(ctx, dbgen.ListRunsParams{
		OrgID: scope.OrgID, ProjectID: filter, Limit: page.Limit, Offset: page.Offset,
	})
	if err != nil {
		return Listed{}, fmt.Errorf("list runs: %w", db.Classify(err))
	}
	total, err := s.store.CountRuns(ctx, dbgen.CountRunsParams{OrgID: scope.OrgID, ProjectID: filter})
	if err != nil {
		return Listed{}, fmt.Errorf("count runs: %w", db.Classify(err))
	}
	return Listed{Runs: runs, Total: total}, nil
}

// Cancel stops a run.
//
// Cancelling an already-cancelled run succeeds: cancel is a retryable request
// and the caller's intent is already satisfied. Cancelling a run that finished
// on its own is a 409, because the outcome the caller is asking for is no
// longer reachable.
func (s *Service) Cancel(ctx context.Context, scope auth.OrgScope, runID uuid.UUID) (dbgen.Run, error) {
	var cancelled dbgen.Run
	alreadyCancelled := false

	err := s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		// FOR UPDATE, not a bare read: two cancels arriving together must not
		// both decide the run is live and both send a run.cancel frame.
		current, err := q.GetRunForUpdate(ctx, dbgen.GetRunForUpdateParams{OrgID: scope.OrgID, ID: runID})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				return httpx.NotFound("run not found")
			}
			return fmt.Errorf("look up run: %w", db.Classify(err))
		}

		if current.Status == dbgen.RunStatusCancelled {
			cancelled, alreadyCancelled = current, true
			return nil
		}
		if isTerminal(current.Status) {
			return httpx.Conflict("that run already finished as %s", current.Status)
		}

		cancelled, err = q.CancelRun(ctx, dbgen.CancelRunParams{OrgID: scope.OrgID, ID: runID})
		if err != nil {
			return fmt.Errorf("cancel run: %w", db.Classify(err))
		}
		return nil
	})
	if err != nil {
		return dbgen.Run{}, err
	}
	if alreadyCancelled {
		return cancelled, nil
	}

	s.publishStatus(cancelled)
	// Telling the daemon is best-effort by design: the run is already
	// cancelled in the source of truth, and a daemon that is offline will find
	// out when it reconnects and its result frame is rejected as terminal.
	s.tellDaemonToCancel(ctx, cancelled)
	return cancelled, nil
}

func (s *Service) tellDaemonToCancel(ctx context.Context, run dbgen.Run) {
	if !run.RuntimeID.Valid {
		return
	}
	frame, err := realtime.NewFrame(qaschemaRunCancel, &run.ID, cancelSeq, cancelPayload())
	if err != nil {
		httpx.LoggerFrom(ctx).ErrorContext(ctx, "could not build run.cancel", "err", err, "run_id", run.ID)
		return
	}
	target := realtime.Target{OrgID: run.OrgID, RuntimeID: run.RuntimeID.UUID}
	if err := s.registry.Send(ctx, target, frame); err != nil && !errors.Is(err, realtime.ErrRuntimeOffline) {
		httpx.LoggerFrom(ctx).WarnContext(ctx, "could not deliver run.cancel", "err", err, "run_id", run.ID)
	}
}

// Events returns a page of a run's event stream after `since`.
func (s *Service) Events(ctx context.Context, scope auth.OrgScope, runID uuid.UUID, since int64, limit int32) ([]dbgen.RunEvent, error) {
	// Establishes both that the run exists and that it is this tenant's,
	// before any event row is read.
	if _, err := s.Get(ctx, scope, runID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > s.cfg.EventPageLimit {
		limit = s.cfg.EventPageLimit
	}

	events, err := s.store.ListRunEventsSince(ctx, dbgen.ListRunEventsSinceParams{
		OrgID: scope.OrgID, RunID: runID, Seq: since, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", db.Classify(err))
	}
	return events, nil
}

// OpenRunStream implements realtime.StreamSource.
func (s *Service) OpenRunStream(ctx context.Context, orgID, runID uuid.UUID, since int64) (realtime.RunStream, error) {
	scope := auth.OrgScope{OrgID: orgID}
	current, err := s.Get(ctx, scope, runID)
	if err != nil {
		return realtime.RunStream{}, err
	}

	lastSeq, err := s.store.GetLastRunEventSeq(ctx, dbgen.GetLastRunEventSeqParams{OrgID: orgID, RunID: runID})
	if err != nil {
		return realtime.RunStream{}, fmt.Errorf("read last event seq: %w", db.Classify(err))
	}

	events, err := s.Events(ctx, scope, runID, since, s.cfg.EventPageLimit)
	if err != nil {
		return realtime.RunStream{}, err
	}

	view := NewView(current)
	open, err := json.Marshal(streamFrame{Type: FrameSnapshot, RunID: runID, Run: &view, LastSeq: &lastSeq})
	if err != nil {
		return realtime.RunStream{}, fmt.Errorf("marshal run snapshot: %w", err)
	}

	backlog := make([]realtime.Message, 0, len(events))
	for _, event := range events {
		msg, err := eventMessage(runID, event)
		if err != nil {
			return realtime.RunStream{}, err
		}
		backlog = append(backlog, msg)
	}
	return realtime.RunStream{Open: open, Backlog: backlog}, nil
}

// publishStatus tells every browser watching a run that its state changed.
func (s *Service) publishStatus(run dbgen.Run) {
	view := NewView(run)
	frame, err := json.Marshal(streamFrame{Type: FrameStatus, RunID: run.ID, Run: &view})
	if err != nil {
		// Marshalling a struct of scalars cannot fail; if it somehow does, the
		// database is still correct and the client will see the state on its
		// next poll or reconnect.
		return
	}
	s.hub.Publish(run.ID, realtime.Message{Seq: realtime.NoSequence, Frame: frame})
}

func eventMessage(runID uuid.UUID, event dbgen.RunEvent) (realtime.Message, error) {
	view := NewEventView(event)
	frame, err := json.Marshal(streamFrame{Type: FrameEvent, RunID: runID, Event: &view})
	if err != nil {
		return realtime.Message{}, fmt.Errorf("marshal run event: %w", err)
	}
	return realtime.Message{Seq: event.Seq, Frame: frame}, nil
}

func (s *Service) notifyScheduler() {
	if s.wake != nil {
		s.wake()
	}
}

// parseMode validates the run mode. An unknown one is a 422 rather than a
// database enum error, so the client is told what the four values are.
func parseMode(raw string) (dbgen.RunMode, error) {
	switch dbgen.RunMode(raw) {
	case dbgen.RunModeDiscover, dbgen.RunModePlan, dbgen.RunModeExecute, dbgen.RunModeFull:
		return dbgen.RunMode(raw), nil
	default:
		return "", httpx.InvalidField("mode", "must be one of discover, plan, execute, full")
	}
}

// maxIdempotencyKeyLength bounds what goes into a unique index. The value is a
// client-chosen retry token; a uuid or a hash is what it is for.
const maxIdempotencyKeyLength = 200

func validateIdempotencyKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > maxIdempotencyKeyLength || strings.TrimSpace(key) != key {
		return httpx.InvalidField("Idempotency-Key",
			fmt.Sprintf("must be at most %d characters with no surrounding whitespace", maxIdempotencyKeyLength))
	}
	return nil
}

func isTerminal(status dbgen.RunStatus) bool {
	switch status {
	case dbgen.RunStatusPassed, dbgen.RunStatusFailed, dbgen.RunStatusCancelled, dbgen.RunStatusError:
		return true
	default:
		return false
	}
}

func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// dedupeIDs removes repeats while keeping the caller's order, so a selection
// that names the same case twice is not reported as a missing one.
func dedupeIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
