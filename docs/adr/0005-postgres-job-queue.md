# ADR-005: The job queue is Postgres `FOR UPDATE SKIP LOCKED`

- **Status:** Accepted — 2026-09-04
- **Affected:** `server/internal/run` (LONG-10), `server/pkg/db`,
  migrations (LONG-5), `docker-compose.yml`
- **Related:** [ADR-002](0002-daemon-outbound-only-presigned-artifacts.md)

## Context

The backend has to hand runs to runtimes: enqueue a run, pick it up exactly
once, assign it to an online daemon, retry if the daemon dies, and let the UI
see the state. That is a queue.

The volume is small and bounded by reality: a test run takes minutes, a
runtime executes one at a time, and a customer has a handful of runtimes. The
queue is not the bottleneck; a run is.

Postgres is already the source of truth, already in `docker-compose.yml`,
already backed up, and already the thing every consistency question is
answered against.

## Decision

Runs are queued in Postgres. Workers claim with

```sql
SELECT … FROM runs
 WHERE status = 'queued' AND (runtime_id IS NULL OR runtime_id = $1)
 ORDER BY created_at
 FOR UPDATE SKIP LOCKED
 LIMIT 1;
```

inside the transaction that flips the row to `assigned`. No Redis, no NATS, no
external broker in the MVP. Claim state, retry count, and lease expiry are
columns on the run, so the queue is visible in the same query as the run.

Wake-ups are event-driven (`LISTEN`/`NOTIFY` or an in-process signal on
enqueue) with a slow polling fallback, so an idle system is not spinning.

## Alternatives considered

- **Redis / a dedicated broker.** Rejected: a second stateful service in every
  deployment and every developer's compose file, plus a consistency seam
  between "queued in Redis" and "queued in Postgres" that we would then have
  to reconcile. Not paid for by MVP throughput.
- **In-memory queue in the API process.** Rejected: loses every queued run on
  restart and cannot be scaled to two API instances.
- **Cron-style polling only.** Rejected: assignment latency is a product
  characteristic — the UI should show a run starting within a second of the
  click.

## Consequences

- Assignment latency target: an online runtime receives `run.assign` within
  one second of enqueue (LONG-10 acceptance).
- Crashed daemons are handled by **lease expiry**: an assigned run whose
  runtime stops heartbeating past its lease is returned to `queued` with an
  incremented attempt count, and gives up as `ENVIRONMENT_ERROR` after the
  limit. Requeue must be idempotent against a daemon that comes back.
- Long transactions and connection-pool sizing matter: the claim transaction
  must stay short and must never span a network call to a daemon.
- Migrating to a real broker later is a contained change if all queue access
  stays behind the `run` service — no other package may read `runs.status` to
  make scheduling decisions.
