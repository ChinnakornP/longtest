-- The first tenant-scoped table. Everything the platform learns about a target
-- web application hangs off a project.

-- +goose Up

CREATE TABLE projects (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    -- The application under test. Scheme is required so the daemon never has
    -- to guess http vs https when it hands the URL to the browser.
    base_url   text        NOT NULL CHECK (base_url ~ '^https?://'),
    archived_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Composite-FK target: see the conventions in 00001.
    CONSTRAINT projects_org_id_id_key UNIQUE (org_id, id),
    -- Makes `POST /projects` idempotent on retry without an extra key column.
    CONSTRAINT projects_org_id_name_key UNIQUE (org_id, name)
);

CREATE INDEX projects_org_id_created_at_idx ON projects (org_id, created_at DESC);

CREATE TRIGGER projects_set_updated_at
    BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down

DROP TABLE IF EXISTS projects;
