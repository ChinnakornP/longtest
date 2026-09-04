-- name: CreateExecution :one
INSERT INTO executions (org_id, run_id, test_case_id, test_case_version)
VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id, test_case_id) DO NOTHING
RETURNING *;

-- Seeds a run's whole work list in one statement from the cases it selected,
-- pinning each to the version that is current right now.
-- name: CreateExecutionsForRun :many
INSERT INTO executions (org_id, run_id, test_case_id, test_case_version)
SELECT tc.org_id, sqlc.arg(run_id)::uuid, tc.id, tc.current_version
FROM test_cases tc
WHERE tc.org_id = sqlc.arg(org_id)
  AND tc.id = ANY (sqlc.arg(test_case_ids)::uuid[])
ON CONFLICT (run_id, test_case_id) DO NOTHING
RETURNING *;

-- name: GetExecution :one
SELECT * FROM executions WHERE org_id = $1 AND id = $2;

-- name: GetExecutionByRunAndCase :one
SELECT * FROM executions WHERE org_id = $1 AND run_id = $2 AND test_case_id = $3;

-- name: MarkExecutionRunning :one
UPDATE executions
SET result = 'running', started_at = coalesce(started_at, now())
WHERE org_id = $1 AND id = $2 AND result IN ('pending', 'running')
RETURNING *;

-- Terminal write, guarded on the current result so a redelivered result frame
-- is a no-op rather than a rewrite.
-- name: FinishExecution :one
UPDATE executions
SET result = sqlc.arg(result),
    failure_class = sqlc.narg(failure_class),
    error_message = sqlc.arg(error_message),
    duration_ms = sqlc.narg(duration_ms),
    finished_at = now()
WHERE org_id = sqlc.arg(org_id)
  AND id = sqlc.arg(id)
  AND result IN ('pending', 'running')
RETURNING *;

-- The report read: executions plus the case they ran, in one join. Fetching
-- the test case per execution would be the N+1 this replaces.
-- name: ListExecutionsForRun :many
SELECT sqlc.embed(e), tc.ref, tc.name, tc.priority, tc.category
FROM executions e
JOIN test_cases tc ON tc.id = e.test_case_id AND tc.org_id = e.org_id
WHERE e.org_id = $1 AND e.run_id = $2
ORDER BY tc.priority, tc.ref;

-- Phase 6 input: how one case has behaved over its recent runs.
-- name: ListRecentExecutionsForTestCase :many
SELECT * FROM executions
WHERE org_id = $1 AND test_case_id = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: UpsertExecutionStep :one
INSERT INTO execution_steps (org_id, execution_id, step_index, action, target,
                             result, unstable, error_message, duration_ms,
                             started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.narg(duration_ms),
        sqlc.narg(started_at), sqlc.narg(finished_at))
ON CONFLICT (execution_id, step_index) DO UPDATE
SET action = EXCLUDED.action,
    target = EXCLUDED.target,
    result = EXCLUDED.result,
    unstable = EXCLUDED.unstable,
    error_message = EXCLUDED.error_message,
    duration_ms = EXCLUDED.duration_ms,
    started_at = EXCLUDED.started_at,
    finished_at = EXCLUDED.finished_at
RETURNING *;

-- name: ListExecutionSteps :many
SELECT * FROM execution_steps
WHERE org_id = $1 AND execution_id = $2
ORDER BY step_index;

-- Every step of a run in one query, for the report view.
-- name: ListExecutionStepsForRun :many
SELECT s.*
FROM execution_steps s
JOIN executions e ON e.id = s.execution_id AND e.org_id = s.org_id
WHERE s.org_id = $1 AND e.run_id = $2
ORDER BY s.execution_id, s.step_index;
