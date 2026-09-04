package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// TestRunEventsAreDeduplicatedBySeq is the acceptance test for at-least-once
// event delivery: the daemon retries frames after a reconnect, and the
// (run_id, seq) unique index has to turn that into exactly-once storage.
func TestRunEventsAreDeduplicatedBySeq(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	org := newOrg(t, s)
	project := newProject(t, s, org.ID)
	run := newRun(t, s, org.ID, project.ID, uuid.NullUUID{})

	event := dbgen.AppendRunEventParams{
		OrgID:   org.ID,
		RunID:   run.ID,
		Seq:     1,
		Phase:   "discovery",
		Level:   dbgen.RunEventLevelInfo,
		Code:    "page.found",
		Message: "found /employees",
		Data:    []byte(`{"path":"/employees"}`),
		// sqlc types INSERT parameters as nullable; the column itself is NOT NULL.
		Ts: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}

	inserted, err := s.AppendRunEvent(ctx, event)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if inserted != 1 {
		t.Fatalf("first append should insert one row, got %d", inserted)
	}

	// The redelivery. Same seq, and deliberately a DIFFERENT body, to prove the
	// original is kept rather than overwritten.
	redelivered := event
	redelivered.Message = "resent after reconnect"
	again, err := s.AppendRunEvent(ctx, redelivered)
	if err != nil {
		t.Fatalf("redelivery must not error: %v", err)
	}
	if again != 0 {
		t.Fatalf("redelivery should insert nothing, got %d rows", again)
	}

	count, err := s.CountRunEvents(ctx, dbgen.CountRunEventsParams{OrgID: org.ID, RunID: run.ID})
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one stored event, got %d", count)
	}

	stored, err := s.ListRunEventsSince(ctx, dbgen.ListRunEventsSinceParams{
		OrgID: org.ID, RunID: run.ID, Seq: 0, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(stored) != 1 || stored[0].Message != "found /employees" {
		t.Fatalf("the redelivery overwrote the original: %+v", stored)
	}

	// A different seq on the same run is a new event, not a duplicate.
	next := event
	next.Seq = 2
	if inserted, err := s.AppendRunEvent(ctx, next); err != nil || inserted != 1 {
		t.Fatalf("seq 2 should be stored: rows=%d err=%v", inserted, err)
	}

	last, err := s.GetLastRunEventSeq(ctx, dbgen.GetLastRunEventSeqParams{OrgID: org.ID, RunID: run.ID})
	if err != nil {
		t.Fatalf("last seq: %v", err)
	}
	if last != 2 {
		t.Fatalf("expected last seq 2, got %d", last)
	}
}

// TestClaimQueuedRunSkipsLockedRows is the acceptance test for the queue: two
// workers polling at the same time must get different runs, and neither may
// block on the other.
func TestClaimQueuedRunSkipsLockedRows(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	org := newOrg(t, s)
	project := newProject(t, s, org.ID)
	runtime := newRuntime(t, s, org.ID)
	runtimeID := uuid.NullUUID{UUID: runtime.ID, Valid: true}

	newRun(t, s, org.ID, project.ID, runtimeID)
	newRun(t, s, org.ID, project.ID, runtimeID)

	params := dbgen.ClaimQueuedRunParams{OrgID: org.ID, RuntimeID: runtimeID}

	tx1, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(context.WithoutCancel(ctx)) }()

	tx2, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = tx2.Rollback(context.WithoutCancel(ctx)) }()

	first, err := dbgen.New(tx1).ClaimQueuedRun(ctx, params)
	if err != nil {
		t.Fatalf("tx1 claim: %v", err)
	}

	// tx1 still holds its row lock. Without SKIP LOCKED this call would block
	// until tx1 finished, so the short deadline is part of the assertion.
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	second, err := dbgen.New(tx2).ClaimQueuedRun(claimCtx, params)
	if err != nil {
		t.Fatalf("tx2 claim blocked or failed while tx1 held its row: %v", err)
	}

	if first.ID == second.ID {
		t.Fatalf("two concurrent workers claimed the same run %s", first.ID)
	}
	if first.Status != dbgen.RunStatusAssigned || second.Status != dbgen.RunStatusAssigned {
		t.Fatalf("claimed runs should be 'assigned', got %s and %s", first.Status, second.Status)
	}
	if first.Attempts != 1 || second.Attempts != 1 {
		t.Fatalf("claiming should count an attempt, got %d and %d", first.Attempts, second.Attempts)
	}

	// The queue is now empty as far as any third worker is concerned: both
	// remaining rows are locked, and SKIP LOCKED must skip rather than wait.
	tx3, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx3: %v", err)
	}
	defer func() { _ = tx3.Rollback(context.WithoutCancel(ctx)) }()

	emptyCtx, cancelEmpty := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEmpty()

	if _, err := dbgen.New(tx3).ClaimQueuedRun(emptyCtx, params); !errors.Is(Classify(err), ErrNotFound) {
		t.Fatalf("a third worker should find nothing to claim, got %v", err)
	}
}

// TestClaimQueuedRunIsScopedToOrgAndRuntime checks the claim cannot reach
// across tenants or steal another runtime's work.
func TestClaimQueuedRunIsScopedToOrgAndRuntime(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	orgA := newOrg(t, s)
	orgB := newOrg(t, s)
	projectA := newProject(t, s, orgA.ID)
	runtimeA := newRuntime(t, s, orgA.ID)
	otherRuntimeA := newRuntime(t, s, orgA.ID)
	runtimeB := newRuntime(t, s, orgB.ID)

	queued := newRun(t, s, orgA.ID, projectA.ID, uuid.NullUUID{UUID: runtimeA.ID, Valid: true})

	// Another organization's daemon must not see it, even though it exists.
	if _, err := s.ClaimQueuedRun(ctx, dbgen.ClaimQueuedRunParams{
		OrgID: orgB.ID, RuntimeID: uuid.NullUUID{UUID: runtimeB.ID, Valid: true},
	}); !errors.Is(Classify(err), ErrNotFound) {
		t.Fatalf("another org claimed the run, got %v", err)
	}

	// Nor must a different runtime in the same organization.
	if _, err := s.ClaimQueuedRun(ctx, dbgen.ClaimQueuedRunParams{
		OrgID: orgA.ID, RuntimeID: uuid.NullUUID{UUID: otherRuntimeA.ID, Valid: true},
	}); !errors.Is(Classify(err), ErrNotFound) {
		t.Fatalf("the wrong runtime claimed the run, got %v", err)
	}

	claimed, err := s.ClaimQueuedRun(ctx, dbgen.ClaimQueuedRunParams{
		OrgID: orgA.ID, RuntimeID: uuid.NullUUID{UUID: runtimeA.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("the owning runtime should claim it: %v", err)
	}
	if claimed.ID != queued.ID {
		t.Fatalf("claimed the wrong run: %s != %s", claimed.ID, queued.ID)
	}
}

// TestExecutionsAreUniquePerRunAndCase covers the other half of at-least-once
// delivery: a redelivered result must not create a second execution row.
func TestExecutionsAreUniquePerRunAndCase(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	org := newOrg(t, s)
	project := newProject(t, s, org.ID)
	run := newRun(t, s, org.ID, project.ID, uuid.NullUUID{})

	tc, err := s.CreateTestCase(ctx, dbgen.CreateTestCaseParams{
		OrgID:     org.ID,
		ProjectID: project.ID,
		Ref:       "TC-001",
		Name:      "Login",
		Priority:  dbgen.TestPriorityCritical,
		Category:  dbgen.TestCategoryFunctional,
		Status:    dbgen.TestCaseStatusApproved,
		Payload:   []byte(`{"steps":[]}`),
	})
	if err != nil {
		t.Fatalf("create test case: %v", err)
	}

	created, err := s.CreateExecutionsForRun(ctx, dbgen.CreateExecutionsForRunParams{
		OrgID:       org.ID,
		RunID:       run.ID,
		TestCaseIds: []uuid.UUID{tc.ID},
	})
	if err != nil {
		t.Fatalf("seed executions: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("expected one execution, got %d", len(created))
	}
	if created[0].TestCaseVersion != tc.CurrentVersion {
		t.Fatalf("execution should pin the current version %d, got %d",
			tc.CurrentVersion, created[0].TestCaseVersion)
	}

	// Replaying the same seeding call is a no-op rather than a duplicate.
	again, err := s.CreateExecutionsForRun(ctx, dbgen.CreateExecutionsForRunParams{
		OrgID:       org.ID,
		RunID:       run.ID,
		TestCaseIds: []uuid.UUID{tc.ID},
	})
	if err != nil {
		t.Fatalf("replay seeding: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("replaying should insert nothing, got %d rows", len(again))
	}
}
