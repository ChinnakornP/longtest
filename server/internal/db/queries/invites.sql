-- Organization invites. Tenancy layer, like sessions and runtime_tokens: the
-- accept path is how an org_id is established for the invitee, so it looks the
-- row up by token hash and cannot take an org_id parameter.

-- name: CreateInvite :one
INSERT INTO invites (org_id, email, role, token_hash, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- Re-inviting somebody rotates the token instead of leaving two valid ones
-- outstanding. Run inside the same transaction as CreateInvite, otherwise the
-- partial unique index rejects the new row.
-- name: RevokeLiveInvitesForEmail :execrows
UPDATE invites
SET revoked_at = now()
WHERE org_id = $1
  AND email = $2
  AND accepted_at IS NULL
  AND revoked_at IS NULL;

-- The acceptance read. Expiry, revocation and prior acceptance are filtered
-- here rather than in Go so no caller can forget one of the three.
-- name: GetLiveInviteByTokenHash :one
SELECT * FROM invites
WHERE token_hash = $1
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > now();

-- The claim. `accepted_at IS NULL` in the predicate plus the row lock the
-- UPDATE takes means two concurrent accepts of the same token produce exactly
-- one winner, with no read-then-write window in between.
-- name: AcceptInvite :one
UPDATE invites
SET accepted_at = now(), accepted_by = sqlc.arg(accepted_by)
WHERE token_hash = sqlc.arg(token_hash)
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: ListInvites :many
SELECT * FROM invites
WHERE org_id = $1
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > now()
ORDER BY created_at DESC;

-- name: RevokeInvite :execrows
UPDATE invites
SET revoked_at = now()
WHERE org_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL;

-- Housekeeping: an expired invite can never be accepted again.
-- name: DeleteExpiredInvites :execrows
DELETE FROM invites
WHERE accepted_at IS NULL AND expires_at < now() - sqlc.arg(grace)::interval;
