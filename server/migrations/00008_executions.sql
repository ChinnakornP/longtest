-- What actually happened when a run executed a test case.
--
-- `executions` is created up front for every case a run intends to execute, so
-- the row doubles as the run's work list: `POST /runs` with an explicit
-- testCaseIds selection pre-inserts them as 'pending'. That keeps the
-- selection referentially intact (no uuid[] column that can point at a deleted
-- case) and makes UNIQUE (run_id, test_case_id) the idempotency guard for a
-- daemon that reports the same result twice.

-- +goose Up

CREATE TYPE execution_result AS ENUM (
    'pending', 'running', 'pass', 'fail', 'skipped', 'error'
);

CREATE TYPE failure_class AS ENUM (
    'PRODUCT_BUG',
    'TEST_BUG',
    'ENVIRONMENT_ERROR',
    'NETWORK_ERROR',
    'AUTHENTICATION_ERROR',
    'TIMEOUT',
    'UNKNOWN'
);

CREATE TABLE executions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid             NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    run_id       uuid             NOT NULL,
    test_case_id uuid             NOT NULL,
    -- Exact definition this execution ran, so a report stays readable after
    -- the case is edited.
    test_case_version integer     NOT NULL CHECK (test_case_version >= 1),
    result       execution_result NOT NULL DEFAULT 'pending',
    -- Set only on a non-pass terminal result; the CHECK keeps the two in sync.
    failure_class failure_class,
    error_message text            NOT NULL DEFAULT '',
    duration_ms  integer          CHECK (duration_ms IS NULL OR duration_ms >= 0),
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz      NOT NULL DEFAULT now(),
    updated_at   timestamptz      NOT NULL DEFAULT now(),

    CONSTRAINT executions_org_id_id_key UNIQUE (org_id, id),
    -- One execution of a case per run: the dedup gate for a retried result.
    CONSTRAINT executions_run_id_test_case_id_key UNIQUE (run_id, test_case_id),
    CONSTRAINT executions_org_run_fkey
        FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id) ON DELETE CASCADE,
    CONSTRAINT executions_org_test_case_fkey
        FOREIGN KEY (org_id, test_case_id) REFERENCES test_cases (org_id, id) ON DELETE CASCADE,
    -- The snapshot must exist. Combined with the FK above, this cannot point
    -- into another tenant: test_case_id is already org-checked.
    CONSTRAINT executions_test_case_version_fkey
        FOREIGN KEY (test_case_id, test_case_version)
        REFERENCES test_case_versions (test_case_id, version),
    CONSTRAINT executions_failure_class_matches_result CHECK (
        failure_class IS NULL OR result IN ('fail', 'error')
    )
);

CREATE INDEX executions_org_run_result_idx ON executions (org_id, run_id, result);
-- Phase 6: "how has this case behaved over the last N runs?"
CREATE INDEX executions_org_test_case_created_at_idx
    ON executions (org_id, test_case_id, created_at DESC);

CREATE TRIGGER executions_set_updated_at
    BEFORE UPDATE ON executions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE execution_steps (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid             NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    execution_id uuid             NOT NULL,
    -- 0-based index into the test case's `steps` array.
    step_index   integer          NOT NULL CHECK (step_index >= 0),
    action       text             NOT NULL,
    -- The step's target as executed, plus the locator that actually resolved.
    target       jsonb            NOT NULL DEFAULT '{}'::jsonb,
    result       execution_result NOT NULL DEFAULT 'pending',
    -- true when the step used a raw locator instead of an Application Map ref;
    -- such steps are expected to rot and are reported separately.
    unstable     boolean          NOT NULL DEFAULT false,
    error_message text            NOT NULL DEFAULT '',
    duration_ms  integer          CHECK (duration_ms IS NULL OR duration_ms >= 0),
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz      NOT NULL DEFAULT now(),

    CONSTRAINT execution_steps_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT execution_steps_execution_id_step_index_key UNIQUE (execution_id, step_index),
    CONSTRAINT execution_steps_org_execution_fkey
        FOREIGN KEY (org_id, execution_id) REFERENCES executions (org_id, id) ON DELETE CASCADE
);

CREATE INDEX execution_steps_org_execution_idx
    ON execution_steps (org_id, execution_id, step_index);

-- +goose Down

DROP TABLE IF EXISTS execution_steps;
DROP TABLE IF EXISTS executions;
DROP TYPE IF EXISTS failure_class;
DROP TYPE IF EXISTS execution_result;
