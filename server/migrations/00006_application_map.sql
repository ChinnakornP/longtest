-- The Application Map (contract B): what the platform knows about the target
-- application, accumulated across runs.
--
-- This is the durable half of the product's moat, so it is normalised into
-- tables rather than kept as one `application_map` jsonb blob per project:
-- the map is read per page, updated per element, and has to answer "what
-- changed since the last run?" without rewriting a megabyte of JSON.
--
-- Drift handling: discovery never deletes. It stamps `last_seen_run_id` on
-- everything it observes, and anything whose stamp falls behind the project's
-- latest discovery run is stale and can be filtered out at read time. A
-- delete-and-reinsert map would lose element ids that live test cases refer to.

-- +goose Up

CREATE TABLE pages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id      uuid        NOT NULL,
    -- Stable identifier used inside the map and by test steps ("page.employees").
    ref             text        NOT NULL CHECK (length(btrim(ref)) BETWEEN 1 AND 200),
    -- Route as seen from base_url, always rooted.
    path            text        NOT NULL CHECK (path LIKE '/%'),
    title           text        NOT NULL DEFAULT '',
    auth_required   boolean     NOT NULL DEFAULT false,
    first_seen_run_id uuid,
    last_seen_run_id  uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pages_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT pages_project_id_path_key UNIQUE (project_id, path),
    CONSTRAINT pages_project_id_ref_key UNIQUE (project_id, ref),
    CONSTRAINT pages_org_project_fkey
        FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT pages_org_last_seen_run_fkey
        FOREIGN KEY (org_id, last_seen_run_id) REFERENCES runs (org_id, id)
        ON DELETE SET NULL (last_seen_run_id),
    CONSTRAINT pages_org_first_seen_run_fkey
        FOREIGN KEY (org_id, first_seen_run_id) REFERENCES runs (org_id, id)
        ON DELETE SET NULL (first_seen_run_id)
);

CREATE INDEX pages_org_id_project_id_idx ON pages (org_id, project_id);

CREATE TRIGGER pages_set_updated_at
    BEFORE UPDATE ON pages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE elements (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    page_id    uuid        NOT NULL,
    -- The handle test steps use ("emp.btn.add"). Test cases reference elements
    -- by ref, never by a CSS selector the planner invented: that is what makes
    -- execution deterministic when the page markup shifts.
    ref        text        NOT NULL CHECK (length(btrim(ref)) BETWEEN 1 AND 200),
    kind       text        NOT NULL DEFAULT '',
    label      text        NOT NULL DEFAULT '',
    -- Ordered locator fallback chain (testId -> role+name -> label -> css),
    -- shape owned by qa-schema's application-map@1.
    locators   jsonb       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(locators) = 'array'),
    last_seen_run_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT elements_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT elements_page_id_ref_key UNIQUE (page_id, ref),
    CONSTRAINT elements_org_page_fkey
        FOREIGN KEY (org_id, page_id) REFERENCES pages (org_id, id) ON DELETE CASCADE,
    CONSTRAINT elements_org_last_seen_run_fkey
        FOREIGN KEY (org_id, last_seen_run_id) REFERENCES runs (org_id, id)
        ON DELETE SET NULL (last_seen_run_id)
);

CREATE INDEX elements_org_id_page_id_idx ON elements (org_id, page_id);

CREATE TRIGGER elements_set_updated_at
    BEFORE UPDATE ON elements
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE workflows (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id       uuid        NOT NULL,
    ref              text        NOT NULL CHECK (length(btrim(ref)) BETWEEN 1 AND 200),
    name             text        NOT NULL DEFAULT '',
    -- Ordered page/element refs. Kept as jsonb rather than a workflow_nodes
    -- table: it is always read and written whole, and nothing joins into it.
    path             jsonb       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(path) = 'array'),
    expected_outcome text        NOT NULL DEFAULT '',
    last_seen_run_id uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workflows_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT workflows_project_id_ref_key UNIQUE (project_id, ref),
    CONSTRAINT workflows_org_project_fkey
        FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT workflows_org_last_seen_run_fkey
        FOREIGN KEY (org_id, last_seen_run_id) REFERENCES runs (org_id, id)
        ON DELETE SET NULL (last_seen_run_id)
);

CREATE INDEX workflows_org_id_project_id_idx ON workflows (org_id, project_id);

CREATE TRIGGER workflows_set_updated_at
    BEFORE UPDATE ON workflows
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down

DROP TABLE IF EXISTS workflows;
DROP TABLE IF EXISTS elements;
DROP TABLE IF EXISTS pages;
