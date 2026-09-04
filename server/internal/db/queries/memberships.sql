-- Tenancy layer: see the note in organizations.sql. This is the table the auth
-- middleware checks the X-Org-ID header against on every request.

-- name: UpsertMembership :one
INSERT INTO memberships (org_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role
RETURNING *;

-- The authorization read on the request path: it answers "is this user a
-- member of this org, and as what?" in a single primary-key hit.
-- name: GetMembership :one
SELECT * FROM memberships WHERE org_id = $1 AND user_id = $2;

-- name: ListMembers :many
SELECT m.*, u.email, u.name
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1
ORDER BY u.email;

-- name: DeleteMembership :execrows
DELETE FROM memberships WHERE org_id = $1 AND user_id = $2;

-- Guard for "demote/remove the last owner": an org with no owner cannot be
-- administered again.
-- name: CountOwners :one
SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner';
