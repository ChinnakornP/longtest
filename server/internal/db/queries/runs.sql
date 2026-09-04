-- Runs, and the Postgres-backed job queue they double as.

-- name: CreateRun :one
INSERT INTO runs (org_id, project_id, runtime_id, mode, idempotency_key, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- Retry path for CreateRun: a client that replays POST /runs with the same
-- Idempotency-Key gets its original run back instead of a second browser
-- session against the customer's application.
-- name: GetRunByIdempotencyKey :one
SELECT * FROM runs WHERE org_id = $1 AND idempotency_key = $2;

-- name: GetRun :one
SELECT * FROM runs WHERE org_id = $1 AND id = $2;

-- Read-modify-write on a single run (status transitions, counter fixups) must
-- go through this so two writers cannot interleave.
-- name: GetRunForUpdate :one
SELECT * FROM runs WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: ListRuns :many
SELECT * FROM runs
WHERE org_id = $1
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id)::uuid)
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountRuns :one
SELECT count(*) FROM runs
WHERE org_id = $1
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id)::uuid);

-- Queue claim.
--
-- FOR UPDATE SKIP LOCKED, not an advisory lock: the queue is a table we
-- already have to read, the work item is a row, and SKIP LOCKED lets N daemons
-- poll concurrently while each still gets a distinct run. An advisory lock
-- would need a second lookup to decide WHICH key to lock, which reintroduces
-- the race it is supposed to remove. The lock is released by the enclosing
-- transaction, so a worker that dies mid-claim simply loses its claim.
--
-- The inner SELECT is what SKIP LOCKED applies to; the outer UPDATE then only
-- touches the one row this worker won.
-- name: ClaimQueuedRun :one
UPDATE runs AS c
SET status = 'assigned',
    attempts = c.attempts + 1,
    assigned_at = now(),
    heartbeat_at = now()
WHERE c.org_id = sqlc.arg(org_id)
  AND c.id = (
    SELECT r.id
    FROM runs r
    WHERE r.org_id = sqlc.arg(org_id)
      AND r.runtime_id = sqlc.arg(runtime_id)
      AND r.status = 'queued'
    ORDER BY r.created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING c.*;

-- name: MarkRunRunning :one
UPDATE runs
SET status = 'running', started_at = coalesce(started_at, now()), heartbeat_at = now(),
    phase = sqlc.arg(phase)
WHERE org_id = $1 AND id = $2 AND status IN ('assigned', 'running')
RETURNING *;

-- name: HeartbeatRun :execrows
UPDATE runs SET heartbeat_at = now()
WHERE org_id = $1 AND id = $2 AND status IN ('assigned', 'running');

-- name: SetRunPhase :execrows
UPDATE runs SET phase = $3, heartbeat_at = now()
WHERE org_id = $1 AND id = $2 AND status IN ('assigned', 'running');

-- Terminal transition. Guarded on the current status so a late duplicate
-- result frame from a reconnecting daemon cannot re-open or overwrite a run
-- that already finished.
-- name: FinishRun :one
UPDATE runs
SET status = sqlc.arg(status),
    finished_at = now(),
    error_code = sqlc.arg(error_code),
    error_message = sqlc.arg(error_message)
WHERE org_id = sqlc.arg(org_id)
  AND id = sqlc.arg(id)
  AND status IN ('queued', 'assigned', 'running')
RETURNING *;

-- name: CancelRun :one
UPDATE runs
SET status = 'cancelled', finished_at = now()
WHERE org_id = $1 AND id = $2 AND status IN ('queued', 'assigned', 'running')
RETURNING *;

-- Recomputes the counters from the executions that actually exist, so a
-- dropped or duplicated result frame cannot leave the summary lying.
-- name: RefreshRunCounters :one
UPDATE runs r
SET total_count   = c.total,
    passed_count  = c.passed,
    failed_count  = c.failed,
    skipped_count = c.skipped,
    error_count   = c.errored
FROM (
    SELECT count(*)                                        AS total,
           count(*) FILTER (WHERE e.result = 'pass')        AS passed,
           count(*) FILTER (WHERE e.result = 'fail')        AS failed,
           count(*) FILTER (WHERE e.result = 'skipped')     AS skipped,
           count(*) FILTER (WHERE e.result = 'error')       AS errored
    FROM executions e
    WHERE e.org_id = sqlc.arg(org_id) AND e.run_id = sqlc.arg(id)
) c
WHERE r.org_id = sqlc.arg(org_id) AND r.id = sqlc.arg(id)
RETURNING r.*;

-- Requeue sweep for runs whose daemon stopped heart-beating. Runs across every
-- organization by design.
-- name: RequeueStaleRuns :many
-- org-scope-exempt: platform maintenance sweeper, not reachable from a
-- tenant-facing handler; it never returns row contents, only ids to log.
UPDATE runs
SET status = 'queued', assigned_at = NULL, heartbeat_at = NULL
WHERE status IN ('assigned', 'running')
  AND heartbeat_at < now() - sqlc.arg(stale_after)::interval
  AND attempts < sqlc.arg(max_attempts)::integer
RETURNING id, org_id;

-- Runs that exhausted their attempts are dead-lettered rather than retried
-- forever.
-- name: FailStaleRuns :many
-- org-scope-exempt: platform maintenance sweeper, not reachable from a
-- tenant-facing handler; it never returns row contents, only ids to log.
UPDATE runs
SET status = 'error',
    finished_at = now(),
    error_code = 'runtime_lost',
    error_message = 'the runtime stopped reporting before the run finished'
WHERE status IN ('assigned', 'running')
  AND heartbeat_at < now() - sqlc.arg(stale_after)::interval
  AND attempts >= sqlc.arg(max_attempts)::integer
RETURNING id, org_id;

-- The scheduler's claim. ClaimQueuedRun above pins to a runtime that was named
-- at enqueue time; this one is what an idle daemon asks for, so it also picks
-- up runs that were created without a runtime (`runtime_id IS NULL`) and
-- stamps itself onto the row it wins.
--
-- Same FOR UPDATE SKIP LOCKED as ClaimQueuedRun and for the same reason: the
-- work item is a row we already have to read, and SKIP LOCKED lets every
-- connected daemon poll concurrently while each still gets a distinct run. An
-- advisory lock would need a prior lookup to decide WHICH key to lock, which
-- reintroduces the race it is meant to remove.
-- name: ClaimQueuedRunForRuntime :one
UPDATE runs AS c
SET status = 'assigned',
    runtime_id = sqlc.arg(runtime_id),
    attempts = c.attempts + 1,
    assigned_at = now(),
    heartbeat_at = now()
WHERE c.org_id = sqlc.arg(org_id)
  -- One run at a time per runtime: a daemon drives one browser session, and a
  -- second assignment would have it interleave two runs against the customer's
  -- application. The guard is inside the claim statement rather than a check
  -- the scheduler makes first, so two schedulers cannot both read "idle" and
  -- both assign.
  AND NOT EXISTS (
    SELECT 1 FROM runs busy
    WHERE busy.org_id = sqlc.arg(org_id)
      AND busy.runtime_id = sqlc.arg(runtime_id)
      AND busy.status IN ('assigned', 'running')
  )
  AND c.id = (
    SELECT r.id
    FROM runs r
    WHERE r.org_id = sqlc.arg(org_id)
      AND (r.runtime_id IS NULL OR r.runtime_id = sqlc.arg(runtime_id))
      AND r.status = 'queued'
    ORDER BY r.created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING c.*;

-- Undo of a claim whose `run.assign` never made it onto the wire (the daemon
-- disconnected between the claim and the send). Returning the run to the queue
-- immediately is what keeps assignment latency at one scheduler tick instead of
-- one lease expiry. `attempts` is deliberately NOT decremented: a runtime that
-- keeps dropping mid-assign must still exhaust its attempts.
-- name: ReleaseClaimedRun :execrows
UPDATE runs
SET status = 'queued', assigned_at = NULL, heartbeat_at = NULL
WHERE org_id = $1 AND id = $2 AND status = 'assigned';

-- Belongs-to check for the daemon control plane: a `run.event` frame is only
-- accepted for a run in the token's own organization that is assigned to the
-- token's own runtime. Both halves are in the WHERE clause, so a frame that
-- names another tenant's run reads as "no such run" rather than as a
-- permission decision made in Go.
-- name: GetRunForRuntime :one
SELECT * FROM runs
WHERE org_id = $1 AND id = $2 AND runtime_id = sqlc.arg(runtime_id);

-- Runs a daemon is still expected to be working on. Used when a control-plane
-- connection drops so the runs it held can be dealt with immediately rather
-- than waiting out the heartbeat lease.
-- name: ListInFlightRunsForRuntime :many
SELECT * FROM runs
WHERE org_id = $1 AND runtime_id = $2 AND status IN ('assigned', 'running')
ORDER BY created_at;

-- Lease refresh for everything a daemon says it is still working on, in one
-- statement. The runtime is bound as a parameter, so a heartbeat can only
-- extend the lease on runs that daemon actually holds — a frame listing another
-- runtime's run in the same organization updates nothing.
-- name: HeartbeatRunsForRuntime :execrows
UPDATE runs
SET heartbeat_at = now()
WHERE org_id = $1
  AND runtime_id = sqlc.arg(runtime_id)
  AND id = ANY (sqlc.arg(ids)::uuid[])
  AND status IN ('assigned', 'running');
