-- Daemon credentials. Tenancy layer, like sessions: the token lookup is how an
-- org_id is established for a daemon connection, so it cannot take one.

-- name: CreateRuntimeToken :one
INSERT INTO runtime_tokens (org_id, runtime_id, token_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- The daemon authentication read. Whatever runtime id the daemon claims in its
-- frames is ignored; the (org_id, runtime_id) pair on this row is the truth.
-- name: GetLiveRuntimeTokenByHash :one
SELECT * FROM runtime_tokens
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: TouchRuntimeToken :execrows
UPDATE runtime_tokens SET last_used_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: ListRuntimeTokens :many
SELECT id, org_id, runtime_id, created_at, last_used_at, revoked_at
FROM runtime_tokens
WHERE org_id = $1 AND runtime_id = $2
ORDER BY created_at DESC;

-- name: RevokeRuntimeToken :execrows
UPDATE runtime_tokens SET revoked_at = now()
WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL;

-- name: RevokeRuntimeTokensForRuntime :execrows
UPDATE runtime_tokens SET revoked_at = now()
WHERE org_id = $1 AND runtime_id = $2 AND revoked_at IS NULL;
