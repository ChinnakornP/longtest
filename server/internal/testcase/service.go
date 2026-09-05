package testcase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
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
