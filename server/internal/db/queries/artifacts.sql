-- Artifact metadata. The bytes go straight from the daemon to object storage
-- with a presigned PUT; the storage_key layout is enforced by a CHECK on the
-- table, so a key pointing outside this row's own org/run is rejected here.

-- Re-registering the same object after a daemon reconnect updates the row
-- rather than failing: the upload itself is idempotent on the same key.
-- name: UpsertArtifact :one
INSERT INTO artifacts (org_id, run_id, execution_id, test_case_id, kind, name,
                       storage_key, content_type, size_bytes, sha256)
VALUES ($1, $2, sqlc.narg(execution_id), sqlc.narg(test_case_id), $3, $4, $5, $6,
        sqlc.narg(size_bytes), sqlc.narg(sha256))
ON CONFLICT (storage_key) DO UPDATE
SET content_type = EXCLUDED.content_type,
    size_bytes = EXCLUDED.size_bytes,
    sha256 = EXCLUDED.sha256
RETURNING *;

-- name: GetArtifact :one
SELECT * FROM artifacts WHERE org_id = $1 AND id = $2;

-- name: ListArtifactsForRun :many
SELECT * FROM artifacts
WHERE org_id = $1 AND run_id = $2
ORDER BY created_at;

-- name: ListArtifactsForExecution :many
SELECT * FROM artifacts
WHERE org_id = $1 AND execution_id = $2
ORDER BY created_at;

-- name: DeleteArtifact :execrows
DELETE FROM artifacts WHERE org_id = $1 AND id = $2;
