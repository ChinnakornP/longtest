-- The fixture registry: the names of the logins a project's runs can
-- establish, and nothing else.
--
-- This table is the reason a planner can write
-- `preconditions: ["fixture:logged_in_as_admin"]` and cannot write a password.
-- It holds NO credential column, by design and not by omission: the values
-- live in the daemon's sealed FixtureStore (see daemon/security/fixtures.go),
-- on the operator's own machine, under a key this backend never sees. A dump
-- of this database is therefore a list of names.
--
-- What it buys the planner: a `preconditions` entry naming anything that is
-- not registered here is a plan the ingest refuses. Without the registry the
-- server can only check the *shape* of a precondition, which lets a hijacked
-- planner invent `fixture:logged_in_as_root` and have it stored as a real
-- test case waiting for a reviewer to approve.

-- +goose Up

CREATE TABLE project_fixtures (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id  uuid        NOT NULL,
    -- Must match the Precondition pattern in test-case.schema.json, so a name
    -- that is storable here is a name a planner can legally emit.
    name        text        NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
    -- What the login is for, shown to a reviewer. Never a value.
    description text        NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT project_fixtures_org_id_id_key UNIQUE (org_id, id),
    -- Makes registering a fixture idempotent on retry without a second key.
    CONSTRAINT project_fixtures_project_id_name_key UNIQUE (project_id, name),
    CONSTRAINT project_fixtures_org_project_fkey
        FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE
);

-- The plan-ingest read: "every fixture name this project has", issued once per
-- planning result.
CREATE INDEX project_fixtures_org_project_idx ON project_fixtures (org_id, project_id);

CREATE TRIGGER project_fixtures_set_updated_at
    BEFORE UPDATE ON project_fixtures
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down

DROP TABLE IF EXISTS project_fixtures;
