-- What the failure analyst concluded, and the evidence it cited.

-- One finding per execution; re-analysis updates in place so regenerating a
-- report does not stack duplicates.
-- name: UpsertFinding :one
INSERT INTO findings (org_id, run_id, execution_id, test_case_id, step_index,
                      failure_class, summary, root_cause, confidence,
                      suggested_fix, analyzed_by_provider, analyzed_by_version)
VALUES ($1, $2, sqlc.narg(execution_id), sqlc.narg(test_case_id),
        sqlc.narg(step_index), $3, $4, $5, $6, $7,
        sqlc.narg(analyzed_by_provider), sqlc.arg(analyzed_by_version))
ON CONFLICT (execution_id) DO UPDATE
SET failure_class = EXCLUDED.failure_class,
    summary = EXCLUDED.summary,
    root_cause = EXCLUDED.root_cause,
    confidence = EXCLUDED.confidence,
    suggested_fix = EXCLUDED.suggested_fix,
    step_index = EXCLUDED.step_index,
    analyzed_by_provider = EXCLUDED.analyzed_by_provider,
    analyzed_by_version = EXCLUDED.analyzed_by_version
RETURNING *;

-- name: GetFinding :one
SELECT * FROM findings WHERE org_id = $1 AND id = $2;

-- name: ListFindingsForRun :many
SELECT * FROM findings
WHERE org_id = $1 AND run_id = $2
ORDER BY confidence DESC, created_at;

-- name: DeleteFinding :execrows
DELETE FROM findings WHERE org_id = $1 AND id = $2;

-- name: LinkFindingEvidence :execrows
INSERT INTO finding_evidence (org_id, finding_id, artifact_id)
SELECT sqlc.arg(org_id)::uuid, sqlc.arg(finding_id)::uuid, a.id
FROM unnest(sqlc.arg(artifact_ids)::uuid[]) AS a(id)
ON CONFLICT (finding_id, artifact_id) DO NOTHING;

-- Findings with their evidence for a whole run in one query. Fetching evidence
-- per finding would be the N+1 the join table exists to avoid.
-- name: ListFindingEvidenceForRun :many
SELECT fe.finding_id, sqlc.embed(a)
FROM finding_evidence fe
JOIN findings f ON f.id = fe.finding_id AND f.org_id = fe.org_id
JOIN artifacts a ON a.id = fe.artifact_id AND a.org_id = fe.org_id
WHERE fe.org_id = $1 AND f.run_id = $2
ORDER BY fe.finding_id, a.created_at;

-- name: UnlinkFindingEvidence :execrows
DELETE FROM finding_evidence
WHERE org_id = $1 AND finding_id = $2 AND artifact_id = $3;

-- Failure-class breakdown for a project's recent runs.
-- name: SummarizeFindingsByClass :many
SELECT f.failure_class, count(*) AS total
FROM findings f
JOIN runs r ON r.id = f.run_id AND r.org_id = f.org_id
WHERE f.org_id = $1
  AND r.project_id = $2
  AND f.created_at >= now() - sqlc.arg(window_size)::interval
GROUP BY f.failure_class
ORDER BY total DESC;
