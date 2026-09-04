-- Tenancy layer: see the note in organizations.sql. Sessions are keyed by the
-- SHA-256 of the cookie value, never by the cookie value itself.

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- The per-request session read. Expiry and revocation are filtered here rather
-- than in Go so that a handler cannot forget the check.
-- name: GetLiveSessionByTokenHash :one
SELECT sqlc.embed(s), sqlc.embed(u)
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND s.expires_at > now();

-- name: TouchSession :execrows
UPDATE sessions SET last_used_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeSession :execrows
UPDATE sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL;

-- "Log out everywhere", also used after a password change.
-- name: RevokeUserSessions :execrows
UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- Housekeeping: expired rows are useless and this table only grows.
-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < now() - sqlc.arg(grace)::interval;
