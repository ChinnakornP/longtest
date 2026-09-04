-- name: UpsertPage :one
INSERT INTO pages (org_id, project_id, ref, path, title, auth_required,
                   first_seen_run_id, last_seen_run_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(run_id), sqlc.narg(run_id))
ON CONFLICT (project_id, ref) DO UPDATE
SET path = EXCLUDED.path,
    title = EXCLUDED.title,
    auth_required = EXCLUDED.auth_required,
    -- Discovery never resets first_seen_run_id: it is what dates a page.
    last_seen_run_id = coalesce(EXCLUDED.last_seen_run_id, pages.last_seen_run_id)
RETURNING *;

-- Bulk form for the discovery ingest, issued as a single pgx batch: one
-- network round trip for the whole map instead of one per page. Fetching or
-- writing the map page by page is the N+1 this replaces.
-- name: UpsertPageBatch :batchone
INSERT INTO pages (org_id, project_id, ref, path, title, auth_required,
                   first_seen_run_id, last_seen_run_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(run_id), sqlc.narg(run_id))
ON CONFLICT (project_id, ref) DO UPDATE
SET path = EXCLUDED.path,
    title = EXCLUDED.title,
    auth_required = EXCLUDED.auth_required,
    last_seen_run_id = coalesce(EXCLUDED.last_seen_run_id, pages.last_seen_run_id)
RETURNING *;

-- name: GetPage :one
SELECT * FROM pages WHERE org_id = $1 AND id = $2;

-- name: GetPageByRef :one
SELECT * FROM pages WHERE org_id = $1 AND project_id = $2 AND ref = $3;

-- name: ListPages :many
SELECT * FROM pages
WHERE org_id = $1 AND project_id = $2
ORDER BY path;

-- Stale-map filter: pages not observed by the given discovery run. Callers
-- report these rather than deleting them, because live test cases may still
-- reference them.
-- name: ListStalePages :many
SELECT * FROM pages
WHERE org_id = $1
  AND project_id = $2
  AND (last_seen_run_id IS NULL OR last_seen_run_id <> sqlc.arg(run_id))
ORDER BY path;

-- name: DeletePage :execrows
DELETE FROM pages WHERE org_id = $1 AND id = $2;
