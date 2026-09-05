-- Fixture names. There is deliberately no query here that returns a
-- credential, because there is no column that holds one.

-- Registering the same fixture twice is the retry this upsert exists for.
-- name: UpsertProjectFixture :one
INSERT INTO project_fixtures (org_id, project_id, name, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, name) DO UPDATE
SET description = EXCLUDED.description
RETURNING *;

-- name: ListProjectFixtures :many
SELECT * FROM project_fixtures
WHERE org_id = $1 AND project_id = $2
ORDER BY name;

-- The plan-ingest read: names only, one statement, no row objects to build.
-- name: ListProjectFixtureNames :many
SELECT name FROM project_fixtures
WHERE org_id = $1 AND project_id = $2
ORDER BY name;

-- name: DeleteProjectFixture :execrows
DELETE FROM project_fixtures WHERE org_id = $1 AND project_id = $2 AND name = $3;
