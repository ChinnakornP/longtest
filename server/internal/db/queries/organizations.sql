-- Tenancy root. `organizations`, `users`, `memberships` and `sessions` are the
-- tenancy layer itself, so they are outside the org_id-parameter rule that
-- TestQueriesAreOrgScoped enforces on domain tables: these queries are how an
-- org_id is established in the first place.

-- name: CreateOrganization :one
INSERT INTO organizations (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = $1;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: UpdateOrganization :one
UPDATE organizations
SET name = $2
WHERE id = $1
RETURNING *;

-- name: DeleteOrganization :execrows
DELETE FROM organizations WHERE id = $1;

-- Org picker for a signed-in user; the role comes along so the UI can hide
-- what the member cannot do.
-- name: ListOrganizationsForUser :many
SELECT o.*, m.role
FROM organizations o
JOIN memberships m ON m.org_id = o.id
WHERE m.user_id = $1
ORDER BY o.name;
