# ADR-007: An org id in a path is an assertion, never a source

- **Status:** Accepted — 2026-09-04; amended 2026-09-05 (LONG-24), see
  [Amendment](#amendment-2026-09-05--the-guarantee-is-now-in-the-type-system)
- **Affected:** `server/internal/auth` (middleware), `server/internal/org`,
  every org-scoped route added by LONG-10, `apps/web` (LONG-9), LONG-13
- **Related:** [ADR-006](0006-multi-tenant-from-day-one.md) — refines it, does
  not supersede it

## Context

ADR-006 froze the access model as "httpOnly session cookie plus an `X-Org-ID`
header", and rejected `/orgs/{id}/…` as an alternative, because a handler that
reads the org id out of its own path is one forgotten check away from a
cross-tenant leak.

The endpoint contract agreed for LONG-7 nevertheless names path-scoped routes:

```
GET  /api/v1/orgs/{id}/members
POST /api/v1/orgs/{id}/invites
POST /api/v1/orgs/{id}/runtimes/pair
```

These are the URLs the web app is being built against, and they are the shape
a REST client expects. Read literally, they contradict ADR-006. Read as URLs
alone, they say nothing about where the server gets its tenant from.

Two failure modes matter here, and they are different:

1. **A handler trusts the path.** This is what ADR-006 rejected, and nothing
   below changes that.
2. **A client's URL and its active organization disagree.** The web app keeps
   the active org in a store and builds URLs from a route parameter; a user who
   switches organizations in one tab and clicks a stale link in another can
   produce a request whose header says A and whose path says B. Silently
   honouring the header there means acting on A while the user is looking at a
   page about B.

## Decision

The `X-Org-ID` header remains the **only** source of the active organization.
A path segment is an **assertion about** that organization, checked and then
discarded.

Concretely:

- `auth.RequireOrg` resolves the scope from the header and verifies membership,
  exactly as ADR-006 specifies. It runs first.
- `auth.RequireOrgMatchesPath("orgID")` then compares the path segment with the
  already-resolved scope and answers **403** if they differ.
- Handlers obtain the org id only from `auth.MustOrgScope(ctx)`. An
  `auth.OrgScope` has no exported field and no exported constructor, so a
  handler cannot build one from a path, a body or a query string even by
  accident: the struct literal that would do it does not compile.

The header is still what the middleware trusts; the path can only ever narrow
the set of requests that are served, never widen it.

## Alternatives considered

- **Drop the path segment: `GET /api/v1/members`.** Strictly the safest, and
  what ADR-006 implies. Rejected because it makes every URL meaningless out of
  context — a link in a bug report, a browser history entry, a server log line
  no longer says which organization it was about — and because the contract the
  web and daemon tasks are building against already carries the segment.
- **Read the org from the path and drop the header.** Rejected for the reason
  ADR-006 gives: it puts the tenancy decision in as many places as there are
  handlers.
- **Accept the header and ignore a mismatched path.** Rejected: it turns a
  stale tab into a silent write against the wrong tenant, which is precisely
  the class of bug tenancy checks exist to prevent.
- **Answer 404 on a mismatch.** Rejected: the caller is a member of the
  organization in the header, so there is no id being disclosed. 403 says what
  actually happened. A resource id belonging to *another* organization is still
  a 404, because the org-scoped query genuinely does not find it.

## Consequences

- The web app must send `X-Org-ID` on every request AND keep it consistent with
  the URL it is calling. A mismatch is a client bug and shows up as a 403.
- Every org-scoped route registers `RequireOrgMatchesPath` after `RequireOrg`.
  A route that forgets it is not a leak — the header still decides — but it
  loses the stale-tab guard, so the pairing is part of the route template
  LONG-10 copies.
- ADR-006 stands unchanged: no handler reads an org id from a request.

## Amendment 2026-09-05 — the guarantee is now in the type system

As first accepted, this ADR claimed there was "no exported constructor for an
`auth.OrgScope`". That was true of *functions* and false of the *type*: every
field of `OrgScope` was exported, so `auth.OrgScope{OrgID: idFromPath}`
compiled from any package. Nobody had written that line, but the guarantee was
a convention, and the route template stage 5–6 copies would have carried the
convention into every new handler.

The fields are now unexported and read through accessors:

```go
type OrgScope struct {
    caller Caller
    orgID  uuid.UUID
    role   Role
}

func (s OrgScope) OrgID() uuid.UUID { return s.orgID }
func (s OrgScope) Role() Role       { return s.role }
```

`auth.Caller` and `auth.RuntimeCaller` are sealed the same way, for the same
reason: forging a `Caller` names an arbitrary user, and forging a
`RuntimeCaller` names an arbitrary daemon. `WithCaller`, `WithOrgScope` and
`WithRuntimeCaller` are unexported too, so the middleware in `internal/auth` is
the only thing that can put a principal into a request context.

Outside `internal/auth` the only value of any of these types that can be
written down is the zero value, which names nothing and is refused by
`MustOrgScope` / `MustRuntimeCaller`. Three tests in
`server/internal/auth/seal_test.go` keep it that way, and each has been checked
to fail when the property is broken:

- `TestAuthPrincipalsAreSealed` — every field of all three types is unexported.
- `TestNoExportedConstructorForAPrincipal` — parses `internal/auth` and fails on
  any new exported function returning a principal that is not a documented
  context or credential lookup.
- `TestNoPackageOutsideAuthWritesAPrincipalLiteral` — no package outside
  `internal/auth` populates one of these literals.

### The one org id `auth` does not resolve

The run scheduler builds a `run.assign` frame for a run it has already claimed
from the queue. There is no request behind it, so there is no caller whose
membership `auth` could verify and no `OrgScope` to pass; the org id is the one
on the claimed row. That path used to fake a scope (`auth.OrgScope{OrgID:
claimed.OrgID}`) and now goes through `project.Service.SystemGet` /
`SystemApplicationMap`, which take the org id as a plain argument and are named
for the fact that no request chose it.

`TestSystemProjectReadsHaveNoRequestCallers` in `internal/project` allowlists
the single file allowed to call them (`internal/run/assign.go`) and fails on
any other caller, so a handler cannot use the system path to get back the org
id it is not allowed to read from a path.

Nothing about the tenancy model, the middleware chain or the HTTP contract
changed: the header is still the only source of the active organization, and a
mismatched path segment is still a 403.
