-- name: UpsertWorkflow :one
INSERT INTO workflows (org_id, project_id, ref, name, path, expected_outcome, last_seen_run_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(run_id))
ON CONFLICT (project_id, ref) DO UPDATE
SET name = EXCLUDED.name,
    path = EXCLUDED.path,
    expected_outcome = EXCLUDED.expected_outcome,
    last_seen_run_id = coalesce(EXCLUDED.last_seen_run_id, workflows.last_seen_run_id)
RETURNING *;

-- name: GetWorkflow :one
SELECT * FROM workflows WHERE org_id = $1 AND id = $2;

-- name: ListWorkflows :many
SELECT * FROM workflows
WHERE org_id = $1 AND project_id = $2
ORDER BY name;

-- name: DeleteWorkflow :execrows
DELETE FROM workflows WHERE org_id = $1 AND id = $2;
