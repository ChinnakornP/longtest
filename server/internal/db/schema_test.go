package db

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// TestSchemaTenancy checks the invariant the whole multi-tenant design rests
// on, against the real database rather than against the migration text: every
// domain table carries a NOT NULL org_id that references organizations, and
// exposes a UNIQUE (org_id, id) so it can be the target of a composite foreign
// key. A migration that forgets either one fails here.
func TestSchemaTenancy(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	var tables []string
	for table := range domainTables {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			var isNullable string
			err := s.Pool().QueryRow(ctx, `
				SELECT is_nullable
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'org_id'`,
				table).Scan(&isNullable)
			if err != nil {
				t.Fatalf("%s has no org_id column: %v", table, err)
			}
			if isNullable != "NO" {
				t.Fatalf("%s.org_id must be NOT NULL, got is_nullable=%q", table, isNullable)
			}

			var refsOrganizations bool
			if err := s.Pool().QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_constraint c
					JOIN pg_class child ON child.oid = c.conrelid
					JOIN pg_class parent ON parent.oid = c.confrelid
					WHERE c.contype = 'f'
					  AND child.relname = $1
					  AND parent.relname = 'organizations'
				)`, table).Scan(&refsOrganizations); err != nil {
				t.Fatalf("look up foreign keys of %s: %v", table, err)
			}
			if !refsOrganizations {
				t.Fatalf("%s.org_id does not reference organizations", table)
			}
		})
	}

	// Tables that can be the target of a composite (org_id, id) foreign key
	// must declare the matching unique constraint. Leaf tables are exempt: the
	// index would never be used, and on run_events - by far the highest-volume
	// table here - a redundant unique index is a real write cost.
	leafTables := map[string]bool{
		"run_events":       true, // append-only event stream, nothing references it
		"finding_evidence": true, // pure join table
	}
	t.Run("composite_fk_targets", func(t *testing.T) {
		for _, table := range tables {
			if leafTables[table] {
				continue
			}
			var hasUnique bool
			if err := s.Pool().QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_constraint c
					JOIN pg_class t ON t.oid = c.conrelid
					JOIN LATERAL unnest(c.conkey) AS k(attnum) ON true
					JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
					WHERE c.contype IN ('u', 'p')
					  AND t.relname = $1
					GROUP BY c.oid
					HAVING array_agg(a.attname::text ORDER BY a.attname::text) = ARRAY['id', 'org_id']
				)`, table).Scan(&hasUnique); err != nil {
				t.Fatalf("look up unique constraints of %s: %v", table, err)
			}
			if !hasUnique {
				t.Errorf("%s is missing UNIQUE (org_id, id); nothing can reference it with a composite FK", table)
			}
		}
	})
}

// TestCrossOrgForeignKeysAreRejected is the acceptance test for the composite
// foreign keys: a row in one organization must not be able to point at a row in
// another, and the database - not the service layer - is what refuses.
func TestCrossOrgForeignKeysAreRejected(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	orgA := newOrg(t, s)
	orgB := newOrg(t, s)

	projectB := newProject(t, s, orgB.ID)
	runtimeB := newRuntime(t, s, orgB.ID)

	t.Run("run in org A cannot reference a project in org B", func(t *testing.T) {
		_, err := s.CreateRun(ctx, dbgen.CreateRunParams{
			OrgID:     orgA.ID,
			ProjectID: projectB.ID,
			Mode:      dbgen.RunModeFull,
		})
		assertInvalidReference(t, err)
	})

	t.Run("run in org A cannot reference a runtime in org B", func(t *testing.T) {
		projectA := newProject(t, s, orgA.ID)
		_, err := s.CreateRun(ctx, dbgen.CreateRunParams{
			OrgID:     orgA.ID,
			ProjectID: projectA.ID,
			RuntimeID: uuid.NullUUID{UUID: runtimeB.ID, Valid: true},
			Mode:      dbgen.RunModeFull,
		})
		assertInvalidReference(t, err)
	})

	t.Run("page in org A cannot reference a project in org B", func(t *testing.T) {
		_, err := s.UpsertPage(ctx, dbgen.UpsertPageParams{
			OrgID:     orgA.ID,
			ProjectID: projectB.ID,
			Ref:       "page.home",
			Path:      "/",
		})
		assertInvalidReference(t, err)
	})

	t.Run("element in org A cannot reference a page in org B", func(t *testing.T) {
		pageB, err := s.UpsertPage(ctx, dbgen.UpsertPageParams{
			OrgID:     orgB.ID,
			ProjectID: projectB.ID,
			Ref:       "page.home",
			Path:      "/",
		})
		if err != nil {
			t.Fatalf("seed page: %v", err)
		}
		_, err = s.UpsertElement(ctx, dbgen.UpsertElementParams{
			OrgID:    orgA.ID,
			PageID:   pageB.ID,
			Ref:      "home.btn.login",
			Locators: []byte(`[]`),
		})
		assertInvalidReference(t, err)
	})

	t.Run("artifact in org A cannot reference a run in org B", func(t *testing.T) {
		runB := newRun(t, s, orgB.ID, projectB.ID, uuid.NullUUID{})
		_, err := s.UpsertArtifact(ctx, dbgen.UpsertArtifactParams{
			OrgID:      orgA.ID,
			RunID:      runB.ID,
			Kind:       dbgen.ArtifactKindScreenshot,
			Name:       "shot.png",
			StorageKey: artifactKey(orgA.ID, runB.ID, uuid.Nil, "shot.png"),
		})
		assertInvalidReference(t, err)
	})

	t.Run("the same write inside one org succeeds", func(t *testing.T) {
		// Without this the test above would also pass if the write were broken
		// for an unrelated reason.
		projectA := newProject(t, s, orgA.ID)
		runA := newRun(t, s, orgA.ID, projectA.ID, uuid.NullUUID{})
		if _, err := s.UpsertArtifact(ctx, dbgen.UpsertArtifactParams{
			OrgID:      orgA.ID,
			RunID:      runA.ID,
			Kind:       dbgen.ArtifactKindScreenshot,
			Name:       "shot.png",
			StorageKey: artifactKey(orgA.ID, runA.ID, uuid.Nil, "shot.png"),
		}); err != nil {
			t.Fatalf("same-org artifact should be accepted: %v", err)
		}
	})
}

// TestArtifactStorageKeyIsTenantScoped covers the CHECK that keeps a daemon
// from registering an object under another tenant's prefix.
func TestArtifactStorageKeyIsTenantScoped(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	org := newOrg(t, s)
	other := newOrg(t, s)
	project := newProject(t, s, org.ID)
	run := newRun(t, s, org.ID, project.ID, uuid.NullUUID{})
	day := time.Now().UTC().Format("2006-01-02")

	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"canonical run-level key", fmt.Sprintf("orgs/%s/runs/%s/%s/network.json", org.ID, day, run.ID), false},
		// The per-case segment is the test case's ref, which is what the daemon
		// has: it is handed test-case@1 documents and never sees test_cases.id.
		{"canonical per-case key", fmt.Sprintf("orgs/%s/runs/%s/%s/TC-001/trace.zip", org.ID, day, run.ID), false},
		{"another org's prefix", fmt.Sprintf("orgs/%s/runs/%s/%s/shot.png", other.ID, day, run.ID), true},
		{"another run's prefix", fmt.Sprintf("orgs/%s/runs/%s/%s/shot.png", org.ID, day, uuid.New()), true},
		{"no org prefix at all", fmt.Sprintf("runs/%s/%s/shot.png", day, run.ID), true},
		{"path traversal in the name", fmt.Sprintf("orgs/%s/runs/%s/%s/../../shot.png", org.ID, day, run.ID), true},
		{"path traversal in the case segment", fmt.Sprintf("orgs/%s/runs/%s/%s/../%s/shot.png", org.ID, day, run.ID, uuid.New()), true},
		// The case above is three segments deep, so it would be rejected on
		// depth alone. These are the traversals that ARE one segment deep and
		// so have to be refused by the segment shape itself.
		{"dot-dot as the case segment", fmt.Sprintf("orgs/%s/runs/%s/%s/../shot.png", org.ID, day, run.ID), true},
		{"single dot as the case segment", fmt.Sprintf("orgs/%s/runs/%s/%s/./shot.png", org.ID, day, run.ID), true},
		{"dot-dot as the name", fmt.Sprintf("orgs/%s/runs/%s/%s/..", org.ID, day, run.ID), true},
		{"dotfile name", fmt.Sprintf("orgs/%s/runs/%s/%s/.bashrc", org.ID, day, run.ID), true},
		{"malformed date", fmt.Sprintf("orgs/%s/runs/latest/%s/shot.png", org.ID, run.ID), true},
		// The tail is bounded: one optional case segment, then a name. A deeper
		// path would let a daemon invent structure the report cannot address.
		{"one segment too deep", fmt.Sprintf("orgs/%s/runs/%s/%s/TC-001/attempt-2/shot.png", org.ID, day, run.ID), true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.UpsertArtifact(ctx, dbgen.UpsertArtifactParams{
				OrgID:      org.ID,
				RunID:      run.ID,
				Kind:       dbgen.ArtifactKindScreenshot,
				Name:       "shot.png",
				StorageKey: tt.key,
			})
			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("expected %q to be rejected", tt.key)
			case tt.wantErr:
				if got := Classify(err); !errors.Is(got, ErrInvalidValue) {
					t.Fatalf("expected an invalid-value error, got %v", got)
				}
			case err != nil:
				t.Fatalf("expected %q to be accepted: %v", tt.key, err)
			}
		})
	}
}

// TestTestCaseVersionsAreSnapshotted covers the trigger that makes the Phase-6
// regression work possible: every payload change leaves an immutable copy
// behind, and a status-only change does not.
func TestTestCaseVersionsAreSnapshotted(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	org := newOrg(t, s)
	project := newProject(t, s, org.ID)

	tc, err := s.CreateTestCase(ctx, dbgen.CreateTestCaseParams{
		OrgID:     org.ID,
		ProjectID: project.ID,
		Ref:       "TC-001",
		Name:      "Create employee",
		Priority:  dbgen.TestPriorityHigh,
		Category:  dbgen.TestCategoryFunctional,
		Status:    dbgen.TestCaseStatusDraft,
		Payload:   []byte(`{"steps":[{"action":"navigate","url":"/employees"}]}`),
	})
	if err != nil {
		t.Fatalf("create test case: %v", err)
	}
	if tc.CurrentVersion != 1 {
		t.Fatalf("new test case should start at version 1, got %d", tc.CurrentVersion)
	}
	assertVersionCount(t, s, org.ID, tc.ID, 1)

	updated, err := s.UpdateTestCasePayload(ctx, dbgen.UpdateTestCasePayloadParams{
		OrgID:   org.ID,
		ID:      tc.ID,
		Payload: []byte(`{"steps":[{"action":"navigate","url":"/employees/new"}]}`),
	})
	if err != nil {
		t.Fatalf("update payload: %v", err)
	}
	if updated.CurrentVersion != 2 {
		t.Fatalf("editing the payload should bump to version 2, got %d", updated.CurrentVersion)
	}
	assertVersionCount(t, s, org.ID, tc.ID, 2)

	// v1 must still hold the ORIGINAL payload, not a copy of the new one.
	v1, err := s.GetTestCaseVersion(ctx, dbgen.GetTestCaseVersionParams{
		OrgID: org.ID, TestCaseID: tc.ID, Version: 1,
	})
	if err != nil {
		t.Fatalf("read version 1: %v", err)
	}
	if !strings.Contains(string(v1.Payload), `"/employees"`) || strings.Contains(string(v1.Payload), `"/employees/new"`) {
		t.Fatalf("version 1 was mutated by a later edit: %s", v1.Payload)
	}

	// Approving is not a payload change and must not create a version.
	if _, err := s.SetTestCaseStatus(ctx, dbgen.SetTestCaseStatusParams{
		OrgID: org.ID, ID: tc.ID, Status: dbgen.TestCaseStatusApproved,
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	assertVersionCount(t, s, org.ID, tc.ID, 2)
}

// TestRunIdempotencyKeyIsUniquePerOrg covers the retry guard on POST /runs: the
// same key twice in one org is a conflict, while two orgs may use the same key.
func TestRunIdempotencyKeyIsUniquePerOrg(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	orgA := newOrg(t, s)
	orgB := newOrg(t, s)
	projectA := newProject(t, s, orgA.ID)
	projectB := newProject(t, s, orgB.ID)

	key := pgtype.Text{String: "idem-" + uuid.NewString(), Valid: true}

	first, err := s.CreateRun(ctx, dbgen.CreateRunParams{
		OrgID: orgA.ID, ProjectID: projectA.ID, Mode: dbgen.RunModeFull, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = s.CreateRun(ctx, dbgen.CreateRunParams{
		OrgID: orgA.ID, ProjectID: projectA.ID, Mode: dbgen.RunModeFull, IdempotencyKey: key,
	})
	if got := Classify(err); !errors.Is(got, ErrConflict) {
		t.Fatalf("replaying the same idempotency key should conflict, got %v", got)
	}

	// The retry path: read the original back instead of starting a second run.
	existing, err := s.GetRunByIdempotencyKey(ctx, dbgen.GetRunByIdempotencyKeyParams{
		OrgID: orgA.ID, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("read back by idempotency key: %v", err)
	}
	if existing.ID != first.ID {
		t.Fatalf("idempotent retry returned a different run: %s != %s", existing.ID, first.ID)
	}

	// Keys are scoped per organization, so another tenant is unaffected.
	if _, err := s.CreateRun(ctx, dbgen.CreateRunParams{
		OrgID: orgB.ID, ProjectID: projectB.ID, Mode: dbgen.RunModeFull, IdempotencyKey: key,
	}); err != nil {
		t.Fatalf("the same key in another org must be accepted: %v", err)
	}
}

// TestWithTxRollsBackOnError checks that a failed unit of work leaves nothing
// behind - the guarantee every multi-row write in the services depends on.
func TestWithTxRollsBackOnError(t *testing.T) {
	s := requireDB(t)
	ctx := t.Context()

	org := newOrg(t, s)
	name := "project-" + uuid.NewString()
	sentinel := errors.New("boom")

	err := s.WithTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.CreateProject(ctx, dbgen.CreateProjectParams{
			OrgID: org.ID, Name: name, BaseURL: "https://demo.example.com",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx should return fn's error, got %v", err)
	}

	if _, err := s.GetProjectByName(ctx, dbgen.GetProjectByNameParams{OrgID: org.ID, Name: name}); !errors.Is(Classify(err), ErrNotFound) {
		t.Fatalf("the rolled-back project is still visible: %v", err)
	}
}

// --- helpers --------------------------------------------------------------

func assertInvalidReference(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the database to reject this cross-organization reference")
	}
	classified := Classify(err)
	if !errors.Is(classified, ErrInvalidReference) {
		t.Fatalf("expected an invalid-reference error, got %v", classified)
	}
	var ce *ConstraintError
	if !errors.As(classified, &ce) || ce.Constraint == "" {
		t.Fatalf("expected the violated constraint to be named, got %v", classified)
	}
}

func assertVersionCount(t *testing.T, s *Store, orgID, testCaseID uuid.UUID, want int) {
	t.Helper()
	versions, err := s.ListTestCaseVersions(t.Context(), dbgen.ListTestCaseVersionsParams{
		OrgID: orgID, TestCaseID: testCaseID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != want {
		t.Fatalf("expected %d snapshot(s), got %d", want, len(versions))
	}
}

func artifactKey(orgID, runID, testCaseID uuid.UUID, name string) string {
	day := time.Now().UTC().Format("2006-01-02")
	if testCaseID == uuid.Nil {
		return fmt.Sprintf("orgs/%s/runs/%s/%s/%s", orgID, day, runID, name)
	}
	return fmt.Sprintf("orgs/%s/runs/%s/%s/%s/%s", orgID, day, runID, testCaseID, name)
}
