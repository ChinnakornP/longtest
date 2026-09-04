# REST API reference

The conventions every route in this API follows — authentication, tenancy,
roles, the error envelope — with the auth and organization surface as the
worked example.

**The route-by-route reference is [`api/openapi.yaml`](api/openapi.yaml).** It
covers every endpoint including projects, runs, test cases, runtimes, reports
and both WebSockets, and it is checked against the router by
`TestOpenAPICoversEveryMountedRoute`: a route mounted without an entry there, or
an entry with no route, fails the build.

Base path: `/api/v1`.

## Conventions

### Authentication

A session is an httpOnly cookie named `qa_session`, set by `signup` and `login`.
It is `SameSite=Lax`, `Secure` (off only for local http development, see
`SESSION_COOKIE_SECURE`), and has an absolute 7-day lifetime with no sliding
renewal. Browsers must send `credentials: "include"`.

Daemons authenticate with `Authorization: Bearer qart_…` instead. The two
credentials are not interchangeable: a session cannot act as a runtime and a
runtime token cannot act as a user.

### Tenancy

Every request that touches organization data carries:

```
X-Org-ID: <uuid>
```

This header is the **only** source of the active organization. Middleware
verifies membership and puts `(userID, orgID, role)` into the request context;
no handler reads an org id from a body, a query string, or a path. Where a route
also carries `{orgID}` in its path, that segment is an assertion checked against
the header — see [ADR-007](adr/0007-org-id-in-path-is-an-assertion.md).

A missing header, an unparsable one, an organization that does not exist, and an
organization the caller does not belong to are all **403**. A resource id that
belongs to another organization is **404**, because the org-scoped query does
not find it.

### Roles

| Role     | May                                                             |
| -------- | --------------------------------------------------------------- |
| `viewer` | read everything in the organization                              |
| `member` | + create projects and start runs                                 |
| `admin`  | + manage members, invites and runtimes                           |
| `owner`  | + transfer or delete the organization, invite another owner      |

The check is "at least this role", so `RequireRole(RoleMember)` admits members,
admins and owners. Nobody may invite somebody to a role above their own.

### Error envelope

Every failure, on every route, is:

```json
{ "error": { "code": "validation_failed",
             "message": "the request body failed validation",
             "details": { "fields": { "email": "must be an e-mail address" } } } }
```

`code` is stable and machine-readable; `message` is safe to show a user.
`details` is optional. A 5xx never carries a driver message, a SQL fragment or a
constraint name — those are logged with the request id instead.

Codes: `bad_request`, `validation_failed`, `unauthorized`, `forbidden`,
`not_found`, `conflict`, `unsupported_media_type`, `payload_too_large`,
`rate_limited`, `timeout`, `unavailable`, `internal`.

Every response carries `X-Request-ID`; quote it in a bug report and the server
log line is one grep away.

## Endpoints

| Method   | Path                                           | Auth              | Success | Failures                     |
| -------- | ---------------------------------------------- | ----------------- | ------- | ---------------------------- |
| `POST`   | `/api/v1/auth/signup`                          | none              | 201     | 409, 415, 422                |
| `POST`   | `/api/v1/auth/login`                           | none              | 200     | 400, 401, 415, 422           |
| `POST`   | `/api/v1/auth/logout`                          | cookie (optional) | 204     | —                            |
| `GET`    | `/api/v1/me`                                   | session           | 200     | 401                          |
| `POST`   | `/api/v1/orgs`                                 | session           | 201     | 401, 409, 422                |
| `GET`    | `/api/v1/orgs/{orgID}/members`                 | session + org, `viewer` | 200 | 401, 403                  |
| `POST`   | `/api/v1/orgs/{orgID}/invites`                 | session + org, `admin`  | 201 | 401, 403, 409, 422        |
| `GET`    | `/api/v1/orgs/{orgID}/invites`                 | session + org, `admin`  | 200 | 401, 403                  |
| `DELETE` | `/api/v1/orgs/{orgID}/invites/{inviteID}`      | session + org, `admin`  | 204 | 401, 403, 404             |
| `POST`   | `/api/v1/invites/accept`                       | session           | 200     | 401, 403, 404, 409, 422      |
| `POST`   | `/api/v1/orgs/{orgID}/runtimes/pair`           | session + org, `admin`  | 201 | 401, 403                  |
| `POST`   | `/api/v1/runtimes/redeem`                      | none (pairing code) | 201   | 404, 409, 422                |
| `GET`    | `/healthz`                                     | none              | 200     | —                            |
| `GET`    | `/readyz`                                      | none              | 200     | 503 when the database is down |

### Notable bodies

`POST /api/v1/auth/signup`

```json
{ "email": "a@example.com", "password": "at least 12 characters",
  "name": "A Person", "orgName": "Acme QA" }
→ 201 { "user": {...}, "org": {...}, "role": "owner" }   + Set-Cookie: qa_session
```

The account and its first organization are created in one transaction; the
creator is always the owner.

`GET /api/v1/me`

```json
{ "user": { "id": "...", "email": "...", "name": "...", "createdAt": "..." },
  "orgs": [ { "id": "...", "name": "...", "slug": "...", "role": "admin" } ] }
```

`orgs` may be empty — a user invited to an organization has an account before
they have a membership.

`POST /api/v1/orgs/{orgID}/invites`

```json
{ "email": "colleague@example.com", "role": "member" }
→ 201 { "id": "...", "email": "...", "role": "member",
        "expiresAt": "...", "createdAt": "...", "token": "<shown once>" }
```

The token is returned exactly once and is not stored in plaintext; delivering it
is out of scope for the MVP (no e-mail). Re-inviting the same address revokes
the previous invite and issues a new token. `POST /api/v1/invites/accept` takes
`{"token": "..."}` and may only be called by the account whose e-mail the invite
names; it never downgrades an existing membership.

`POST /api/v1/orgs/{orgID}/runtimes/pair`

```json
→ 201 { "pairingCode": "K7Q2-9FMR-3XT8", "expiresAt": "..." }
```

Single use, 15-minute TTL. Case and separators are ignored when it is redeemed.

`POST /api/v1/runtimes/redeem`

```json
{ "pairingCode": "K7Q2-9FMR-3XT8", "runtimeName": "qa-macbook",
  "hostInfo": { "hostname": "qa-macbook", "os": "darwin",
                "arch": "arm64", "version": "0.3.1" } }
→ 201 { "runtimeId": "...", "runtimeToken": "qart_…",
        "runtimeName": "qa-macbook", "orgId": "..." }
```

Unauthenticated, because a fresh daemon holds nothing but the code. The
organization comes from the code, never from the body. `runtimeToken` is shown
once — a daemon that loses it must be paired again. Redeeming a spent or expired
code is a 404; a runtime name already used in that organization is a 409.

## What is deliberately missing

- **Rate limiting.** `login` and `runtimes/redeem` are unauthenticated and
  should be throttled at the edge (or in a later issue) before this is exposed
  to the internet. The credentials themselves are brute-force resistant —
  argon2id, and a 60-bit pairing code with a 15-minute TTL — but the CPU cost of
  an argon2 verification is a denial-of-service surface on its own.
- **A CSRF token.** `SameSite=Lax` blocks the cross-site POST, the CORS
  allowlist is exact-match with credentials, and `X-Org-ID` forces a preflight
  on every state-changing organization route. A token becomes necessary if a
  state-changing `GET` is ever added, or if `SameSite` is relaxed.
- **Password reset, e-mail delivery, OAuth/SSO, MFA.** Out of scope for the MVP
  by the task contract.
- **`405` in the envelope.** A known path with the wrong method is answered by
  `net/http`'s `ServeMux` with its own plain-text body. Every other failure uses
  the envelope above.
