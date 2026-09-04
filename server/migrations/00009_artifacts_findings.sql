-- Evidence (artifacts) and the AI's conclusions about it (findings).
--
-- Artifacts are uploaded by the daemon straight to S3/MinIO with a presigned
-- PUT; only the metadata lands here. `storage_key` therefore has to be trusted
-- to be tenant-scoped, so its layout is enforced by a CHECK rather than by
-- convention: the key must literally start with this row's own org_id and
-- run_id. A daemon that tries to register an object under another tenant's
-- prefix is rejected by the database, not just by the service layer.
--
-- Layout (execution-result@1 Artifact.key, org-scoped per the multi-tenant
-- decision):
--   orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/{testCaseRef}/{name}
-- with the {testCaseRef} segment omitted for run-level artifacts.
--
-- That segment is a test case *ref* (TC-001), not this database's uuid. The
-- daemon composes the key from the test-case document it was handed over the
-- control plane, which carries the ref and never the uuid the backend assigns,
-- so requiring a uuid here would reject every artifact the daemon can produce.
-- The tenant boundary is the orgs/{orgId}/runs/{runId}/ prefix, which is
-- checked literally below and is the same prefix the presigned PUT is scoped
-- to; the tail is only a name.
--
-- Both tail segments must START with an alphanumeric. S3 keys are opaque, so
-- '..' cannot escape the prefix in the object store itself - but these keys get
-- joined onto filesystem paths downstream (report bundling, artifact download),
-- and '{runId}/../shot.png' is one segment deep and would otherwise match. The
-- same rule drops '.', a bare '..' name and dotfiles.

-- +goose Up

-- Exactly the members of execution-result@1 Artifact.kind. No 'other' escape
-- hatch: the schema is the source of truth, and a kind the contract cannot
-- express must fail at ingest rather than be filed under a bucket no consumer
-- knows how to render.
CREATE TYPE artifact_kind AS ENUM (
    'screenshot', 'video', 'trace', 'network', 'console', 'dom', 'report'
);

-- finding@1 AnalyzedBy.provider.
CREATE TYPE agent_provider AS ENUM ('claude', 'opencode', 'antigravity');

CREATE TABLE artifacts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid          NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    run_id       uuid          NOT NULL,
    -- Both optional: a run-level artifact (e.g. the discovery HAR) has neither.
    execution_id uuid,
    test_case_id uuid,
    kind         artifact_kind NOT NULL,
    name         text          NOT NULL CHECK (name ~ '^[A-Za-z0-9._-]{1,200}$'),
    storage_key  text          NOT NULL,
    content_type text          NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   bigint        CHECK (size_bytes IS NULL OR size_bytes >= 0),
    sha256       bytea         CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    created_at   timestamptz   NOT NULL DEFAULT now(),

    CONSTRAINT artifacts_org_id_id_key UNIQUE (org_id, id),
    -- One row per object. Also the idempotency guard for a re-uploaded
    -- artifact after a daemon reconnect.
    CONSTRAINT artifacts_storage_key_key UNIQUE (storage_key),
    CONSTRAINT artifacts_storage_key_layout CHECK (
        storage_key ~ (
            '^orgs/' || org_id::text
            || '/runs/[0-9]{4}-[0-9]{2}-[0-9]{2}/' || run_id::text
            || '/([A-Za-z0-9][A-Za-z0-9._-]{0,199}/)?'
            || '[A-Za-z0-9][A-Za-z0-9._-]{0,199}$'
        )
    ),
    CONSTRAINT artifacts_org_run_fkey
        FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id) ON DELETE CASCADE,
    CONSTRAINT artifacts_org_execution_fkey
        FOREIGN KEY (org_id, execution_id) REFERENCES executions (org_id, id) ON DELETE CASCADE,
    CONSTRAINT artifacts_org_test_case_fkey
        FOREIGN KEY (org_id, test_case_id) REFERENCES test_cases (org_id, id) ON DELETE SET NULL (test_case_id)
);

CREATE INDEX artifacts_org_run_idx ON artifacts (org_id, run_id);
CREATE INDEX artifacts_org_execution_idx ON artifacts (org_id, execution_id)
    WHERE execution_id IS NOT NULL;

CREATE TABLE findings (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid          NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    run_id       uuid          NOT NULL,
    execution_id uuid,
    test_case_id uuid,
    -- Step the analyst blamed, 0-based; NULL for a whole-case finding.
    step_index   integer       CHECK (step_index IS NULL OR step_index >= 0),
    failure_class failure_class NOT NULL,
    -- One-line headline for the report list; the full reasoning is root_cause.
    summary      text          NOT NULL DEFAULT '' CHECK (length(summary) <= 500),
    root_cause   text          NOT NULL DEFAULT '',
    -- double precision, not numeric: this is a model score that is only ever
    -- compared and rendered, never summed, and float64 keeps sqlc's generated
    -- signature usable without a pgtype.Numeric dance.
    confidence   double precision NOT NULL DEFAULT 0
        CHECK (confidence >= 0 AND confidence <= 1),
    suggested_fix text         NOT NULL DEFAULT '',
    -- Which AI CLI produced this verdict. Kept because a finding is a model
    -- output: comparing precision across providers (LONG-19) is impossible
    -- once the attribution has been dropped on ingest.
    analyzed_by_provider agent_provider,
    analyzed_by_version  text     NOT NULL DEFAULT '',
    created_at   timestamptz   NOT NULL DEFAULT now(),
    updated_at   timestamptz   NOT NULL DEFAULT now(),

    CONSTRAINT findings_org_id_id_key UNIQUE (org_id, id),
    -- One finding per execution: re-analysis updates in place instead of
    -- stacking duplicates every time a report is regenerated.
    CONSTRAINT findings_execution_id_key UNIQUE (execution_id),
    CONSTRAINT findings_org_run_fkey
        FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id) ON DELETE CASCADE,
    CONSTRAINT findings_org_execution_fkey
        FOREIGN KEY (org_id, execution_id) REFERENCES executions (org_id, id) ON DELETE CASCADE,
    CONSTRAINT findings_org_test_case_fkey
        FOREIGN KEY (org_id, test_case_id) REFERENCES test_cases (org_id, id) ON DELETE SET NULL (test_case_id)
);

CREATE INDEX findings_org_run_idx ON findings (org_id, run_id);
-- Phase 6: "which failure classes dominate this project?"
CREATE INDEX findings_org_failure_class_created_at_idx
    ON findings (org_id, failure_class, created_at DESC);

CREATE TRIGGER findings_set_updated_at
    BEFORE UPDATE ON findings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Which artifacts back a finding. A join table rather than a uuid[] column so
-- that the report can fetch finding + evidence in ONE join instead of a
-- per-finding artifact lookup, and so a deleted artifact cannot leave a
-- dangling id behind.
CREATE TABLE finding_evidence (
    org_id      uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    finding_id  uuid        NOT NULL,
    artifact_id uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (finding_id, artifact_id),
    CONSTRAINT finding_evidence_org_finding_fkey
        FOREIGN KEY (org_id, finding_id) REFERENCES findings (org_id, id) ON DELETE CASCADE,
    CONSTRAINT finding_evidence_org_artifact_fkey
        FOREIGN KEY (org_id, artifact_id) REFERENCES artifacts (org_id, id) ON DELETE CASCADE
);

CREATE INDEX finding_evidence_org_artifact_idx ON finding_evidence (org_id, artifact_id);

-- +goose Down

DROP TABLE IF EXISTS finding_evidence;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS artifacts;
DROP TYPE IF EXISTS agent_provider;
DROP TYPE IF EXISTS artifact_kind;
