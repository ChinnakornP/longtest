# ADR-008: apps/web ships no backend

- **Status:** Accepted — 2026-09-04
- **Affected:** `apps/web` (LONG-9), `server/internal/{auth,org}` (LONG-7),
  `.github/workflows/ci.yml`, LONG-8, LONG-19
- **Related:** [ADR-006](0006-multi-tenant-from-day-one.md) — that ADR defines
  the auth/tenancy contract; this one governs where it is allowed to run.

## Context

T05 (`server/internal/auth`, `server/internal/org` — LONG-7) had not landed
when T07 (LONG-9, the web app shell) needed a working signup/login/org flow to
build and test against. T07 added a full in-memory mock of the T05 contract as
Next.js App Router route handlers under `apps/web/src/app/api/v1/**`, so the
UI could ship and be demoed today.

A security review of the resulting PR verified that these route handlers
compile into the production build of `apps/web` exactly like any other route
— `next build` followed by `next start` accepted signup/login requests and
issued session cookies at the same origin as the dashboard, with no
difference between dev and prod. That makes `apps/web` a second,
unauthenticated auth backend the moment it is deployed: anyone can create an
account and org directly against the web origin, bypassing the Go backend,
colliding with the `qa_session` cookie name reserved for T05, and exposing an
unbounded in-memory store and a synchronous `scryptSync` login path to the
internet as a denial-of-service vector. This was still free to fix because no
deploy image exists yet (T12); once one does, the mock would bake into it
unnoticed.

## Decision

`apps/web` is a client of the Go backend only. **No production build of
`apps/web` may contain a server route handler under `/api/**`, under any
circumstance** — not gated by an environment variable, not disabled by a
runtime check. The boundary is enforced at build time, not at request time,
because a route that was never compiled cannot be reopened by a
misconfigured `NODE_ENV` or a forgotten env var on the deploy host.

**Mechanism:**

- Files that implement the T07 mock backend are named `route.mock.ts`, not
  `route.ts`. `apps/web/next.config.ts` includes `mock.ts`/`mock.tsx` in
  `pageExtensions` only when `NODE_ENV !== 'production'`; a production build
  resolves routes from `.ts`/`.tsx` only, so `route.mock.ts` files do not
  exist as far as the App Router is concerned.
- `apps/web/src/lib/api/client.ts` requires `NEXT_PUBLIC_API_BASE_URL` to be
  set whenever `NODE_ENV === 'production'`; an unset value fails the
  build/boot instead of silently resolving to same-origin, which is what made
  the shadow backend reachable without anyone configuring it.
- CI's `security` job builds `apps/web` and asserts
  `apps/web/.next/app-path-routes-manifest.json` contains no `/api/*` entry,
  failing the pipeline if it does. This checks the actual build output, not
  the intent behind it.

## Alternatives considered

- **Runtime env gate** (each handler returns 404 unless a dev flag is set).
  Rejected: the route still exists in the shipped bundle — a misconfigured or
  missing env var on the deploy host silently reopens it, which is exactly
  the failure mode the review flagged.
- **MSW (Mock Service Worker) instead of route handlers.** More
  architecturally correct — mocks would never be routable at all — but a full
  rewrite of the mock layer for something that is deleted the moment T05
  ships. Not worth it for the mock's remaining lifetime.

## Consequences

- The T07 mock backend (`apps/web/src/mocks/**`, `apps/web/src/app/api/v1/**`)
  only runs under `next dev`. `next build`/`next start` never expose it,
  regardless of environment configuration.
- Any E2E suite that runs against a production build (`next build` +
  `next start`) must talk to a real Go backend — it cannot rely on the mock.
  This applies directly to T06 and T17.
- Whoever lands `server/internal/{auth,org}` (T05) deletes
  `apps/web/src/mocks/**`, `apps/web/src/app/api/v1/**`, and the
  `pageExtensions` line in `next.config.ts`, and points
  `NEXT_PUBLIC_API_BASE_URL` at the real backend, in the same change that
  switches the frontend over. The CI guard and this ADR stay in place
  permanently — they are the boundary now, not a statement about the mock.
