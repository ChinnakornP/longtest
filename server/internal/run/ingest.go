package run

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
	"github.com/ChinnakornP/longtest/server/internal/testcase"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// resultPayload is run.result as this package reads it.
//
// It differs from qaschema.RunResultPayload in one place: the test plan stays
// raw. Its cases are stored verbatim in test_cases.payload and handed back to
// a daemon unchanged, and round-tripping one through a Go struct would reorder
// its keys on every run and drop whatever a newer minor version of the
// contract added. Keeping the whole plan raw is also what lets it be
// re-validated against test-plan@1 here rather than trusted.
type resultPayload struct {
	Status string                   `json:"status"`
	Error  *qaschema.RunError       `json:"error,omitempty"`
	AppMap *qaschema.ApplicationMap `json:"appMap,omitempty"`
	// TestPlan stays as the bytes the planner wrote. They are re-validated
	// against test-plan@1 here — the daemon validated them too, but a daemon
	// is a customer-side process holding a pairing token, and "it already
	// checked" is not a reason to store what it sends.
	TestPlan   json.RawMessage            `json:"testPlan,omitempty"`
	Executions []qaschema.ExecutionResult `json:"executions,omitempty"`
	Findings   []qaschema.Finding         `json:"findings,omitempty"`
	Artifacts  []qaschema.Artifact        `json:"artifacts,omitempty"`
}

// RunResult ingests a terminal result frame and finishes the run.
//
// The whole ingest is one transaction. A run.result carries the application
// map, the test plan, every execution with its steps and assertions, every
// artifact and every finding; committing those in pieces would leave a run
// that is "passed" with half its executions missing and no way for a later
// request to tell.
//
// It is idempotent. The daemon's delivery is at-least-once, so a redelivered
// result must not double-count: every write below is an upsert or is guarded on
// the current status, and FinishRun only fires on a run that has not finished.
func (s *Service) RunResult(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, raw json.RawMessage) error {
	if _, err := s.runForRuntime(ctx, rc, runID); err != nil {
		return err
	}

	var payload resultPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return &realtime.ProtocolError{Reason: "run.result payload is not decodable"}
	}

	var finished dbgen.Run
	var plan *planOutcome
	err := s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		current, err := q.GetRunForUpdate(ctx, dbgen.GetRunForUpdateParams{OrgID: rc.OrgID(), ID: runID})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				return &realtime.ProtocolError{Reason: "that run does not exist"}
			}
			return fmt.Errorf("lock run: %w", db.Classify(err))
		}
		// Re-checked under the row lock: the run could have been reassigned
		// between the ownership check above and this transaction.
		if !current.RuntimeID.Valid || current.RuntimeID.UUID != rc.RuntimeID() {
			return &realtime.ProtocolError{Reason: "that run is not assigned to this runtime"}
		}

		ingest := &ingestion{
			q:               q,
			orgID:           rc.OrgID(),
			run:             current,
			artifacts:       map[string]uuid.UUID{},
			testCases:       map[string]dbgen.TestCase{},
			executionByCase: map[uuid.UUID]dbgen.Execution{},
			logger:          httpx.LoggerFrom(ctx),
		}
		if err := ingest.apply(ctx, payload); err != nil {
			return err
		}
		plan = ingest.plan

		refreshed, err := q.RefreshRunCounters(ctx, dbgen.RefreshRunCountersParams{OrgID: rc.OrgID(), ID: runID})
		if err != nil {
			return fmt.Errorf("refresh run counters: %w", db.Classify(err))
		}

		finished, err = finishRun(ctx, q, refreshed, payload)
		return err
	})

	var rejected *planRejected
	if errors.As(err, &rejected) {
		// The transaction above is gone, and with it every row this frame
		// would have written. The run itself still has to be closed out:
		// leaving it running forever because its plan was bad is a worse
		// outcome than an error the operator can read.
		return s.failRejectedPlan(ctx, rc, runID, rejected)
	}
	if err != nil {
		return err
	}

	if finished.ID != uuid.Nil {
		s.publishStatus(finished)
		// Narrated only on the delivery that actually finished the run. The
		// daemon's delivery is at-least-once, and a redelivery re-runs the
		// whole ingest — every write in it is an upsert, so it is harmless —
		// but the event stream is append-only, and a second plan_stored line
		// would be the one part of a redelivery a reader could see.
		s.narratePlan(ctx, rc, runID, plan)
	}
	// A finished run frees its runtime, so the scheduler should look for the
	// next queued run now rather than at the next tick.
	s.notifyScheduler()
	return nil
}

// failRejectedPlan closes out a run whose plan this backend refused.
//
// It is a second, minimal transaction on purpose. The first one was rolled
// back — that rollback is what guarantees no rejected case reached the
// database — so the run's terminal status cannot be written by it. This writes
// exactly two things: the run's error, and the event that says why.
func (s *Service) failRejectedPlan(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, rejected *planRejected) error {
	logger := httpx.LoggerFrom(ctx)
	logger.WarnContext(ctx, "rejected an AI test plan",
		"run_id", runID, "problems", len(rejected.Review.Rejections),
		"rules", rejected.Message())

	var finished dbgen.Run
	err := s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		current, err := q.GetRunForUpdate(ctx, dbgen.GetRunForUpdateParams{OrgID: rc.OrgID(), ID: runID})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				return &realtime.ProtocolError{Reason: "that run does not exist"}
			}
			return fmt.Errorf("lock run: %w", db.Classify(err))
		}
		if isTerminal(current.Status) {
			// Already closed — a redelivery of a result we rejected the first
			// time. The rejection is recorded; saying it twice is noise.
			return nil
		}

		if err := appendServerEvent(ctx, q, rc.OrgID(), current, planRejectedEvent(rejected)); err != nil {
			return err
		}
		finished, err = q.FinishRun(ctx, dbgen.FinishRunParams{
			OrgID:        rc.OrgID(),
			ID:           runID,
			Status:       dbgen.RunStatusError,
			ErrorCode:    string(qaschema.RunErrorCodeAgentOutputInvalid),
			ErrorMessage: truncate(rejected.Message(), maxRunErrorMessage),
		})
		if err != nil && !errors.Is(db.Classify(err), db.ErrNotFound) {
			return fmt.Errorf("finish rejected run: %w", db.Classify(err))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if finished.ID != uuid.Nil {
		s.publishStatus(finished)
	}
	s.notifyScheduler()
	return nil
}

// narratePlan records what an accepted plan did, as one run event.
//
// Best-effort: the cases are already committed, and losing the narration is a
// missing line in a log, not a missing test case. It is deliberately not part
// of the ingest transaction for exactly that reason — a failure to write a
// sentence must not roll back a plan.
func (s *Service) narratePlan(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, plan *planOutcome) {
	if plan == nil {
		return
	}
	current, err := s.store.GetRun(ctx, dbgen.GetRunParams{OrgID: rc.OrgID(), ID: runID})
	if err != nil {
		return
	}
	if err := s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		return appendServerEvent(ctx, q, rc.OrgID(), current, planStoredEvent(plan))
	}); err != nil {
		httpx.LoggerFrom(ctx).WarnContext(ctx, "could not record the plan outcome",
			"err", err, "run_id", runID)
	}
}

// maxRunErrorMessage bounds what goes into runs.error_message.
const maxRunErrorMessage = 500

// serverEvent is a run event this backend authored rather than forwarded.
type serverEvent struct {
	Code    string
	Level   dbgen.RunEventLevel
	Message string
	Data    map[string]any
}

// appendServerEvent puts one backend-authored event on a run's stream.
//
// The sequence is taken as one past the highest the run has, under the row
// lock the caller already holds. The daemon owns the sequence space, but a run
// receiving a server-authored event is a run whose result frame has arrived,
// which is the last frame a daemon sends for it — so there is no further
// daemon event to collide with.
func appendServerEvent(ctx context.Context, q *dbgen.Queries, orgID uuid.UUID, run dbgen.Run, event serverEvent) error {
	last, err := q.GetLastRunEventSeq(ctx, dbgen.GetLastRunEventSeqParams{OrgID: orgID, RunID: run.ID})
	if err != nil {
		return fmt.Errorf("read last event seq: %w", db.Classify(err))
	}
	data, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal run event data: %w", err)
	}
	if _, err := q.AppendRunEvent(ctx, dbgen.AppendRunEventParams{
		OrgID:   orgID,
		RunID:   run.ID,
		Seq:     last + 1,
		Phase:   string(qaschema.RunEventPayloadPhasePlan),
		Level:   event.Level,
		Code:    event.Code,
		Message: event.Message,
		Data:    data,
		Ts:      pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		return fmt.Errorf("append run event: %w", db.Classify(err))
	}
	return nil
}

// planRejectedEvent is the event a refused plan leaves behind.
//
// The rejections travel in `data` as the structured rule/detail pairs the
// planner's next attempt is told about. They are the validator's own sentences
// — "no element X exists in this project's application map" — and name a ref
// the model wrote, which is why the human-facing `message` is the rule counts
// instead.
func planRejectedEvent(rejected *planRejected) serverEvent {
	return serverEvent{
		Code:    "plan_rejected",
		Level:   dbgen.RunEventLevelError,
		Message: rejected.Message(),
		Data: map[string]any{
			"rejections": rejected.Review.Rejections,
			"stored":     0,
		},
	}
}

func planStoredEvent(plan *planOutcome) serverEvent {
	missing := make([]string, 0, 5)
	for _, category := range plan.Review.MissingCategories() {
		missing = append(missing, string(category))
	}
	return serverEvent{
		Code:  "plan_stored",
		Level: dbgen.RunEventLevelInfo,
		Message: fmt.Sprintf("stored %d new and %d revised test cases; %d were already approved",
			plan.Stored.Created, plan.Stored.Revised, len(plan.Review.Duplicates)),
		Data: map[string]any{
			"created":           plan.Stored.Created,
			"revised":           plan.Stored.Revised,
			"duplicates":        plan.Review.Duplicates,
			"skippedApproved":   plan.Stored.SkippedApproved,
			"missingCategories": missing,
		},
	}
}

// terminalStatusFor decides what a result frame means for the run row.
//
// The daemon reports whether IT finished, not whether the application under
// test passed: "completed" means the harness ran to the end, and whether that
// is a pass or a fail is decided here from the executions that actually
// landed. A daemon that decided the verdict itself could report a green run
// with a failed execution in it.
//
// It is a pure function so the mapping can be table-tested; finishRun is the
// two lines that write what it returns.
func terminalStatusFor(counted dbgen.Run, payload resultPayload) (dbgen.FinishRunParams, error) {
	params := dbgen.FinishRunParams{OrgID: counted.OrgID, ID: counted.ID}

	switch qaschema.RunResultPayloadStatus(payload.Status) {
	case qaschema.RunResultPayloadStatusCompleted:
		params.Status = dbgen.RunStatusPassed
		if counted.FailedCount > 0 || counted.ErrorCount > 0 {
			params.Status = dbgen.RunStatusFailed
		}
	case qaschema.RunResultPayloadStatusCancelled:
		params.Status = dbgen.RunStatusCancelled
	case qaschema.RunResultPayloadStatusFailed:
		// The harness itself broke. That is `error`, distinct from `failed`,
		// which means the application under test misbehaved.
		params.Status = dbgen.RunStatusError
		params.ErrorCode = string(qaschema.RunErrorCodeInternal)
		params.ErrorMessage = "the runtime could not finish this run"
		if payload.Error != nil {
			params.ErrorCode = string(payload.Error.Code)
			params.ErrorMessage = payload.Error.Message
		}
	default:
		return dbgen.FinishRunParams{}, &realtime.ProtocolError{Reason: "unknown run.result status " + payload.Status}
	}
	return params, nil
}

// finishRun writes the terminal status decided above.
func finishRun(ctx context.Context, q *dbgen.Queries, counted dbgen.Run, payload resultPayload) (dbgen.Run, error) {
	params, err := terminalStatusFor(counted, payload)
	if err != nil {
		return dbgen.Run{}, err
	}

	finished, err := q.FinishRun(ctx, params)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			// The run was already terminal — cancelled while the daemon was
			// finishing, or this is a redelivered result. Both are fine: the
			// guard on FinishRun is what makes them fine.
			return dbgen.Run{}, nil
		}
		return dbgen.Run{}, fmt.Errorf("finish run: %w", db.Classify(err))
	}
	return finished, nil
}

// ingestion carries the state one result frame needs while it is being
// written: the transaction, the run it belongs to, and the mapping from the
// daemon's run-local artifact handles to the rows they became.
type ingestion struct {
	q     *dbgen.Queries
	orgID uuid.UUID
	run   dbgen.Run
	// artifacts maps a daemon-assigned artifact id (a run-local handle, not a
	// database id) to the row it was stored as, so a step or a finding can
	// cite evidence the daemon numbered before anything was persisted.
	artifacts map[string]uuid.UUID
	// testCases and executions are loaded once, by ref, before anything is
	// written. A result frame carries up to 500 executions and as many
	// findings, each naming its case by ref; resolving those one at a time
	// would be a thousand statements inside the ingest transaction.
	testCases       map[string]dbgen.TestCase
	executionByCase map[uuid.UUID]dbgen.Execution
	// plan is what the planning half of this frame did, set only when the
	// frame carried a plan and it passed review. It is read after the
	// transaction commits, to narrate the outcome.
	plan   *planOutcome
	logger interface {
		WarnContext(ctx context.Context, msg string, args ...any)
	}
}

func (in *ingestion) apply(ctx context.Context, payload resultPayload) error {
	if err := in.applicationMap(ctx, payload.AppMap); err != nil {
		return err
	}
	if err := in.testPlan(ctx, payload.TestPlan); err != nil {
		return err
	}
	// After the plan, because a full run's cases were created by the block
	// above and the executions below have to resolve against them.
	if err := in.loadIndex(ctx, payload); err != nil {
		return err
	}
	// Run-level evidence first: a finding may cite the discovery HAR, which
	// belongs to no execution.
	for _, a := range payload.Artifacts {
		if _, err := in.artifact(ctx, a, uuid.NullUUID{}, uuid.NullUUID{}); err != nil {
			return err
		}
	}
	if err := in.executions(ctx, payload.Executions); err != nil {
		return err
	}
	return in.findings(ctx, payload.Findings)
}

// applicationMap upserts what discovery observed. Nothing is deleted: a page
// that disappeared is detected by its last_seen_run_id falling behind, and
// deleting it would orphan the element refs live test cases point at.
func (in *ingestion) applicationMap(ctx context.Context, appMap *qaschema.ApplicationMap) error {
	if appMap == nil {
		return nil
	}
	runRef := uuid.NullUUID{UUID: in.run.ID, Valid: true}

	for _, page := range appMap.Pages {
		stored, err := in.q.UpsertPage(ctx, dbgen.UpsertPageParams{
			OrgID:        in.orgID,
			ProjectID:    in.run.ProjectID,
			Ref:          page.ID,
			Path:         page.Path,
			Title:        page.Title,
			AuthRequired: page.AuthRequired != nil && *page.AuthRequired,
			RunID:        runRef,
		})
		if err != nil {
			return fmt.Errorf("upsert page %s: %w", page.ID, db.Classify(err))
		}

		for _, element := range page.Elements {
			locators, err := json.Marshal(element.Locators)
			if err != nil {
				return fmt.Errorf("marshal locators for element %s: %w", element.Ref, err)
			}
			label := ""
			if element.Label != nil {
				label = *element.Label
			}
			if _, err := in.q.UpsertElement(ctx, dbgen.UpsertElementParams{
				OrgID:    in.orgID,
				PageID:   stored.ID,
				Ref:      element.Ref,
				Kind:     string(element.Type),
				Label:    label,
				Locators: locators,
				RunID:    runRef,
			}); err != nil {
				return fmt.Errorf("upsert element %s: %w", element.Ref, db.Classify(err))
			}
		}
	}

	for _, workflow := range appMap.Workflows {
		steps, err := json.Marshal(workflow.Path)
		if err != nil {
			return fmt.Errorf("marshal path for workflow %s: %w", workflow.ID, err)
		}
		if _, err := in.q.UpsertWorkflow(ctx, dbgen.UpsertWorkflowParams{
			OrgID:           in.orgID,
			ProjectID:       in.run.ProjectID,
			Ref:             workflow.ID,
			Name:            workflow.Name,
			Path:            steps,
			ExpectedOutcome: workflow.ExpectedOutcome,
			RunID:           runRef,
		}); err != nil {
			return fmt.Errorf("upsert workflow %s: %w", workflow.ID, db.Classify(err))
		}
	}
	return nil
}

// testPlan reviews the plan and stores what survives.
//
// This is the boundary the whole planning feature turns on: an AI wrote the
// document, so the document is DATA until this backend has checked it against
// what this project actually has. testcase.ReviewPlan is where those checks
// live; the rule they enforce is all-or-nothing. A plan with one unresolvable
// element ref in it is rejected whole, and the ingest transaction is rolled
// back, so a rejected plan leaves not one row behind.
//
// Rejecting rather than dropping the bad case matters: a suite silently
// missing the case that would have caught the bug reads, to the person looking
// at it, exactly like a suite that was written that way on purpose.
func (in *ingestion) testPlan(ctx context.Context, document json.RawMessage) error {
	if len(document) == 0 {
		return nil
	}

	planCtx, err := testcase.LoadPlanContext(ctx, in.q, in.orgID, in.run.ProjectID)
	if err != nil {
		return err
	}
	review := testcase.ReviewPlan(document, planCtx)
	if !review.OK() {
		return &planRejected{RunID: in.run.ID, Review: review}
	}

	stored, err := testcase.PersistPlan(ctx, in.q, in.orgID, in.run.ProjectID,
		uuid.NullUUID{UUID: in.run.ID, Valid: true}, review.Accepted)
	if err != nil {
		return err
	}
	in.plan = &planOutcome{Review: review, Stored: stored}
	return nil
}

// planRejected is a plan that failed review. It is an error so that returning
// it rolls the ingest transaction back — that rollback IS the "no bad data
// reaches the database" guarantee, rather than a sequence of writes this
// function has to remember not to make.
type planRejected struct {
	RunID  uuid.UUID
	Review testcase.PlanReview
}

func (e *planRejected) Error() string {
	return fmt.Sprintf("the planner's output was rejected: %d problems, first: %s",
		len(e.Review.Rejections), e.Review.Rejections[0])
}

// Message is what the run row reports to whoever reads it next. It names the
// rules rather than quoting the model, because a rejection detail can quote
// model output, and model output on a hijacked run is page content.
func (e *planRejected) Message() string {
	counts := map[string]int{}
	var order []string
	for _, rejection := range e.Review.Rejections {
		if _, seen := counts[rejection.Rule]; !seen {
			order = append(order, rejection.Rule)
		}
		counts[rejection.Rule]++
	}
	parts := make([]string, 0, len(order))
	for _, rule := range order {
		parts = append(parts, fmt.Sprintf("%s x%d", rule, counts[rule]))
	}
	return "the test plan was rejected before anything was stored: " + strings.Join(parts, ", ")
}

// planOutcome is what a successful review did, kept so the run event can
// report it after the transaction commits.
type planOutcome struct {
	Review testcase.PlanReview
	Stored testcase.Stored
}

// loadIndex resolves every case ref the frame mentions, and every execution the
// run already has, in two statements.
//
// It runs after the test plan, because a full run's cases were created by that
// step and the executions below have to resolve against them.
func (in *ingestion) loadIndex(ctx context.Context, payload resultPayload) error {
	refs := make([]string, 0, len(payload.Executions)+len(payload.Findings))
	seen := map[string]struct{}{}
	for _, result := range payload.Executions {
		refs = appendUnique(refs, seen, result.TestCaseID)
	}
	for _, finding := range payload.Findings {
		refs = appendUnique(refs, seen, finding.TestCaseID)
	}
	if len(refs) == 0 {
		return nil
	}

	cases, err := in.q.ListTestCasesByRefs(ctx, dbgen.ListTestCasesByRefsParams{
		OrgID: in.orgID, ProjectID: in.run.ProjectID, Refs: refs,
	})
	if err != nil {
		return fmt.Errorf("look up test cases by ref: %w", db.Classify(err))
	}
	for _, testCase := range cases {
		in.testCases[testCase.Ref] = testCase
	}

	existing, err := in.q.ListExecutionsForRun(ctx, dbgen.ListExecutionsForRunParams{
		OrgID: in.orgID, RunID: in.run.ID,
	})
	if err != nil {
		return fmt.Errorf("look up the run's executions: %w", db.Classify(err))
	}
	for _, row := range existing {
		in.executionByCase[row.Execution.TestCaseID] = row.Execution
	}
	return nil
}

func appendUnique(refs []string, seen map[string]struct{}, ref string) []string {
	if ref == "" {
		return refs
	}
	if _, ok := seen[ref]; ok {
		return refs
	}
	seen[ref] = struct{}{}
	return append(refs, ref)
}

func (in *ingestion) executions(ctx context.Context, results []qaschema.ExecutionResult) error {
	for _, result := range results {
		testCase, ok := in.testCases[result.TestCaseID]
		if !ok {
			// A result for a case this project does not have. Dropping the
			// whole frame over it would lose every other execution in it, so
			// the run finishes with what did land and the gap is visible in
			// the counters.
			in.logger.WarnContext(ctx, "run result names an unknown test case",
				"run_id", in.run.ID, "test_case_ref", result.TestCaseID)
			continue
		}

		execution, err := in.execution(ctx, testCase, result)
		if err != nil {
			return err
		}
		if err := in.steps(ctx, execution.ID, testCase.ID, result); err != nil {
			return err
		}
		if err := in.assertions(ctx, execution.ID, result.Assertions); err != nil {
			return err
		}
	}
	return nil
}

// execution finds or creates the row for one case in this run, then closes it
// out. The row usually already exists — POST /runs pre-seeds the work list —
// but a `full` run plans its own cases, so a result can be the first time we
// hear of one.
func (in *ingestion) execution(ctx context.Context, testCase dbgen.TestCase, result qaschema.ExecutionResult) (dbgen.Execution, error) {
	existing, ok := in.executionByCase[testCase.ID]
	if !ok {
		// A `full` run plans its own cases, so a result can be the first time
		// we hear of one. Everything else was pre-seeded by POST /runs.
		created, err := in.q.CreateExecution(ctx, dbgen.CreateExecutionParams{
			OrgID: in.orgID, RunID: in.run.ID, TestCaseID: testCase.ID, TestCaseVersion: testCase.CurrentVersion,
		})
		if err != nil {
			return dbgen.Execution{}, fmt.Errorf("create execution for %s: %w", testCase.Ref, db.Classify(err))
		}
		existing = created
		in.executionByCase[testCase.ID] = created
	}

	finished, err := in.q.FinishExecution(ctx, dbgen.FinishExecutionParams{
		OrgID:        in.orgID,
		ID:           existing.ID,
		Result:       dbgen.ExecutionResult(result.Result),
		FailureClass: nullFailureClass(result.FailureClass),
		ErrorMessage: deref(result.Message),
		DurationMs:   nullInt4(result.DurationMs),
	})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			// Already terminal: a redelivered result. Keep the first verdict —
			// it is the one the report and any finding already reference.
			return existing, nil
		}
		return dbgen.Execution{}, fmt.Errorf("finish execution for %s: %w", testCase.Ref, db.Classify(err))
	}
	in.executionByCase[testCase.ID] = finished
	return finished, nil
}

func (in *ingestion) steps(ctx context.Context, executionID, testCaseID uuid.UUID, result qaschema.ExecutionResult) error {
	// An execution's own artifacts are attributed to it and to its case, which
	// is what lets the report show evidence per failing step.
	scope := uuid.NullUUID{UUID: executionID, Valid: true}
	caseRef := uuid.NullUUID{UUID: testCaseID, Valid: true}
	for _, a := range result.Artifacts {
		if _, err := in.artifact(ctx, a, scope, caseRef); err != nil {
			return err
		}
	}

	for _, step := range result.Steps {
		target, err := json.Marshal(map[string]any{"resolvedLocator": step.ResolvedLocator})
		if err != nil {
			return fmt.Errorf("marshal step target: %w", err)
		}
		if _, err := in.q.UpsertExecutionStep(ctx, dbgen.UpsertExecutionStepParams{
			OrgID:        in.orgID,
			ExecutionID:  executionID,
			StepIndex:    boundedIndex(step.Index),
			Action:       string(step.Action),
			Target:       target,
			Result:       dbgen.ExecutionResult(step.Status),
			Unstable:     step.UnstableTarget != nil && *step.UnstableTarget,
			ErrorMessage: deref(step.Message),
			DurationMs:   nullInt4(step.DurationMs),
			StartedAt:    parseOptionalTime(step.StartedAt),
			FinishedAt:   parseOptionalTime(step.EndedAt),
		}); err != nil {
			return fmt.Errorf("upsert step %d: %w", step.Index, db.Classify(err))
		}
	}
	return nil
}

func (in *ingestion) assertions(ctx context.Context, executionID uuid.UUID, assertions []qaschema.AssertionResult) error {
	for _, assertion := range assertions {
		if _, err := in.q.UpsertExecutionAssertion(ctx, dbgen.UpsertExecutionAssertionParams{
			OrgID:          in.orgID,
			ExecutionID:    executionID,
			AssertionIndex: boundedIndex(assertion.Index),
			Type:           string(assertion.Type),
			Status:         dbgen.ExecutionResult(assertion.Status),
			// Expected and Actual are lifted off the page under test. They are
			// stored as data and rendered as data; nothing switches on them.
			Expected: deref(assertion.Expected),
			Actual:   deref(assertion.Actual),
			Message:  deref(assertion.Message),
		}); err != nil {
			return fmt.Errorf("upsert assertion %d: %w", assertion.Index, db.Classify(err))
		}
	}
	return nil
}

// artifact records one uploaded object.
//
// The key is re-checked against this run's own prefix before the insert. The
// artifacts_storage_key_layout CHECK would reject a bad one anyway, but a
// constraint violation is a 500 with a constraint name in the log, whereas
// this is a named protocol error the daemon's author can act on.
func (in *ingestion) artifact(ctx context.Context, a qaschema.Artifact, executionID, testCaseID uuid.NullUUID) (uuid.UUID, error) {
	prefix := artifact.KeyPrefix(in.orgID, in.run.ID, runDay(in.run))
	if err := artifact.CheckKeyUnderPrefix(prefix, a.Key); err != nil {
		return uuid.Nil, &realtime.ProtocolError{Reason: "artifact key is outside this run's prefix"}
	}

	digest, err := decodeDigest(a.Sha256)
	if err != nil {
		return uuid.Nil, &realtime.ProtocolError{Reason: "artifact sha256 is not hex"}
	}

	stored, err := in.q.UpsertArtifact(ctx, dbgen.UpsertArtifactParams{
		OrgID:       in.orgID,
		RunID:       in.run.ID,
		ExecutionID: executionID,
		TestCaseID:  testCaseID,
		Kind:        dbgen.ArtifactKind(a.Kind),
		Name:        path.Base(a.Key),
		StorageKey:  a.Key,
		ContentType: contentTypeOr(a.ContentType),
		SizeBytes:   nullInt8(a.SizeBytes),
		Sha256:      digest,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert artifact %s: %w", a.Key, db.Classify(err))
	}

	in.artifacts[a.ID] = stored.ID
	return stored.ID, nil
}

func (in *ingestion) findings(ctx context.Context, findings []qaschema.Finding) error {
	for _, finding := range findings {
		executionID, testCaseID := in.locateFinding(finding)

		stored, err := in.q.UpsertFinding(ctx, dbgen.UpsertFindingParams{
			OrgID:              in.orgID,
			RunID:              in.run.ID,
			ExecutionID:        executionID,
			TestCaseID:         testCaseID,
			StepIndex:          nullInt4(finding.StepIndex),
			FailureClass:       dbgen.FailureClass(finding.FailureClass),
			Summary:            truncate(deref(finding.Summary), 500),
			RootCause:          finding.RootCause,
			Confidence:         finding.Confidence,
			SuggestedFix:       deref(finding.SuggestedFix),
			AnalyzedByProvider: nullProvider(finding.AnalyzedBy),
			AnalyzedByVersion:  analyzedByVersion(finding.AnalyzedBy),
		})
		if err != nil {
			return fmt.Errorf("upsert finding for %s: %w", finding.TestCaseID, db.Classify(err))
		}

		// Evidence ids are the daemon's run-local handles. Only the ones that
		// actually became rows are linked; a handle naming nothing we stored
		// would otherwise be a dangling reference the report cannot render.
		evidence := make([]uuid.UUID, 0, len(finding.Evidence))
		for _, handle := range finding.Evidence {
			if id, ok := in.artifacts[handle]; ok {
				evidence = append(evidence, id)
			}
		}
		if len(evidence) == 0 {
			continue
		}
		if _, err := in.q.LinkFindingEvidence(ctx, dbgen.LinkFindingEvidenceParams{
			OrgID: in.orgID, FindingID: stored.ID, ArtifactIds: evidence,
		}); err != nil {
			return fmt.Errorf("link finding evidence: %w", db.Classify(err))
		}
	}
	return nil
}

// locateFinding resolves the case a finding blames to the execution it belongs
// to, from the index loaded up front. A finding about a case with no execution
// in this run is still stored — it is a conclusion about the run — but with no
// execution attached.
func (in *ingestion) locateFinding(finding qaschema.Finding) (uuid.NullUUID, uuid.NullUUID) {
	testCase, ok := in.testCases[finding.TestCaseID]
	if !ok {
		return uuid.NullUUID{}, uuid.NullUUID{}
	}
	execution, ok := in.executionByCase[testCase.ID]
	if !ok {
		return uuid.NullUUID{}, uuid.NullUUID{UUID: testCase.ID, Valid: true}
	}
	return uuid.NullUUID{UUID: execution.ID, Valid: true},
		uuid.NullUUID{UUID: testCase.ID, Valid: true}
}

// --- small conversions ----------------------------------------------------

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// boundedIndex narrows a contract index to the integer width the column uses.
//
// test-case@1 caps steps and assertions at a few dozen each, and the columns
// are integer, so the only way to reach the clamp is a daemon sending an index
// the contract does not allow. Clamping keeps that out of the database instead
// of wrapping it into a negative index the CHECK would reject with a
// constraint name in the log.
func boundedIndex(v int) int32 {
	switch {
	case v < 0:
		return 0
	case v > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(v)
	}
}

func contentTypeOr(ct *string) string {
	if ct == nil || *ct == "" {
		return "application/octet-stream"
	}
	return *ct
}

func nullInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: boundedIndex(*v), Valid: true}
}

func nullInt8(v *int) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: int64(*v), Valid: true}
}

func nullFailureClass(fc *qaschema.FailureClass) dbgen.NullFailureClass {
	if fc == nil {
		return dbgen.NullFailureClass{}
	}
	return dbgen.NullFailureClass{FailureClass: dbgen.FailureClass(*fc), Valid: true}
}

func nullProvider(by *qaschema.AnalyzedBy) dbgen.NullAgentProvider {
	if by == nil {
		return dbgen.NullAgentProvider{}
	}
	return dbgen.NullAgentProvider{AgentProvider: dbgen.AgentProvider(by.Provider), Valid: true}
}

func analyzedByVersion(by *qaschema.AnalyzedBy) string {
	if by == nil || by.Version == nil {
		return ""
	}
	return *by.Version
}

func decodeDigest(hexDigest *string) ([]byte, error) {
	if hexDigest == nil || *hexDigest == "" {
		return nil, nil
	}
	digest, err := hex.DecodeString(*hexDigest)
	if err != nil || len(digest) != 32 {
		return nil, fmt.Errorf("sha256 must be 64 hex characters")
	}
	return digest, nil
}

func parseOptionalTime(raw *string) pgtype.Timestamptz {
	if raw == nil || *raw == "" {
		return pgtype.Timestamptz{}
	}
	parsed, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: parsed.UTC(), Valid: true}
}
