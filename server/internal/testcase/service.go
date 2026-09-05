package testcase

import (
	"bytes"
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
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Service is the domain layer for test cases.
type Service struct {
	store auth.Store
}

// NewService returns the test case service.
func NewService(store auth.Store) *Service { return &Service{store: store} }

// Listed is one page of test cases plus the total.
type Listed struct {
	TestCases []dbgen.TestCase
	Total     int64
}

// ListForProject returns a page of a project's cases, optionally filtered by
// review status.
func (s *Service) ListForProject(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID, status string, page httpx.Page) (Listed, error) {
	filter, err := parseStatusFilter(status)
	if err != nil {
		return Listed{}, err
	}

	cases, err := s.store.ListTestCases(ctx, dbgen.ListTestCasesParams{
		OrgID: scope.OrgID(), ProjectID: projectID, Status: filter, Limit: page.Limit, Offset: page.Offset,
	})
	if err != nil {
		return Listed{}, fmt.Errorf("list test cases: %w", db.Classify(err))
	}
	total, err := s.store.CountTestCases(ctx, dbgen.CountTestCasesParams{
		OrgID: scope.OrgID(), ProjectID: projectID, Status: filter,
	})
	if err != nil {
		return Listed{}, fmt.Errorf("count test cases: %w", db.Classify(err))
	}
	return Listed{TestCases: cases, Total: total}, nil
}

// Get returns one case. Another organization's case is a 404.
func (s *Service) Get(ctx context.Context, scope auth.OrgScope, id uuid.UUID) (dbgen.TestCase, error) {
	found, err := s.store.GetTestCase(ctx, dbgen.GetTestCaseParams{OrgID: scope.OrgID(), ID: id})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return dbgen.TestCase{}, httpx.NotFound("test case not found")
		}
		return dbgen.TestCase{}, fmt.Errorf("look up test case: %w", db.Classify(err))
	}
	return found, nil
}

// SetStatus moves a case through review.
//
// The transitions are deliberately not a free-for-all: a case is drafted,
// approved, and archived, and an archived case is reinstated as a draft rather
// than jumping straight back into the regression suite a reviewer never
// re-read.
func (s *Service) SetStatus(ctx context.Context, scope auth.OrgScope, id uuid.UUID, status string) (dbgen.TestCase, error) {
	target, err := parseStatus(status)
	if err != nil {
		return dbgen.TestCase{}, err
	}

	current, err := s.Get(ctx, scope, id)
	if err != nil {
		return dbgen.TestCase{}, err
	}
	if current.Status == target {
		return current, nil
	}
	if !allowedTransition(current.Status, target) {
		return dbgen.TestCase{}, httpx.Conflict("a %s test case cannot become %s", current.Status, target)
	}

	updated, err := s.store.SetTestCaseStatus(ctx, dbgen.SetTestCaseStatusParams{
		OrgID: scope.OrgID(), ID: id, Status: target,
	})
	if err != nil {
		return dbgen.TestCase{}, fmt.Errorf("set test case status: %w", db.Classify(err))
	}
	return updated, nil
}

// allowedTransition encodes the review lifecycle.
func allowedTransition(from, to dbgen.TestCaseStatus) bool {
	switch from {
	case dbgen.TestCaseStatusDraft:
		return to == dbgen.TestCaseStatusApproved || to == dbgen.TestCaseStatusArchived
	case dbgen.TestCaseStatusApproved:
		return to == dbgen.TestCaseStatusArchived || to == dbgen.TestCaseStatusDraft
	case dbgen.TestCaseStatusArchived:
		return to == dbgen.TestCaseStatusDraft
	default:
		return false
	}
}

func parseStatus(raw string) (dbgen.TestCaseStatus, error) {
	switch dbgen.TestCaseStatus(raw) {
	case dbgen.TestCaseStatusDraft, dbgen.TestCaseStatusApproved, dbgen.TestCaseStatusArchived:
		return dbgen.TestCaseStatus(raw), nil
	default:
		return "", httpx.InvalidField("status", "must be one of draft, approved, archived")
	}
}

func parseStatusFilter(raw string) (dbgen.NullTestCaseStatus, error) {
	if raw == "" {
		return dbgen.NullTestCaseStatus{}, nil
	}
	status, err := parseStatus(raw)
	if err != nil {
		return dbgen.NullTestCaseStatus{}, httpx.BadRequest("status must be one of draft, approved, archived")
	}
	return dbgen.NullTestCaseStatus{TestCaseStatus: status, Valid: true}, nil
}

// View is a test case as the API renders it.
//
// Payload is passed through as the raw test-case@1 document. It is never
// decoded and re-encoded here: a client validating it against the contract has
// to see the bytes the planner wrote, not a Go struct's idea of them.
type View struct {
	ID        uuid.UUID       `json:"id"`
	ProjectID uuid.UUID       `json:"projectId"`
	Ref       string          `json:"ref"`
	Name      string          `json:"name"`
	Priority  string          `json:"priority"`
	Category  string          `json:"category"`
	Status    string          `json:"status"`
	Version   int32           `json:"version"`
	SourceRun *uuid.UUID      `json:"sourceRunId,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// NewView renders a test case row.
func NewView(tc dbgen.TestCase) View {
	view := View{
		ID:        tc.ID,
		ProjectID: tc.ProjectID,
		Ref:       tc.Ref,
		Name:      tc.Name,
		Priority:  string(tc.Priority),
		Category:  string(tc.Category),
		Status:    string(tc.Status),
		Version:   tc.CurrentVersion,
		Payload:   tc.Payload,
		CreatedAt: tc.CreatedAt.Time.UTC(),
		UpdatedAt: tc.UpdatedAt.Time.UTC(),
	}
	if tc.SourceRunID.Valid {
		id := tc.SourceRunID.UUID
		view.SourceRun = &id
	}
	return view
}

// ListResponse is the body of every test-case list. It lives here rather than
// in the handler so internal/project can return the same shape.
type ListResponse struct {
	TestCases []View `json:"testCases"`
	Total     int64  `json:"total"`
}

// --- payload editing and version history ----------------------------------

// testCaseSchemaID is the contract a stored payload has to satisfy. Named here
// rather than inlined so a bump to test-case@2 is one edit with a compiler
// error at every reader, exactly like testPlanSchemaID in plan.go.
const testCaseSchemaID = "test-case@1"

// maxSchemaProblems bounds how many contract failures a 422 carries. A
// deliberately malformed document can fail hundreds of keywords, and an error
// body larger than the request that caused it is a way to make this process
// allocate. The count that was dropped is reported rather than hidden.
const maxSchemaProblems = 50

// PayloadEdit is one reviewer's edit of a case's executable definition.
type PayloadEdit struct {
	// BaseVersion is the current_version the editor loaded. It is the
	// optimistic lock this endpoint turns on: a mismatch is refused, never
	// resolved as a last write over an edit somebody else already saved.
	BaseVersion int32
	// Payload is the whole test-case@1 document. It is stored verbatim —
	// re-encoding it here would reorder its keys and drop whatever a newer
	// minor version of the contract added.
	Payload json.RawMessage
}

// UpdatePayload replaces a draft case's executable definition.
//
// Only a draft is editable. An approved case is a regression contract: editing
// its content under a new version while it stays approved would silently change
// what every past result meant, so the reviewer moves it back to draft first,
// edits, and re-approves — three deliberate steps instead of one invisible one.
//
// The version bump and the history row are written by the trigger on
// test_cases, not here, so a payload that is byte-different but jsonb-equal to
// the stored one changes nothing at all. That is what makes a retry of the same
// request safe: it re-reads the same version and writes no second history row.
func (s *Service) UpdatePayload(ctx context.Context, scope auth.OrgScope, id uuid.UUID, edit PayloadEdit) (dbgen.TestCase, error) {
	// The contract check runs before the row is read, and therefore before any
	// lock is taken: it is pure CPU over the caller's own bytes, it tells the
	// caller nothing about whether the case exists, and holding a row lock
	// while validating a 1 MiB document would block the other reviewer for no
	// reason.
	document, err := validatePayload(edit)
	if err != nil {
		return dbgen.TestCase{}, err
	}

	var updated dbgen.TestCase
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		// FOR UPDATE, not a bare read: baseVersion is only an optimistic lock
		// if the row cannot change between the check and the write. Two
		// reviewers saving against version 3 must not both pass this check.
		current, err := q.GetTestCaseForUpdate(ctx, dbgen.GetTestCaseForUpdateParams{
			OrgID: scope.OrgID(), ID: id,
		})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				return httpx.NotFound("test case not found")
			}
			return fmt.Errorf("look up test case: %w", db.Classify(err))
		}

		if current.Status != dbgen.TestCaseStatusDraft {
			return httpx.Conflict(
				"a %s test case cannot be edited; move it back to draft first", current.Status)
		}
		if current.CurrentVersion != edit.BaseVersion {
			return httpx.Conflict(
				"this test case is at version %d, not %d; reload it and reapply your edit",
				current.CurrentVersion, edit.BaseVersion)
		}
		// The id is what makes a result comparable run over run, and the row's
		// ref is that id. Renaming it would silently orphan every past
		// execution of this case, so it is refused rather than ignored.
		if document.ID != current.Ref {
			return httpx.InvalidField("payload.id",
				fmt.Sprintf("is immutable: this test case is %q", current.Ref))
		}

		// name, priority and category are the row's projection of the document
		// — the list endpoint orders and filters on them — so they move with
		// it. Leaving them behind would show a reviewer the old name in the
		// list and the new one in the case.
		updated, err = q.UpdateTestCasePayload(ctx, dbgen.UpdateTestCasePayloadParams{
			OrgID:    scope.OrgID(),
			ID:       id,
			Name:     pgtype.Text{String: document.Name, Valid: true},
			Priority: dbgen.NullTestPriority{TestPriority: dbgen.TestPriority(document.Priority), Valid: true},
			Category: dbgen.NullTestCategory{TestCategory: dbgen.TestCategory(document.Category), Valid: true},
			Payload:  edit.Payload,
		})
		if err != nil {
			return fmt.Errorf("update test case payload: %w", db.Classify(err))
		}
		return nil
	})
	if err != nil {
		return dbgen.TestCase{}, err
	}
	return updated, nil
}

// validatePayload checks an edit against the contract and returns the fields
// the row mirrors from it.
//
// It validates with qaschema, the same validator the planner ingest runs, and
// not with a hand-written enum check: a second implementation of the contract
// is a second thing to keep in step, and the one that drifts is always the one
// nobody is looking at.
func validatePayload(edit PayloadEdit) (qaschema.TestCase, error) {
	if edit.BaseVersion < 1 {
		return qaschema.TestCase{}, httpx.InvalidField("baseVersion",
			"is required, and is the version of the test case you loaded")
	}
	if len(bytes.TrimSpace(edit.Payload)) == 0 {
		return qaschema.TestCase{}, httpx.InvalidField("payload", "is required")
	}

	result, err := qaschema.ValidateJSON(testCaseSchemaID, edit.Payload)
	if err != nil {
		// The contract itself is missing or unusable. That is a bug in this
		// build, not in the request, so it stays a 500.
		return qaschema.TestCase{}, fmt.Errorf("validate against %s: %w", testCaseSchemaID, err)
	}
	if !result.Valid {
		return qaschema.TestCase{}, schemaViolation(result.Errors)
	}

	var document qaschema.TestCase
	if err := json.Unmarshal(edit.Payload, &document); err != nil {
		// Unreachable through a document that just validated; checked because
		// everything below reads these fields, and a mispairing would store
		// one case under another one's name.
		return qaschema.TestCase{}, httpx.InvalidField("payload", "is not a decodable test case")
	}
	// The document's own `version` is the contract version (const 1), which
	// the schema has just checked. The row's version is server-assigned by the
	// trigger, and nothing the client sends can set it.
	return document, nil
}

// schemaViolation renders a contract failure as a 422 whose details locate
// every bad field by JSON Pointer, so an editor can highlight them all at once
// instead of surfacing them one save at a time.
func schemaViolation(problems []qaschema.ValidationError) *httpx.Error {
	details := map[string]any{"schema": testCaseSchemaID}
	if len(problems) > maxSchemaProblems {
		details["errors"] = problems[:maxSchemaProblems]
		details["truncated"] = len(problems) - maxSchemaProblems
	} else {
		details["errors"] = problems
	}

	invalid := httpx.Invalid(nil)
	invalid.Message = "the payload is not a valid " + testCaseSchemaID + " document"
	return invalid.WithDetails(details)
}

// VersionHistory is one bounded page of a case's history plus the total it was
// taken from.
type VersionHistory struct {
	Versions []dbgen.TestCaseVersion
	Total    int64
}

// ListVersions returns a case's payload history, newest first.
//
// Every version the case has ever had is here, including the current one:
// version[0] is what the case holds now, and the diff a reviewer reads is
// rendered from any two of these payloads.
func (s *Service) ListVersions(ctx context.Context, scope auth.OrgScope, id uuid.UUID, limit int32) (VersionHistory, error) {
	// The case itself first. test_case_versions is org-scoped too, so skipping
	// this would make another organization's case read as a case with no
	// history rather than as a case that is not there.
	if _, err := s.Get(ctx, scope, id); err != nil {
		return VersionHistory{}, err
	}

	versions, err := s.store.ListTestCaseVersions(ctx, dbgen.ListTestCaseVersionsParams{
		OrgID: scope.OrgID(), TestCaseID: id, Limit: limit,
	})
	if err != nil {
		return VersionHistory{}, fmt.Errorf("list test case versions: %w", db.Classify(err))
	}
	total, err := s.store.CountTestCaseVersions(ctx, dbgen.CountTestCaseVersionsParams{
		OrgID: scope.OrgID(), TestCaseID: id,
	})
	if err != nil {
		return VersionHistory{}, fmt.Errorf("count test case versions: %w", db.Classify(err))
	}
	return VersionHistory{Versions: versions, Total: total}, nil
}

// VersionView is one historical payload as the API renders it.
//
// It carries no id of its own: a version is addressed by its case and its
// number, and exposing the row id would invite a client to build a URL this API
// does not serve.
type VersionView struct {
	Version int32 `json:"version"`
	// Status is the review state the case was in when this payload was
	// written, which is how a reader tells "this is what we approved" from
	// "this is a draft somebody was still working on".
	Status    string          `json:"status"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// NewVersionView renders one history row.
func NewVersionView(v dbgen.TestCaseVersion) VersionView {
	return VersionView{
		Version:   v.Version,
		Status:    string(v.Status),
		Payload:   v.Payload,
		CreatedAt: v.CreatedAt.Time.UTC(),
	}
}

// VersionListResponse is the body of the version-history endpoint.
type VersionListResponse struct {
	Versions []VersionView `json:"versions"`
	Total    int64         `json:"total"`
}

// NewVersionListResponse renders a history page.
func NewVersionListResponse(history VersionHistory) VersionListResponse {
	views := make([]VersionView, 0, len(history.Versions))
	for _, version := range history.Versions {
		views = append(views, NewVersionView(version))
	}
	return VersionListResponse{Versions: views, Total: history.Total}
}
