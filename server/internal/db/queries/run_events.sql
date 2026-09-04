-- The run event stream. Delivery from the daemon is at-least-once, so every
-- write here is idempotent on (run_id, seq).

-- Returns 1 when the event was new and 0 when it was a redelivery, which is
-- what the WebSocket fan-out uses to decide whether to broadcast.
-- name: AppendRunEvent :execrows
INSERT INTO run_events (org_id, run_id, seq, phase, level, code, message, data, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (run_id, seq) DO NOTHING;

-- Bulk form for a daemon flushing its backlog after a reconnect: one pgx
-- batch instead of a round trip per event, deduped by the same unique index.
-- name: AppendRunEventBatch :batchexec
INSERT INTO run_events (org_id, run_id, seq, phase, level, code, message, data, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (run_id, seq) DO NOTHING;

-- Backs `GET /runs/{id}/events?since={seq}`; served entirely by the
-- (run_id, seq) unique index.
-- name: ListRunEventsSince :many
SELECT * FROM run_events
WHERE org_id = $1 AND run_id = $2 AND seq > $3
ORDER BY seq
LIMIT $4;

-- name: GetLastRunEventSeq :one
SELECT coalesce(max(seq), -1)::bigint
FROM run_events
WHERE org_id = $1 AND run_id = $2;

-- name: CountRunEvents :one
SELECT count(*) FROM run_events WHERE org_id = $1 AND run_id = $2;
