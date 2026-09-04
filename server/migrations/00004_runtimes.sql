-- Runtimes (the machines running the QA daemon) and the two credential tables
-- that let a daemon join an organization.
--
-- Pairing flow: a member creates a short-lived pairing code in the UI, runs
-- `qa-agent pair <code>` on the target machine, and the backend exchanges the
-- code for a long-lived runtime token. The org of a daemon is ALWAYS derived
-- from the token row — never from anything the daemon sends over the wire.

-- +goose Up

CREATE TABLE runtimes (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name         text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    version      text        NOT NULL DEFAULT '',
    -- Capability report from the daemon `hello` frame (contract D). Kept as
    -- jsonb rather than side tables: the shape is owned by qa-schema and the
    -- backend only ever renders it, never queries inside it.
    browsers     jsonb       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(browsers) = 'array'),
    agents       jsonb       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(agents) = 'array'),
    -- `online` in the API is derived from this, not stored: a daemon that is
    -- killed cannot write a status column on its way out.
    last_seen_at timestamptz,
    disabled_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT runtimes_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT runtimes_org_id_name_key UNIQUE (org_id, name)
);

CREATE INDEX runtimes_org_id_last_seen_at_idx ON runtimes (org_id, last_seen_at DESC);

CREATE TRIGGER runtimes_set_updated_at
    BEFORE UPDATE ON runtimes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE runtime_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    runtime_id   uuid        NOT NULL,
    -- SHA-256 of the bearer token handed to the daemon. The token itself is
    -- shown exactly once, at pairing time, and is not recoverable from here.
    token_hash   bytea       NOT NULL CHECK (octet_length(token_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz,

    CONSTRAINT runtime_tokens_token_hash_key UNIQUE (token_hash),
    CONSTRAINT runtime_tokens_org_runtime_fkey
        FOREIGN KEY (org_id, runtime_id) REFERENCES runtimes (org_id, id) ON DELETE CASCADE
);

CREATE INDEX runtime_tokens_org_id_runtime_id_idx ON runtime_tokens (org_id, runtime_id);

CREATE TABLE pairing_codes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- SHA-256 of the one-time code. A timing-safe lookup is a plain index hit
    -- on the hash; the plaintext code is never stored.
    code_hash   bytea       NOT NULL CHECK (octet_length(code_hash) = 32),
    -- Set when the code is redeemed. `consumed_at IS NULL` plus the partial
    -- unique index below is what makes redemption single-use.
    runtime_id  uuid,
    created_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pairing_codes_code_hash_key UNIQUE (code_hash),
    CONSTRAINT pairing_codes_consumed_together CHECK (
        (consumed_at IS NULL) = (runtime_id IS NULL)
    ),
    CONSTRAINT pairing_codes_org_runtime_fkey
        FOREIGN KEY (org_id, runtime_id) REFERENCES runtimes (org_id, id) ON DELETE CASCADE
);

CREATE INDEX pairing_codes_expires_at_idx ON pairing_codes (expires_at)
    WHERE consumed_at IS NULL;

-- +goose Down

DROP TABLE IF EXISTS pairing_codes;
DROP TABLE IF EXISTS runtime_tokens;
DROP TABLE IF EXISTS runtimes;
