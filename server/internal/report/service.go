package report

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/run"
)

// Service assembles run reports.
type Service struct {
	store     auth.Store
	runs      *run.Service
	artifacts *artifact.Service
}

// NewService returns the report service.
func NewService(store auth.Store, runs *run.Service, artifacts *artifact.Service) *Service {
	if artifacts == nil {
		artifacts = artifact.Disabled()
	}
	return &Service{store: store, runs: runs, artifacts: artifacts}
}

// ArtifactView is one piece of evidence, with a short-lived download URL when
// object storage is configured.
type ArtifactView struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	ContentType string    `json:"contentType"`
	SizeBytes   *int64    `json:"sizeBytes,omitempty"`
	Sha256      string    `json:"sha256,omitempty"`
	// URL is a presigned GET, minted per request and valid for minutes. It is
	// absent on a deployment with no object storage rather than rendered as an
	// unusable link.
	URL       string     `json:"url,omitempty"`
	ExpiresAt *time.Time `json:"urlExpiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// StepView is one executed step.
type StepView struct {
	Index int32 `json:"index"`
	// Action is the step-action name from test-case@1. It is text rather than
	// an enum in the schema and here, so adding one is a contract change and
	// not a migration.
	Action string `json:"action"`
	Result string `json:"result"`
	// Unstable marks a step that used a raw locator instead of an application
	// map ref. Those are expected to rot and are reported separately.
	Unstable     bool            `json:"unstable"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	DurationMs   *int32          `json:"durationMs,omitempty"`
	Target       json.RawMessage `json:"target,omitempty"`
}

// AssertionView is one checked assertion. Expected and Actual were lifted off
// the application under test: they are data, rendered as data.
type AssertionView struct {
	Index    int32  `json:"index"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Message  string `json:"message,omitempty"`
}

// ExecutionView is one test case's run.
type ExecutionView struct {
	ID           uuid.UUID       `json:"id"`
	TestCaseID   uuid.UUID       `json:"testCaseId"`
	TestCaseRef  string          `json:"testCaseRef"`
	Name         string          `json:"name"`
	Priority     string          `json:"priority"`
	Category     string          `json:"category"`
	Version      int32           `json:"testCaseVersion"`
	Result       string          `json:"result"`
	FailureClass string          `json:"failureClass,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	DurationMs   *int32          `json:"durationMs,omitempty"`
	StartedAt    *time.Time      `json:"startedAt,omitempty"`
	FinishedAt   *time.Time      `json:"finishedAt,omitempty"`
	Steps        []StepView      `json:"steps"`
	Assertions   []AssertionView `json:"assertions"`
	Artifacts    []ArtifactView  `json:"artifacts"`
}

// FindingView is one conclusion the analyst drew, with the evidence it cited.
type FindingView struct {
	ID           uuid.UUID      `json:"id"`
	ExecutionID  *uuid.UUID     `json:"executionId,omitempty"`
	TestCaseID   *uuid.UUID     `json:"testCaseId,omitempty"`
	StepIndex    *int32         `json:"stepIndex,omitempty"`
	FailureClass string         `json:"failureClass"`
	Summary      string         `json:"summary"`
	RootCause    string         `json:"rootCause"`
	Confidence   float64        `json:"confidence"`
	SuggestedFix string         `json:"suggestedFix,omitempty"`
	AnalyzedBy   *AnalyzedBy    `json:"analyzedBy,omitempty"`
	Evidence     []ArtifactView `json:"evidence"`
	CreatedAt    time.Time      `json:"createdAt"`
}

// AnalyzedBy attributes a finding to the AI CLI that produced it. Dropping the
// attribution would make comparing providers impossible after the fact.
type AnalyzedBy struct {
	Provider string `json:"provider"`
	Version  string `json:"version,omitempty"`
}

// Report is GET /api/v1/runs/{id}/report.
type Report struct {
	Run        run.View        `json:"run"`
	Executions []ExecutionView `json:"executions"`
	Findings   []FindingView   `json:"findings"`
	// Artifacts are the run-level ones: evidence that belongs to no single
	// execution, such as a discovery HAR.
	Artifacts []ArtifactView `json:"artifacts"`
}

// Get assembles the report for one run.
func (s *Service) Get(ctx context.Context, scope auth.OrgScope, runID uuid.UUID) (Report, error) {
	// Establishes existence and tenancy before anything else is read.
	current, err := s.runs.Get(ctx, scope, runID)
	if err != nil {
		return Report{}, err
	}

	executions, err := s.store.ListExecutionsForRun(ctx, dbgen.ListExecutionsForRunParams{OrgID: scope.OrgID(), RunID: runID})
	if err != nil {
		return Report{}, fmt.Errorf("list executions: %w", db.Classify(err))
	}
	steps, err := s.store.ListExecutionStepsForRun(ctx, dbgen.ListExecutionStepsForRunParams{OrgID: scope.OrgID(), RunID: runID})
	if err != nil {
		return Report{}, fmt.Errorf("list execution steps: %w", db.Classify(err))
	}
	assertions, err := s.store.ListExecutionAssertionsForRun(ctx, dbgen.ListExecutionAssertionsForRunParams{OrgID: scope.OrgID(), RunID: runID})
	if err != nil {
		return Report{}, fmt.Errorf("list execution assertions: %w", db.Classify(err))
	}
	artifacts, err := s.store.ListArtifactsForRun(ctx, dbgen.ListArtifactsForRunParams{OrgID: scope.OrgID(), RunID: runID})
	if err != nil {
		return Report{}, fmt.Errorf("list artifacts: %w", db.Classify(err))
	}
	findings, err := s.store.ListFindingsForRun(ctx, dbgen.ListFindingsForRunParams{OrgID: scope.OrgID(), RunID: runID})
	if err != nil {
		return Report{}, fmt.Errorf("list findings: %w", db.Classify(err))
	}
	evidence, err := s.store.ListFindingEvidenceForRun(ctx, dbgen.ListFindingEvidenceForRunParams{OrgID: scope.OrgID(), RunID: runID})
	if err != nil {
		return Report{}, fmt.Errorf("list finding evidence: %w", db.Classify(err))
	}

	logger := httpx.LoggerFrom(ctx)
	stepsByExecution := map[uuid.UUID][]StepView{}
	for _, step := range steps {
		stepsByExecution[step.ExecutionID] = append(stepsByExecution[step.ExecutionID], StepView{
			Index:        step.StepIndex,
			Action:       step.Action,
			Result:       string(step.Result),
			Unstable:     step.Unstable,
			ErrorMessage: step.ErrorMessage,
			DurationMs:   optionalInt32(step.DurationMs.Int32, step.DurationMs.Valid),
			Target:       step.Target,
		})
	}

	assertionsByExecution := map[uuid.UUID][]AssertionView{}
	for _, a := range assertions {
		assertionsByExecution[a.ExecutionID] = append(assertionsByExecution[a.ExecutionID], AssertionView{
			Index: a.AssertionIndex, Type: a.Type, Status: string(a.Status),
			Expected: a.Expected, Actual: a.Actual, Message: a.Message,
		})
	}

	artifactsByExecution := map[uuid.UUID][]ArtifactView{}
	runLevel := make([]ArtifactView, 0)
	for _, a := range artifacts {
		view := s.artifactView(ctx, scope.OrgID(), a, logger)
		if a.ExecutionID.Valid {
			artifactsByExecution[a.ExecutionID.UUID] = append(artifactsByExecution[a.ExecutionID.UUID], view)
			continue
		}
		runLevel = append(runLevel, view)
	}

	evidenceByFinding := map[uuid.UUID][]ArtifactView{}
	for _, row := range evidence {
		evidenceByFinding[row.FindingID] = append(evidenceByFinding[row.FindingID],
			s.artifactView(ctx, scope.OrgID(), row.Artifact, logger))
	}

	report := Report{
		Run:        run.NewView(current),
		Executions: make([]ExecutionView, 0, len(executions)),
		Findings:   make([]FindingView, 0, len(findings)),
		Artifacts:  runLevel,
	}

	for _, row := range executions {
		e := row.Execution
		view := ExecutionView{
			ID:           e.ID,
			TestCaseID:   e.TestCaseID,
			TestCaseRef:  row.Ref,
			Name:         row.Name,
			Priority:     string(row.Priority),
			Category:     string(row.Category),
			Version:      e.TestCaseVersion,
			Result:       string(e.Result),
			ErrorMessage: e.ErrorMessage,
			DurationMs:   optionalInt32(e.DurationMs.Int32, e.DurationMs.Valid),
			StartedAt:    optionalTime(e.StartedAt.Time, e.StartedAt.Valid),
			FinishedAt:   optionalTime(e.FinishedAt.Time, e.FinishedAt.Valid),
			Steps:        orEmptySteps(stepsByExecution[e.ID]),
			Assertions:   orEmptyAssertions(assertionsByExecution[e.ID]),
			Artifacts:    orEmptyArtifacts(artifactsByExecution[e.ID]),
		}
		if e.FailureClass.Valid {
			view.FailureClass = string(e.FailureClass.FailureClass)
		}
		report.Executions = append(report.Executions, view)
	}

	for _, f := range findings {
		view := FindingView{
			ID:           f.ID,
			StepIndex:    optionalInt32(f.StepIndex.Int32, f.StepIndex.Valid),
			FailureClass: string(f.FailureClass),
			Summary:      f.Summary,
			RootCause:    f.RootCause,
			Confidence:   f.Confidence,
			SuggestedFix: f.SuggestedFix,
			Evidence:     orEmptyArtifacts(evidenceByFinding[f.ID]),
			CreatedAt:    f.CreatedAt.Time.UTC(),
		}
		if f.ExecutionID.Valid {
			id := f.ExecutionID.UUID
			view.ExecutionID = &id
		}
		if f.TestCaseID.Valid {
			id := f.TestCaseID.UUID
			view.TestCaseID = &id
		}
		if f.AnalyzedByProvider.Valid {
			view.AnalyzedBy = &AnalyzedBy{
				Provider: string(f.AnalyzedByProvider.AgentProvider),
				Version:  f.AnalyzedByVersion,
			}
		}
		report.Findings = append(report.Findings, view)
	}

	return report, nil
}

// artifactView renders one artifact, attaching a presigned download URL when
// storage is configured. A signing failure is logged and the artifact is still
// listed: its metadata is useful on its own, and a whole report should not
// disappear because one URL could not be minted.
func (s *Service) artifactView(ctx context.Context, orgID uuid.UUID, a dbgen.Artifact, logger interface {
	WarnContext(ctx context.Context, msg string, args ...any)
}) ArtifactView {
	view := ArtifactView{
		ID:          a.ID,
		Kind:        string(a.Kind),
		Name:        a.Name,
		ContentType: a.ContentType,
		CreatedAt:   a.CreatedAt.Time.UTC(),
	}
	if a.SizeBytes.Valid {
		size := a.SizeBytes.Int64
		view.SizeBytes = &size
	}
	if len(a.Sha256) > 0 {
		view.Sha256 = fmt.Sprintf("%x", a.Sha256)
	}
	if !s.artifacts.Enabled() {
		return view
	}

	signed, err := s.artifacts.GetURL(orgID, a.StorageKey)
	if err != nil {
		logger.WarnContext(ctx, "could not presign an artifact download", "err", err, "artifact_id", a.ID)
		return view
	}
	view.URL = signed.URL
	expires := signed.ExpiresAt
	view.ExpiresAt = &expires
	return view
}

func optionalInt32(v int32, valid bool) *int32 {
	if !valid {
		return nil
	}
	return &v
}

func optionalTime(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// The or-empty helpers keep a JSON body's arrays as [] rather than null, so a
// client never has to null-check a collection.
func orEmptySteps(v []StepView) []StepView {
	if v == nil {
		return []StepView{}
	}
	return v
}

func orEmptyAssertions(v []AssertionView) []AssertionView {
	if v == nil {
		return []AssertionView{}
	}
	return v
}

func orEmptyArtifacts(v []ArtifactView) []ArtifactView {
	if v == nil {
		return []ArtifactView{}
	}
	return v
}
