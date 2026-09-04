# Architecture Decision Records

One file per decision: `ADR-<NNN>-<slug>.md`, each under a page, with
**Context / Decision / Alternatives / Consequences**.

Stage-1 backlog (owned by T3), recording decisions already taken:

| ADR | Decision                                                                 |
| --- | ------------------------------------------------------------------------ |
| 001 | The executor is a Node sidecar over stdio JSON-RPC, not `playwright-go`   |
| 002 | The daemon only dials out; artifacts go straight to S3 via presigned URLs |
| 003 | AI CLIs exchange files (`prompt.md` → `out.json`), never parsed stdout    |
| 004 | Test steps address elements by `ref`, not model-authored CSS              |
| 005 | The job queue is Postgres `FOR UPDATE SKIP LOCKED`; no Redis in the MVP   |

An ADR is amended by a new ADR that supersedes it, never by editing history.
