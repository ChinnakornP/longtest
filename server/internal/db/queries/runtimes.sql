-- name: CreateRuntime :one
INSERT INTO runtimes (org_id, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetRuntime :one
SELECT * FROM runtimes WHERE org_id = $1 AND id = $2;

-- `online` is derived from last_seen_at rather than stored: a daemon that is
-- SIGKILLed never gets to write a status column on its way out.
-- name: ListRuntimes :many
SELECT *,
       (disabled_at IS NULL
        AND last_seen_at IS NOT NULL
        AND last_seen_at > now() - sqlc.arg(online_within)::interval) AS online
FROM runtimes
WHERE org_id = $1
ORDER BY name;

-- Written from the daemon `hello` frame. The org is taken from the runtime
-- token, never from the frame, so it is a parameter here and not a value the
-- daemon supplies.
-- name: RecordRuntimeHello :one
UPDATE runtimes
SET version = $3,
    browsers = $4,
    agents = $5,
    last_seen_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: TouchRuntime :execrows
UPDATE runtimes SET last_seen_at = now() WHERE org_id = $1 AND id = $2;

-- name: SetRuntimeDisabled :one
UPDATE runtimes
SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN now() ELSE NULL END
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: DeleteRuntime :execrows
DELETE FROM runtimes WHERE org_id = $1 AND id = $2;
