-- Bootstrap: extensions, shared helper functions and the conventions every
-- later migration relies on.
--
-- Conventions frozen here (see docs/adr and LONG-5 for the rationale):
--
--  1. Tenancy is physical, not advisory. Every table that holds customer data
--     carries `org_id uuid NOT NULL REFERENCES organizations(id)`. There is no
--     "add tenancy later" path — retrofitting it means rewriting every query.
--
--  2. Cross-table foreign keys inside a tenant are enforced with COMPOSITE
--     foreign keys `(org_id, parent_id) -> parent(org_id, id)`, not with
--     triggers. Every tenant table therefore also declares `UNIQUE (org_id, id)`
--     so it can be the target of one.
--     Why composite FK over a trigger:
--       * it is declarative, so Postgres enforces it on every write path
--         (migrations, psql, a future admin tool), not only on the ones that
--         happen to go through our Go code;
--       * a trigger that does `SELECT ... WHERE id = NEW.parent_id` races with
--         a concurrent UPDATE of the parent's org_id unless it takes a row
--         lock, which serialises inserts on hot parents;
--       * the extra `(org_id, id)` unique index is also the index the tenant
--         scoped read path wants anyway.
--     Cost: one extra unique index per table, and `org_id` has to be carried
--     on child rows. Both are accepted deliberately.
--
--  3. Rows are never hard-deleted by application code in the MVP; the CASCADE
--     rules here exist so that deleting an organization actually removes its
--     data (GDPR-style erasure) without an orphan sweep.
--
--  4. `updated_at` is maintained by a trigger, never by the application, so a
--     hand-written UPDATE in psql cannot leave a stale timestamp behind.

-- +goose Up

-- citext backs case-insensitive natural keys (user e-mail, org slug). Doing it
-- in the type rather than with lower() functional indexes keeps the sqlc query
-- text free of casts that are easy to forget on one code path.
CREATE EXTENSION IF NOT EXISTS citext;

-- gen_random_uuid() is built into Postgres 13+, so pgcrypto is not needed.

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION set_updated_at() IS
    'BEFORE UPDATE trigger: keeps updated_at authoritative regardless of writer.';

-- +goose Down

DROP FUNCTION IF EXISTS set_updated_at();
DROP EXTENSION IF EXISTS citext;
