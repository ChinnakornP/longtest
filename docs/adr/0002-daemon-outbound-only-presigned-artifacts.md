# ADR-002: The daemon only dials out, and artifacts bypass the backend

- **Status:** Accepted — 2026-09-04
- **Affected:** `server/internal/realtime`, `server/internal/artifact` (LONG-10),
  `daemon/runtime`, `daemon/artifacts` (LONG-11), `docker-compose.yml`
- **Related:** [ADR-006](0006-multi-tenant-from-day-one.md)

## Context

The reason a local runtime exists at all is that the app under test is often
unreachable from the internet: `localhost:3000`, `192.168.1.20`,
`staging.internal`. The daemon therefore lives inside a corporate network,
behind NAT, frequently behind an egress proxy, and on a machine whose firewall
we do not control. Anything that requires the backend to reach *in* is
unshippable.

The evidence a single run produces is also large: a `trace.zip` is routinely
tens of megabytes, and a run has one per failing test case. Streaming that
through the API server would make artifact upload the dominant load on a
service whose real job is coordination.

## Decision

1. **One outbound WebSocket per runtime.** The daemon dials
   `WS /api/v1/daemon`, authenticates with its runtime token, and keeps a
   single connection open with heartbeats and reconnect/backoff. The daemon
   exposes **no inbound port**. All server→daemon traffic (`run.assign`,
   `run.cancel`) travels back down that connection using the envelope in
   contract D.
2. **Artifacts go straight to S3/MinIO.** The backend issues presigned PUT
   URLs scoped to a single run's key prefix; the daemon uploads directly and
   reports only object keys in `run.result`. The backend never proxies bytes.

Artifact keys are `orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/{testCaseId}/…`, and
a presigned URL is valid only for its own run prefix.

## Alternatives considered

- **gRPC bidirectional streaming.** Rejected: it solves the same NAT problem
  the WebSocket already solves, while adding proto tooling, a second transport
  in the gateway, and worse behaviour through corporate HTTP proxies.
- **Backend polls or connects to the daemon.** Rejected: cannot cross NAT, and
  would require customers to open inbound ports — the opposite of the pitch.
- **Artifacts uploaded through the API.** Rejected: puts tens of MB per run
  through the request path, and forces the API to hold connections for the
  duration of a slow upstream link.

## Consequences

- Run events are **at-least-once**. Every event carries `(runId, seq)` and the
  server deduplicates on `run_events(run_id, seq) UNIQUE`. Consumers must be
  idempotent; a reconnecting daemon replays from its last acked `seq`.
- The daemon must survive a backend restart without losing an in-flight run:
  reconnect, re-`hello`, resume streaming.
- Artifact retention, lifecycle rules, and orphan cleanup are storage-side
  concerns, not API concerns — an object can exist that no row references.
- MinIO is part of local development (`make up`), because the upload path
  cannot be stubbed away without losing the part most likely to break.
- Presigned URL TTL must outlive a slow upload but not a run; expiry is an
  `ENVIRONMENT_ERROR`, retried by requesting a fresh URL.
