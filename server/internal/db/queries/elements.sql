-- name: UpsertElement :one
INSERT INTO elements (org_id, page_id, ref, kind, label, locators, last_seen_run_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(run_id))
ON CONFLICT (page_id, ref) DO UPDATE
SET kind = EXCLUDED.kind,
    label = EXCLUDED.label,
    locators = EXCLUDED.locators,
    last_seen_run_id = coalesce(EXCLUDED.last_seen_run_id, elements.last_seen_run_id)
RETURNING *;

-- Bulk form for the discovery ingest; see the note on UpsertPageBatch.
-- name: UpsertElementBatch :batchone
INSERT INTO elements (org_id, page_id, ref, kind, label, locators, last_seen_run_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(run_id))
ON CONFLICT (page_id, ref) DO UPDATE
SET kind = EXCLUDED.kind,
    label = EXCLUDED.label,
    locators = EXCLUDED.locators,
    last_seen_run_id = coalesce(EXCLUDED.last_seen_run_id, elements.last_seen_run_id)
RETURNING *;

-- name: GetElement :one
SELECT * FROM elements WHERE org_id = $1 AND id = $2;

-- name: ListElementsForPage :many
SELECT * FROM elements
WHERE org_id = $1 AND page_id = $2
ORDER BY ref;

-- Every element of a project in one query. Assembling the Application Map is
-- then: pages + this + workflows = three statements, whatever the map's size.
-- Fetching elements per page would be the N+1 this replaces.
-- name: ListElementsForProject :many
SELECT e.*
FROM elements e
JOIN pages p ON p.id = e.page_id AND p.org_id = e.org_id
WHERE e.org_id = $1 AND p.project_id = $2
ORDER BY p.path, e.ref;

-- name: DeleteElement :execrows
DELETE FROM elements WHERE org_id = $1 AND id = $2;

-- Every element ref of a project, as bare strings.
--
-- This is the set a planned test case's `target.ref` values are checked
-- against before a single case is stored. It is refs only rather than
-- ListElementsForProject's full rows because the check is set membership over
-- a few thousand short strings, and materialising locators and labels to throw
-- them away is the allocation this avoids on every planning ingest.
-- name: ListElementRefsForProject :many
SELECT e.ref
FROM elements e
JOIN pages p ON p.id = e.page_id AND p.org_id = e.org_id
WHERE e.org_id = $1 AND p.project_id = $2
ORDER BY e.ref;
