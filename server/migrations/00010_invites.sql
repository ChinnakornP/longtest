-- Organization invites: the only way a second person joins an org.
--
-- Like `sessions` and `runtime_tokens` this is a TENANCY table, not a domain
-- table: it is one of the ways an org_id is established for a user, so the
-- accept path looks a row up by token hash alone and cannot take an org_id
-- parameter (see internal/db/orgscope_test.go).
--
-- The token itself is never stored. It is shown once, in the response to
-- POST /api/v1/orgs/{id}/invites, and reaches the invitee out of band; e-mail
-- delivery is deliberately out of scope for the MVP.

-- +goose Up

CREATE TABLE invites (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid            NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- citext, like users.email: an invite for "Bob@x.com" must be accepted by
    -- the account registered as "bob@x.com".
    email       citext          NOT NULL CHECK (position('@' IN email) > 1),
    role        membership_role NOT NULL,
    -- SHA-256 of the invite token. A dump of this table cannot be replayed as
    -- an invite acceptance.
    token_hash  bytea           NOT NULL CHECK (octet_length(token_hash) = 32),
    invited_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    expires_at  timestamptz     NOT NULL,
    accepted_at timestamptz,
    accepted_by uuid REFERENCES users (id) ON DELETE SET NULL,
    revoked_at  timestamptz,
    created_at  timestamptz     NOT NULL DEFAULT now(),
    updated_at  timestamptz     NOT NULL DEFAULT now(),

    CONSTRAINT invites_token_hash_key UNIQUE (token_hash),
    -- An invite is accepted by exactly one account, or by none at all.
    CONSTRAINT invites_accepted_together CHECK (
        (accepted_at IS NULL) = (accepted_by IS NULL)
    ),
    -- An owner invite is the only way to hand over an org, so it is allowed;
    -- the role is otherwise constrained by the service layer, which refuses to
    -- let an admin mint an owner.
    CONSTRAINT invites_org_id_id_key UNIQUE (org_id, id)
);

-- At most one live invite per (org, e-mail): re-inviting someone rotates the
-- token rather than leaving two valid ones outstanding. The partial index is
-- what makes "resend" idempotent instead of a guess.
CREATE UNIQUE INDEX invites_org_id_email_live_idx ON invites (org_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- The org's invite list, newest first.
CREATE INDEX invites_org_id_created_at_idx ON invites (org_id, created_at DESC);

-- The expiry sweep.
CREATE INDEX invites_expires_at_idx ON invites (expires_at)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TRIGGER invites_set_updated_at
    BEFORE UPDATE ON invites
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down

DROP TABLE IF EXISTS invites;
