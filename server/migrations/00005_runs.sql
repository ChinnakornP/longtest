-- Runs and their event stream.
--
-- `runs` doubles as the job queue. There is no Redis in the MVP: workers claim
-- with `FOR UPDATE SKIP LOCKED` (see internal/db/queries/runs.sql), which gives
-- exactly-one-claimer semantics without a second piece of infrastructure and
-- without the "job is in Redis but not in Postgres" split brain.
--
-- `run_events` is the at-least-once side of the daemon protocol: the daemon
-- numbers its frames per run and retries on reconnect, so the (run_id, seq)
-- unique index is what turns "at least once" into "exactly once" on our side.

-- +goose Up

CREATE TYPE run_mode AS ENUM ('discover', 'plan', 'execute', 'full');

CREATE TYPE run_status AS ENUM (
    'queued',     -- created, waiting for its runtime to pick it up
    'assigned',   -- claimed by a daemon, not started yet
    'running',
    'passed',     -- terminal: finished, every execution passed
    'failed',     -- terminal: finished, at least one execution failed
    'cancelled',  -- terminal: cancelled by a user
    'error'       -- terminal: the harness itself broke (not the app under test)
);

CREATE TYPE run_event_level AS ENUM ('debug', 'info', 'warn', 'error');

CREATE TABLE runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid       NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id   uuid       NOT NULL,
    -- NULL only while a run is waiting for any runtime; contract C always
    -- names one, so in practice this is set at insert time.
    runtime_id   uuid,
    mode         run_mode   NOT NULL,
    status       run_status NOT NULL DEFAULT 'queued',
    -- Free-form progress label ("discovering", "planning", ...). Enumerating
    -- it would force a migration every time the pipeline grows a step.
    phase        text       NOT NULL DEFAULT '',

    -- Client-supplied retry key. `POST /runs` replayed with the same key must
    -- return the original run instead of starting a second browser session.
    idempotency_key text,

    -- Queue bookkeeping. `attempts` + `heartbeat_at` let a supervisor requeue
    -- a run whose daemon died without ever reporting a result.
    attempts     integer    NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    heartbeat_at timestamptz,

    -- Counters kept on the row so the run list does not aggregate executions
    -- per row (that is the N+1 this table exists to avoid).
    total_count   integer NOT NULL DEFAULT 0 CHECK (total_count >= 0),
    passed_count  integer NOT NULL DEFAULT 0 CHECK (passed_count >= 0),
    failed_count  integer NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    skipped_count integer NOT NULL DEFAULT 0 CHECK (skipped_count >= 0),
    error_count   integer NOT NULL DEFAULT 0 CHECK (error_count >= 0),

    -- Domain error, not a driver error: the message is safe to show a user.
    error_code    text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',

    created_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    assigned_at timestamptz,
    started_at  timestamptz,
    finished_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT runs_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT runs_org_project_fkey
        FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE,
    -- A runtime may be decommissioned while its finished runs are kept.
    CONSTRAINT runs_org_runtime_fkey
        FOREIGN KEY (org_id, runtime_id) REFERENCES runtimes (org_id, id)
        ON DELETE SET NULL (runtime_id),
    CONSTRAINT runs_finished_has_timestamp CHECK (
        (status IN ('passed', 'failed', 'cancelled', 'error')) = (finished_at IS NOT NULL)
    )
);

-- Idempotent create. Partial so that the common case (no key) is not forced
-- through a single-row-per-org bottleneck.
CREATE UNIQUE INDEX runs_org_id_idempotency_key_key ON runs (org_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- The claim query's index: partial on the queue head only, so it stays small
-- however many finished runs pile up behind it.
CREATE INDEX runs_queue_idx ON runs (runtime_id, created_at)
    WHERE status = 'queued';

-- Requeue sweep for runs whose daemon stopped heart-beating.
CREATE INDEX runs_inflight_heartbeat_idx ON runs (heartbeat_at)
    WHERE status IN ('assigned', 'running');

CREATE INDEX runs_org_id_created_at_idx ON runs (org_id, created_at DESC);
CREATE INDEX runs_org_project_created_at_idx ON runs (org_id, project_id, created_at DESC);

CREATE TRIGGER runs_set_updated_at
    BEFORE UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE run_events (
    -- bigint identity, not uuid: this is the highest-volume table in the
    -- system and it is always read in insertion order.
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id  uuid   NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    run_id  uuid   NOT NULL,
    -- Per-run sequence number assigned by the daemon.
    seq     bigint NOT NULL CHECK (seq >= 0),
    phase   text            NOT NULL DEFAULT '',
    level   run_event_level NOT NULL DEFAULT 'info',
    code    text            NOT NULL DEFAULT '',
    message text            NOT NULL DEFAULT '',
    data    jsonb           NOT NULL DEFAULT '{}'::jsonb,
    -- When the daemon produced the event, vs. when we durably stored it.
    ts         timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- The dedup gate for at-least-once delivery. Also the index that serves
    -- `GET /runs/{id}/events?since={seq}`.
    CONSTRAINT run_events_run_id_seq_key UNIQUE (run_id, seq),
    CONSTRAINT run_events_org_run_fkey
        FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id) ON DELETE CASCADE
);

-- +goose Down

DROP TABLE IF EXISTS run_events;
DROP TABLE IF EXISTS runs;
DROP TYPE IF EXISTS run_event_level;
DROP TYPE IF EXISTS run_status;
DROP TYPE IF EXISTS run_mode;
