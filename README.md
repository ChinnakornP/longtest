# AI QA Agent Platform

Point it at a URL. It explores the application, builds a map of it, plans test
cases, runs them in a real browser, and explains what broke.

```
Next.js web  →  Go backend  →  QA daemon (your machine)  →  Playwright + AI CLI
```

The daemon runs on your own hardware, so the platform can test
`http://localhost:3000` or `staging.internal` without exposing either to the
internet. The AI never drives the browser directly: it plans and it reasons,
Playwright executes.

## Requirements

| Tool           | Version           |
| -------------- | ----------------- |
| Docker         | with Compose v2   |
| Go             | 1.26+             |
| Node.js        | 22.12+ (24 in CI) |
| pnpm           | 11+               |

## Getting started

```bash
git clone https://github.com/ChinnakornP/longtest.git
cd longtest

cp .env.example .env     # local dev values, git-ignored
make up                  # postgres + minio, bucket qa-artifacts

pnpm install
make lint
make test
```

`make help` lists every target.

`make up` publishes Postgres on `127.0.0.1:5432` and MinIO on
`127.0.0.1:9000` (console `:9001`). Both bind to loopback only — see
`BIND_HOST` in `.env.example` before you change that.

## Layout

```
apps/web/                 Next.js dashboard                        (T8)
packages/qa-schema/       JSON Schema wire contracts + codegen      (T1)
packages/ui/              shared presentational components
packages/types/           shared non-wire TypeScript types
server/                   Go backend: REST, WebSocket, job queue    (T4)
  cmd/{server,migrate,seed}/
  migrations/             versioned PostgreSQL schema
  internal/db/            hand-written SQL + sqlc-generated query layer
  internal/{auth,org,project,runtime,run,testcase,report,artifact,realtime}/
  pkg/db/                 DSN handling, connection pool, migrator
daemon/                   Go QA daemon, runs on the operator's box  (T9)
  cmd/qa-daemon/          pair / start / status / doctor
  runtime/                control loop: WS client, run orchestration
  {workspace,artifacts,browser,proc}/
  agent/                  AI CLI detection; provider lands here     (T10)
  executor/               Node sidecar that owns Playwright         (T6)
e2e/                      platform end-to-end tests + fixture app   (T13)
docker/                   image definitions                         (T9/T12)
docs/adr/                 architecture decision records             (T3)
```

Most directories are stage-1 placeholders; the task that fills each one is
noted above and in the per-directory README.

## The QA daemon

`daemon/` runs on a machine inside the customer's network and is the only
component that touches the application under test. It dials **out** to the
backend over one WebSocket and accepts no inbound connections (ADR-002), which
is what lets it test `localhost` and `staging.internal` without exposing them.

```bash
cd daemon && go run ./cmd/qa-daemon pair --code K7Q2-9FMR-3XT8 --server http://localhost:8080
cd daemon && go run ./cmd/qa-daemon doctor      # what is missing, and how to fix it
make dev-daemon                                  # start it
```

`daemon/README.md` covers the config file, the per-run workspace layout,
retention, and what the daemon guarantees about reconnection, idempotency and
cancellation.

## Database

PostgreSQL is the source of truth, the job queue and the event log. There is no
Redis and no ORM.

- **Migrations: [goose](https://github.com/pressly/goose)**, one file per
  change under `server/migrations`, `up` and `down` in the same file, embedded
  into the `migrate` binary. Chosen over golang-migrate because keeping both
  directions in one file makes a missing `down` obvious in review, and because
  `embed.FS` support means deploying the schema is deploying one binary.
- **Queries: [sqlc](https://sqlc.dev)**. SQL is written by hand in
  `server/internal/db/queries` and compiled into type-safe Go in
  `server/internal/db/dbgen`. sqlc type-checks each query against the same
  migration files the migrator applies, so a query cannot drift from the
  deployed schema. The generated code is committed, so `go build` works without
  sqlc installed.

```bash
make up            # start postgres
make migrate-up    # apply the schema
make migrate-status
SEED_OWNER_PASSWORD=... make seed   # one org, one owner, one project

make gen-sqlc      # regenerate after editing migrations/ or queries/
make test-db       # the Go tests that need a real database
make migrate-down  # roll everything back, leaving an empty database
```

### Multi-tenancy

The platform is multi-tenant from the first migration. Retrofitting tenancy
means rewriting every query, so the rules below are enforced mechanically
rather than by convention:

- Every table holding customer data has `org_id uuid NOT NULL REFERENCES
  organizations (id)` and `UNIQUE (org_id, id)`.
- Cross-table references inside a tenant are **composite foreign keys**
  `(org_id, parent_id) -> parent (org_id, id)`. A row in one organization
  physically cannot point at a row in another; Postgres rejects it, not the
  service layer. See the header of `server/migrations/00001_extensions.sql` for
  why this was chosen over a validation trigger.
- Every query that touches a domain table binds `org_id` to a query parameter.
  `TestQueriesAreOrgScoped` in `server/internal/db` fails the build otherwise,
  and its own negative tests prove it still catches violations. A query that
  genuinely cannot be scoped (a platform maintenance sweeper) must carry an
  `-- org-scope-exempt: <reason>` annotation.
- Artifact storage keys are `orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/[{testCaseId}/]{name}`
  and a CHECK constraint refuses any key that does not start with the row's own
  org and run.

Postgres row-level security is deliberately **not** used in the MVP: the
query-level rule plus these tests is the gate, and RLS would need a per-request
`SET LOCAL` that a pooled connection makes easy to get subtly wrong. It stays
available as later defence in depth.

### Adding a migration

Forward-only. Never edit an applied file; add the next number. Follow
expand → migrate → contract: a column is added in one release and dropped in a
later one, and nothing is renamed in the same deploy as the code that uses it.
Anything that takes a long lock on a busy table gets its own migration with
`-- +goose NO TRANSACTION` and `CREATE INDEX CONCURRENTLY`.

## Contracts

Everything that crosses a process boundary is defined once, in
`packages/qa-schema`, and generated for both Go and TypeScript. Components are
written against the schema, not against each other.

## Security model

The product's whole job is to open web pages nobody vetted, and then hand what
it read to an AI CLI that can run commands on the operator's own machine. The
design assumes every page is trying to hijack the agent.

Full detail, including what the system does **not** guarantee, is in
[`docs/SECURITY.md`](docs/SECURITY.md); the attacks the controls answer are in
[`docs/threat-model.md`](docs/threat-model.md). In outline:

- **Page content is data, never instruction.** Anything read out of a page —
  text, `alt`, `title`, comments, response bodies, console output, file names —
  is framed with markers carrying a per-run nonce the page cannot observe, and
  both markers are stripped from the payload so a page cannot close the frame
  early. `daemon/security`, mirrored byte for byte in
  `daemon/executor/src/untrusted.ts`.
- **The plan is gated, not just the prompt.** A prompt boundary whose failure
  mode is "the model was persuaded" is not a control, so `security.PlanGate`
  validates what the model produced before any of it runs: off-allowlist
  navigation, a literal credential, an invented fixture, an unflagged raw
  locator, an unknown action.
- **18 injection cases across 9 channels run in CI** (`e2e/injection-corpus`)
  and assert two properties: page content never reaches a prompt's instruction
  region, and the plan each injection was fishing for is refused.
- **Deny by default outbound.** Exact-host allowlist, no implied subdomains,
  no `file:`/`javascript:`/`data:`, and private and link-local addresses behind
  an explicit opt-in.
- **Credentials never share a context with a page.** Target-application logins
  are referenced as fixtures (`fixture:logged_in_as_admin`), sealed at rest,
  and scrubbed — in every encoding they travel in — out of prompts, workspace
  files, logs, events and artifacts.
- **The AI CLI is confined.** Landlock restricts it to the run's workspace,
  `no_new_privs` closes the setuid escape, rlimits bound it, and it inherits an
  environment allowlist rather than the daemon's own.
- **Humans approve irreversible actions** — payments, deletes, permission
  changes. **Not built yet;** it is the first row of the Known gaps table in
  `docs/SECURITY.md`.

**This repository is public.** Never commit a `.env`, a key, or a real
credential. `.env.example` holds placeholders only, and CI fails the build on a
secret-scan hit.

To report a vulnerability, open a private security advisory — see
[`docs/SECURITY.md`](docs/SECURITY.md#reporting-a-vulnerability). Please do not
open a public issue.

## Contributing

`make lint` and `make test` must pass before you push; CI runs the same
targets, so a green local run predicts a green CI run. `make test-security`
runs the injection corpus and the boundary tests on their own.

CI (`.github/workflows/ci.yml`) has four gates: `lint`, `test`, `security`,
`test-db`.

> **Pending workflow change.** An aggregate `ci` job and the injection-corpus
> gate are parked at `.github/ci-workflow-pending/ci.yml` because the agent's
> credential lacks the GitHub `workflow` scope. See the README there — it is a
> `cp` and a push from a credential that has the scope, plus one branch
> protection setting.

Changes under `.github/`, `docker/`, `docker-compose.yml`, `.env.example`,
`packages/qa-schema/`, `daemon/security/` or `daemon/agent/prompts/` need a
review from the owners listed in `CODEOWNERS`.
