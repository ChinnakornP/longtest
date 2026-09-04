-- Test cases and their immutable version history.
--
-- The executable definition lives in `payload` as a `test-case@1` document
-- (contract A) rather than as normalised step rows: the executor consumes the
-- whole document, qa-schema owns its shape and validates it, and splitting it
-- across tables would mean a migration every time the action vocabulary grows.
-- `execution_steps` records what actually ran, which is a different thing.
--
-- Every payload change is snapshotted into `test_case_versions` BY A TRIGGER,
-- not by the service layer, so a regression suite can always replay exactly
-- the definition a past run used even if the case was edited since. This is
-- what makes the Phase-6 trend/flaky work possible without another migration.

-- +goose Up

CREATE TYPE test_priority AS ENUM ('critical', 'high', 'medium', 'low');

CREATE TYPE test_category AS ENUM (
    'functional', 'validation', 'navigation', 'ui_behavior', 'error_handling'
);

CREATE TYPE test_case_status AS ENUM ('draft', 'approved', 'archived');

CREATE TABLE test_cases (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid             NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id      uuid             NOT NULL,
    -- Human-facing handle inside a project ("TC-001").
    ref             text             NOT NULL CHECK (length(btrim(ref)) BETWEEN 1 AND 100),
    name            text             NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 500),
    priority        test_priority    NOT NULL DEFAULT 'medium',
    category        test_category    NOT NULL DEFAULT 'functional',
    status          test_case_status NOT NULL DEFAULT 'draft',
    -- test-case@1 document. Validated by qa-schema before it ever gets here.
    payload         jsonb            NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    -- Bumped by trigger on every payload change; matches the newest row in
    -- test_case_versions.
    current_version integer          NOT NULL DEFAULT 1 CHECK (current_version >= 1),
    -- The planner run that first produced this case, if any.
    source_run_id   uuid,
    created_at      timestamptz      NOT NULL DEFAULT now(),
    updated_at      timestamptz      NOT NULL DEFAULT now(),

    CONSTRAINT test_cases_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT test_cases_project_id_ref_key UNIQUE (project_id, ref),
    CONSTRAINT test_cases_org_project_fkey
        FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT test_cases_org_source_run_fkey
        FOREIGN KEY (org_id, source_run_id) REFERENCES runs (org_id, id)
        ON DELETE SET NULL (source_run_id)
);

-- The regression-suite read: "every approved case in this project".
CREATE INDEX test_cases_org_project_status_idx ON test_cases (org_id, project_id, status);

CREATE TRIGGER test_cases_set_updated_at
    BEFORE UPDATE ON test_cases
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE test_case_versions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid             NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    test_case_id uuid             NOT NULL,
    version      integer          NOT NULL CHECK (version >= 1),
    payload      jsonb            NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    status       test_case_status NOT NULL,
    created_at   timestamptz      NOT NULL DEFAULT now(),

    CONSTRAINT test_case_versions_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT test_case_versions_test_case_id_version_key UNIQUE (test_case_id, version),
    CONSTRAINT test_case_versions_org_test_case_fkey
        FOREIGN KEY (org_id, test_case_id) REFERENCES test_cases (org_id, id) ON DELETE CASCADE
);

CREATE INDEX test_case_versions_org_test_case_idx
    ON test_case_versions (org_id, test_case_id, version DESC);

-- +goose StatementBegin
CREATE FUNCTION test_cases_bump_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Fires only when the payload actually changed (see the WHEN clause on the
    -- trigger), so a status-only edit does not create a new version.
    NEW.current_version := OLD.current_version + 1;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION test_cases_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO test_case_versions (org_id, test_case_id, version, payload, status)
    VALUES (NEW.org_id, NEW.id, NEW.current_version, NEW.payload, NEW.status);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER test_cases_bump_version
    BEFORE UPDATE ON test_cases
    FOR EACH ROW
    WHEN (OLD.payload IS DISTINCT FROM NEW.payload)
    EXECUTE FUNCTION test_cases_bump_version();

CREATE TRIGGER test_cases_snapshot_insert
    AFTER INSERT ON test_cases
    FOR EACH ROW EXECUTE FUNCTION test_cases_snapshot();

CREATE TRIGGER test_cases_snapshot_update
    AFTER UPDATE ON test_cases
    FOR EACH ROW
    WHEN (OLD.payload IS DISTINCT FROM NEW.payload)
    EXECUTE FUNCTION test_cases_snapshot();

-- +goose Down

DROP TRIGGER IF EXISTS test_cases_snapshot_update ON test_cases;
DROP TRIGGER IF EXISTS test_cases_snapshot_insert ON test_cases;
DROP TRIGGER IF EXISTS test_cases_bump_version ON test_cases;
DROP FUNCTION IF EXISTS test_cases_snapshot();
DROP FUNCTION IF EXISTS test_cases_bump_version();
DROP TABLE IF EXISTS test_case_versions;
DROP TABLE IF EXISTS test_cases;
DROP TYPE IF EXISTS test_case_status;
DROP TYPE IF EXISTS test_category;
DROP TYPE IF EXISTS test_priority;
