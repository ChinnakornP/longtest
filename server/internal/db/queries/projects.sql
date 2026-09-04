-- name: CreateProject :one
INSERT INTO projects (org_id, name, base_url)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE org_id = $1 AND id = $2;

-- Retry path for CreateProject: on a unique violation the service reads the
-- existing row back instead of surfacing a 409 for a duplicate submit.
-- name: GetProjectByName :one
SELECT * FROM projects WHERE org_id = $1 AND name = $2;

-- name: ListProjects :many
SELECT * FROM projects
WHERE org_id = $1
  AND (sqlc.arg(include_archived)::boolean OR archived_at IS NULL)
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountProjects :one
SELECT count(*) FROM projects
WHERE org_id = $1
  AND (sqlc.arg(include_archived)::boolean OR archived_at IS NULL);

-- name: UpdateProject :one
UPDATE projects
SET name = coalesce(sqlc.narg(name), name),
    base_url = coalesce(sqlc.narg(base_url), base_url)
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: ArchiveProject :one
UPDATE projects SET archived_at = now()
WHERE org_id = $1 AND id = $2 AND archived_at IS NULL
RETURNING *;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE org_id = $1 AND id = $2;
