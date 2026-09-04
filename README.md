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
  cmd/{server,migrate}/
  internal/{auth,org,project,runtime,run,testcase,report,artifact,realtime}/
  pkg/db/
daemon/                   Go QA daemon, runs on the operator's box  (T5)
  {browser,runtime,agent,discovery,workspace,artifacts}/
  executor/               Node sidecar that owns Playwright         (T6)
e2e/                      platform end-to-end tests + fixture app   (T13)
docker/                   image definitions                         (T9/T12)
docs/adr/                 architecture decision records             (T3)
```

Most directories are stage-1 placeholders; the task that fills each one is
noted above and in the per-directory README.

## Contracts

Everything that crosses a process boundary is defined once, in
`packages/qa-schema`, and generated for both Go and TypeScript. Components are
written against the schema, not against each other.

## Security model

The product's whole job is to open web pages nobody vetted. The design assumes
every page is trying to hijack the agent.

- **Page content is data, never instruction.** Anything read out of a page —
  text, `alt`, `title`, comments, response bodies, file names — is fenced with
  explicit untrusted markers before an AI CLI sees it, and the markers are
  stripped from the payload so a page cannot close the fence early.
- **Deny by default outbound.** The browser reaches allow-listed origins
  through a proxy. URLs discovered on a page are never fetched automatically.
- **Humans approve irreversible actions:** payments, form submits, credential
  entry, downloading and running files, permission changes.
- **Credentials never share a context with a page.** Target-application logins
  are referenced as fixtures (`fixture:logged_in_as_admin`) and injected at
  request time; the real values never reach a model or a workspace.
- **The browser is sandboxed:** its own container, non-root, read-only rootfs,
  dropped capabilities, no host network, no docker socket, ephemeral profile
  per session, and CPU/RAM/PID/time limits.
- **Everything is attributable.** Navigations and tool calls are logged with
  their provenance — operator-initiated or page-initiated — and secrets are
  redacted before anything is written.

Enforcement is delivered by T9; `daemon/executor/src/untrusted.ts` and
`server/pkg/db.RedactDSN` are the first two pieces in the tree.

**This repository is public.** Never commit a `.env`, a key, or a real
credential. `.env.example` holds placeholders only, and CI fails the build on a
secret-scan hit.

## Contributing

`make lint` and `make test` must pass before you push; CI runs the same
targets. Changes under `.github/`, `docker/`, `docker-compose.yml`,
`.env.example` or `packages/qa-schema/` need a review from the owners listed in
`CODEOWNERS`.
