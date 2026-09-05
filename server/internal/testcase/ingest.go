package testcase

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Planner ingest: reading the context a plan is judged against, and writing
// the cases that survived.
//
// Both halves take a *dbgen.Queries rather than the service's own store,
// because both run inside the caller's transaction. The run ingest commits a
// result frame — application map, plan, executions, artifacts, findings — as
// one unit, and a plan that was reviewed against a snapshot taken outside that
// transaction would be a plan judged against a project that has since changed.

// PlanQueries is the slice of the generated query set a plan review needs. It
// is declared here rather than taking *dbgen.Queries so the dependency is
// visible and a test can supply three functions instead of a database.
type PlanQueries interface {
	ListElementRefsForProject(ctx context.Context, arg dbgen.ListElementRefsForProjectParams) ([]string, error)
	ListProjectFixtureNames(ctx context.Context, arg dbgen.ListProjectFixtureNamesParams) ([]string, error)
	ListTestCasePayloads(ctx context.Context, arg dbgen.ListTestCasePayloadsParams) ([]dbgen.ListTestCasePayloadsRow, error)
}

// LoadPlanContext reads what this project actually has, in three statements.
//
// Three and not more: the element refs, the fixture names and the approved
// suite. Every one of them is a set membership test over the whole plan, so
// fetching them per test case would be the N+1 that runs inside the ingest
// transaction while the daemon waits for its acknowledgement.
func LoadPlanContext(ctx context.Context, q PlanQueries, orgID, projectID uuid.UUID) (PlanContext, error) {
	refs, err := q.ListElementRefsForProject(ctx, dbgen.ListElementRefsForProjectParams{
		OrgID: orgID, ProjectID: projectID,
	})
	if err != nil {
		return PlanContext{}, fmt.Errorf("list element refs: %w", db.Classify(err))
	}
	fixtures, err := q.ListProjectFixtureNames(ctx, dbgen.ListProjectFixtureNamesParams{
		OrgID: orgID, ProjectID: projectID,
	})
	if err != nil {
		return PlanContext{}, fmt.Errorf("list fixture names: %w", db.Classify(err))
	}
	existing, err := q.ListTestCasePayloads(ctx, dbgen.ListTestCasePayloadsParams{
		OrgID: orgID, ProjectID: projectID,
	})
	if err != nil {
		return PlanContext{}, fmt.Errorf("list test cases: %w", db.Classify(err))
	}

	planCtx := PlanContext{
		ElementRefs:           make(map[string]struct{}, len(refs)),
		Fixtures:              make(map[string]struct{}, len(fixtures)),
		ExistingByFingerprint: make(map[string]ExistingCase, len(existing)),
	}
	for _, ref := range refs {
		planCtx.ElementRefs[ref] = struct{}{}
	}
	for _, name := range fixtures {
		planCtx.Fixtures[name] = struct{}{}
	}
	for _, row := range existing {
		fingerprint, ok := FingerprintDocument(row.Payload)
		if !ok {
			// A stored payload that no longer decodes as a test case cannot be
			// compared against anything. It is skipped rather than treated as
			// a match: the cost of missing a duplicate is one extra draft, and
			// the cost of a false match is a case that never gets written.
			continue
		}
		// First writer wins, and the query is ordered by ref, so which of two
		// same-behaviour rows is reported does not depend on row order.
		if _, clash := planCtx.ExistingByFingerprint[fingerprint]; !clash {
			planCtx.ExistingByFingerprint[fingerprint] = ExistingCase{
				Ref: row.Ref, Status: string(row.Status),
			}
		}
	}
	return planCtx, nil
}

// PersistQueries is the write half.
type PersistQueries interface {
	UpsertPlannedTestCase(ctx context.Context, arg dbgen.UpsertPlannedTestCaseParams) (dbgen.TestCase, error)
}

// Stored counts what one plan actually changed, which is what the run event
// and the API response report.
type Stored struct {
	// Created is cases that did not exist before, Revised drafts that did.
	// They are counted apart because "the planner wrote nine new cases" and
	// "the planner rewrote nine drafts you had" are different news.
	Created int
	Revised int
	// SkippedApproved is cases whose ref matches one a human already approved.
	// The upsert declines to overwrite those, and that decline is reported
	// rather than silently absorbed: a planner that keeps regenerating a case
	// under an approved ref is a signal, not noise.
	SkippedApproved int
}

// PersistPlan writes the accepted cases as drafts.
//
// Every write goes through UpsertPlannedTestCase, whose ON CONFLICT clause
// refuses to touch a case that is already approved. There is no path here that
// edits an approved case, which is what makes "approve it and the planner
// leaves it alone" a property of the schema rather than of this function
// remembering to check.
//
// The version history writes itself: test_cases has an AFTER INSERT/UPDATE
// trigger that snapshots every payload change into test_case_versions, so a
// regression run can always replay the exact definition it used even if the
// case was re-planned since.
func PersistPlan(ctx context.Context, q PersistQueries, orgID, projectID uuid.UUID, sourceRun uuid.NullUUID, accepted []AcceptedCase) (Stored, error) {
	var stored Stored
	for _, tc := range accepted {
		row, err := q.UpsertPlannedTestCase(ctx, dbgen.UpsertPlannedTestCaseParams{
			OrgID:       orgID,
			ProjectID:   projectID,
			Ref:         tc.Ref,
			Name:        tc.Name,
			Priority:    dbgen.TestPriority(tc.Priority),
			Category:    dbgen.TestCategory(tc.Category),
			Payload:     tc.Document,
			SourceRunID: sourceRun,
		})
		if err != nil {
			// No row: the ON CONFLICT ... WHERE status = 'draft' clause found
			// an approved case under this ref and declined to overwrite it.
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				stored.SkippedApproved++
				continue
			}
			return stored, fmt.Errorf("store planned test case %s: %w", tc.Ref, db.Classify(err))
		}
		if row.CurrentVersion > 1 {
			stored.Revised++
			continue
		}
		stored.Created++
	}
	return stored, nil
}

// CategoryCountsFor turns the grouped count query into the map the coverage
// report takes.
func CategoryCountsFor(rows []dbgen.CountApprovedTestCasesByCategoryRow) map[qaschema.TestCaseCategory]int {
	out := make(map[qaschema.TestCaseCategory]int, len(rows))
	for _, row := range rows {
		out[qaschema.TestCaseCategory(row.Category)] = int(row.Total)
	}
	return out
}
