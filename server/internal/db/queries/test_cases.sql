-- Test cases. Every payload write here is snapshotted into test_case_versions
-- by a database trigger, so no query below has to remember to do it.

-- name: CreateTestCase :one
INSERT INTO test_cases (org_id, project_id, ref, name, priority, category, status,
                        payload, source_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.narg(source_run_id))
RETURNING *;

-- Planner ingest: a re-run of the planner must not duplicate cases, and must
-- not silently overwrite a case a human already approved.
-- name: UpsertPlannedTestCase :one
INSERT INTO test_cases (org_id, project_id, ref, name, priority, category, payload, source_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.narg(source_run_id))
ON CONFLICT (project_id, ref) DO UPDATE
SET name = EXCLUDED.name,
    priority = EXCLUDED.priority,
    category = EXCLUDED.category,
    payload = EXCLUDED.payload,
    source_run_id = EXCLUDED.source_run_id
WHERE test_cases.status = 'draft'
RETURNING *;

-- name: GetTestCase :one
SELECT * FROM test_cases WHERE org_id = $1 AND id = $2;

-- Read-modify-write on one case goes through this, never through GetTestCase.
-- A payload edit reads the row's review status and its current_version, judges
-- the caller's baseVersion against them, and only then writes; two reviewers
-- saving together must not both read version 3 and both conclude that their
-- edit is the one that applies.
-- name: GetTestCaseForUpdate :one
SELECT * FROM test_cases WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: GetTestCaseByRef :one
SELECT * FROM test_cases WHERE org_id = $1 AND project_id = $2 AND ref = $3;

-- name: ListTestCases :many
SELECT * FROM test_cases
WHERE org_id = $1
  AND project_id = $2
  AND (sqlc.narg(status)::test_case_status IS NULL
       OR status = sqlc.narg(status)::test_case_status)
ORDER BY priority, ref
LIMIT $3 OFFSET $4;

-- name: CountTestCases :one
SELECT count(*) FROM test_cases
WHERE org_id = $1
  AND project_id = $2
  AND (sqlc.narg(status)::test_case_status IS NULL
       OR status = sqlc.narg(status)::test_case_status);

-- The regression suite: what an `execute` run without an explicit selection
-- runs. One query, ordered so the report reads by priority.
-- name: ListApprovedTestCases :many
SELECT * FROM test_cases
WHERE org_id = $1 AND project_id = $2 AND status = 'approved'
ORDER BY priority, ref;

-- Editing the payload bumps current_version and writes a version row, both by
-- trigger. `status` is deliberately not editable here: PATCH status is a
-- separate authorization decision (approve/archive).
-- name: UpdateTestCasePayload :one
UPDATE test_cases
SET name = coalesce(sqlc.narg(name), name),
    priority = coalesce(sqlc.narg(priority), priority),
    category = coalesce(sqlc.narg(category), category),
    payload = sqlc.arg(payload)
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
RETURNING *;

-- name: SetTestCaseStatus :one
UPDATE test_cases SET status = $3
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: DeleteTestCase :execrows
DELETE FROM test_cases WHERE org_id = $1 AND id = $2;

-- name: ListTestCaseVersions :many
SELECT * FROM test_case_versions
WHERE org_id = $1 AND test_case_id = $2
ORDER BY version DESC
LIMIT $3;

-- The total behind a bounded version page. Counted rather than derived from
-- test_cases.current_version: the two agree today only because nothing deletes
-- a version row, and a report that quietly overstates its own history is worse
-- than one extra indexed count.
-- name: CountTestCaseVersions :one
SELECT count(*) FROM test_case_versions
WHERE org_id = $1 AND test_case_id = $2;

-- name: GetTestCaseVersion :one
SELECT * FROM test_case_versions
WHERE org_id = $1 AND test_case_id = $2 AND version = $3;

-- Resolves an explicit test-case selection in one statement.
--
-- Both the organization and the project are bound, so a selection that names a
-- case from another project (or another tenant) simply returns fewer rows than
-- were asked for, and the caller reports that as "not found" without ever
-- learning which id was the bad one.
-- name: ListTestCasesByIDs :many
SELECT * FROM test_cases
WHERE org_id = $1
  AND project_id = $2
  AND id = ANY (sqlc.arg(ids)::uuid[])
ORDER BY priority, ref;

-- Resolves the case refs a result frame names, in one statement.
--
-- A run.result carries up to 500 executions and as many findings, each naming
-- its case by ref ("TC-001"); looking them up one at a time is the N+1 this
-- replaces, and it would run inside the ingest transaction.
-- name: ListTestCasesByRefs :many
SELECT * FROM test_cases
WHERE org_id = $1
  AND project_id = $2
  AND ref = ANY (sqlc.arg(refs)::text[]);

-- The dedupe read for the planner ingest: every existing case's ref, status
-- and payload, so a freshly planned case whose normalised steps match one of
-- them can be dropped instead of stored as a second row for the same test.
--
-- Every status, not just approved. An approved match must not be re-queued for
-- review, an archived one must not come back after somebody retired it, and a
-- draft one must not be stored twice under two ids — which is what a re-plan
-- produces, because a planner renumbers its cases every run.
--
-- Payloads rather than a stored fingerprint column on purpose: the
-- normalisation is contract-aware Go (see internal/testcase/plan.go), and a
-- fingerprint written by an older version of that function would silently stop
-- matching one written by a newer one, with no backfill path and no way to
-- notice. Normalising both sides at read time cannot drift.
-- name: ListTestCasePayloads :many
SELECT id, ref, status, payload FROM test_cases
WHERE org_id = $1 AND project_id = $2
ORDER BY ref;

-- The coverage read: what the project actually runs as regression. Approved
-- only, because a draft nobody has read is not a test this project runs.
-- name: ListApprovedTestCasePayloads :many
SELECT id, ref, payload FROM test_cases
WHERE org_id = $1 AND project_id = $2 AND status = 'approved'
ORDER BY ref;

-- How many approved cases a project has in each category, for the coverage
-- report. Counted in the database rather than by loading the suite: the
-- coverage endpoint already reads every approved payload for its ref sets, and
-- this is the one number it needs that does not come from them.
-- name: CountApprovedTestCasesByCategory :many
SELECT category, count(*) AS total FROM test_cases
WHERE org_id = $1 AND project_id = $2 AND status = 'approved'
GROUP BY category;
