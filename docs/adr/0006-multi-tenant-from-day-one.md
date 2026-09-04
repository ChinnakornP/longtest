# ADR-006: Multi-tenant from day one

- **Status:** Accepted — 2026-09-04
- **Affected:** migrations + sqlc (LONG-5), `server/internal/auth`,
  `server/internal/org` (LONG-7), every domain package (LONG-10),
  `apps/web` (LONG-9), `daemon/runtime` (LONG-11), LONG-14
- **Related:** [ADR-002](0002-daemon-outbound-only-presigned-artifacts.md)
- **Supersedes:** the single-workspace, no-login assumption in the original
  MVP plan (parent issue, "Open questions" #2)

## Context

The first architecture pass proposed a single-workspace MVP with no login and
one pairing token per runtime, deferring tenancy. The product owner chose
multi-tenant from the start.

Retrofitting tenancy is not a feature addition — it is a rewrite of every
query, every index, every artifact key, and every authorization check, plus a
data migration of whatever already exists. Deciding this before the first
migration is written costs a day; deciding it after stage 4 costs the stage.

## Decision

Tenancy exists in the **first** migration and in every layer above it.

- **Tables:** `organizations`, `users`, `memberships`, `sessions`,
  `runtime_tokens`, `pairing_codes`. Every domain table carries
  `org_id NOT NULL` with a foreign key, and org-scoped uniqueness
  (e.g. project name unique *within* an org).
- **Access model (frozen):** httpOnly session cookie plus an **`X-Org-ID`**
  request header. Middleware verifies the caller's membership in that org and
  puts `(userID, orgID, role)` into the request context. Handlers **must not**
  read an org id from the body, path, or query string.
- **Roles:** `owner | admin | member | viewer`.
- **Queries:** every sqlc query against a domain table takes `org_id` as a
  parameter. A lint/test in CI fails the build on any that does not — this is
  the primary cross-tenant guard of the MVP.
- **Daemons:** a one-time pairing code (TTL 15 minutes) is exchanged for a
  runtime token bound to an org. The daemon's org is **always** derived from
  its token; a value sent by the daemon is never trusted.
- **Storage:** artifact keys start with `orgs/{orgId}/`, and presigned URLs are
  scoped to a single run's prefix.

Postgres RLS is **not** enabled in the MVP; enforcement is query-level plus
tests. RLS remains available later as defense in depth without changing this
decision.

## Alternatives considered

- **Single-tenant MVP, add orgs later.** Rejected by the product owner, and
  correctly: the retrofit touches every table and every handler, and any data
  written before it becomes a migration problem.
- **Database- or schema-per-tenant.** Rejected: migration fan-out across
  tenants, connection-pool pressure, and no cross-tenant queries for internal
  operations. Shared tables with `org_id` fit the expected tenant count.
- **Org id in the URL path (`/orgs/{id}/…`).** Rejected: it is easy to forget
  in one handler, and a single forgetful handler is a cross-tenant leak. A
  header read exclusively by middleware gives exactly one place to get right.
- **RLS from day one.** Deferred, not rejected: it costs a role/connection
  model in the daemon-facing and migration paths before we have the tests that
  would tell us it works.

## Consequences

- Stage 3 gains a dedicated auth/tenancy issue (LONG-7) that lands before the
  domain API, and LONG-5 writes tenancy tables in the first migration.
- Every endpoint needs both an authorization test and a **cross-tenant
  negative test** — org A must receive 404, not 403, for org B's resources.
- The web app must carry `X-Org-ID` on every request and expose an org
  switcher (LONG-9).
- Local development needs a seeded org and user; the fixture flow cannot
  assume anonymous access.
- Existing contracts are unaffected in shape: `org_id` is derived server-side
  and does not appear in REST request bodies or the daemon envelope.
