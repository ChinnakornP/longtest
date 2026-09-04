-- Tenancy root: organizations, users, memberships and login sessions.
--
-- `users` is deliberately GLOBAL (no org_id): one person can belong to several
-- organizations with a different role in each, and the e-mail has to be unique
-- across the install so that login works before an org is chosen.
-- `memberships` is the only bridge, and it is what the auth middleware checks
-- against the `X-Org-ID` header on every request.

-- +goose Up

CREATE TYPE membership_role AS ENUM ('owner', 'admin', 'member', 'viewer');

CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    -- citext + UNIQUE: "Acme" and "acme" must not be two tenants.
    slug       citext      NOT NULL CHECK (slug ~ '^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT organizations_slug_key UNIQUE (slug)
);

CREATE TRIGGER organizations_set_updated_at
    BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext      NOT NULL CHECK (position('@' IN email) > 1),
    -- Hash only. The plaintext password never reaches this database, and the
    -- hash format (algorithm + parameters) is owned by the auth task.
    password_hash text        NOT NULL CHECK (length(password_hash) > 0),
    name          text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_email_key UNIQUE (email)
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE memberships (
    org_id     uuid            NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id    uuid            NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       membership_role NOT NULL,
    created_at timestamptz     NOT NULL DEFAULT now(),
    updated_at timestamptz     NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, user_id)
);

-- "which orgs can this user pick?" runs on every request that carries a
-- session; the primary key is org-first so it cannot serve that lookup.
CREATE INDEX memberships_user_id_idx ON memberships (user_id);

CREATE TRIGGER memberships_set_updated_at
    BEFORE UPDATE ON memberships
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The cookie carries a random secret; only its SHA-256 is stored, so a
    -- dump of this table cannot be replayed as a login.
    token_hash   bytea       NOT NULL CHECK (octet_length(token_hash) = 32),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT sessions_token_hash_key UNIQUE (token_hash)
);

-- Session revocation on "log out of every device" and the expiry sweep.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at)
    WHERE revoked_at IS NULL;

-- +goose Down

DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;
DROP TYPE IF EXISTS membership_role;
