# Architecture Decision Records

An ADR records a decision that is not self-evident from the code: something a
future reader would otherwise reopen, and something several components have to
agree on. One file per decision, named `NNNN-<slug>.md`.

## Index

| ADR                                                | Decision                                                                    | Status   |
| -------------------------------------------------- | --------------------------------------------------------------------------- | -------- |
| [001](0001-executor-node-sidecar.md)               | The executor is a Node sidecar over stdio JSON-RPC, not `playwright-go`      | Accepted |
| [002](0002-daemon-outbound-only-presigned-artifacts.md) | The daemon only dials out; artifacts go straight to S3 via presigned URLs | Accepted |
| [003](0003-ai-cli-file-contract.md)                | AI CLIs exchange files (`prompt.md` → `out.json`), never parsed stdout       | Accepted |
| [004](0004-element-ref-locator-chain.md)           | Test steps address elements by `ref`, not model-authored CSS                 | Accepted |
| [005](0005-postgres-job-queue.md)                  | The job queue is Postgres `FOR UPDATE SKIP LOCKED`; no Redis in the MVP      | Accepted |
| [006](0006-multi-tenant-from-day-one.md)           | Multi-tenant from day one: org/user/membership, `X-Org-ID`, org-scoped queries | Accepted |
| [007](0007-org-id-in-path-is-an-assertion.md)      | An org id in a path is an assertion, never a source (refines ADR-006)        | Accepted |
| [008](0008-web-ships-no-backend.md)                | `apps/web` ships no backend: no `/api/**` route handler survives a production build | Accepted |
| [009](0009-artifact-upload-is-a-minting-endpoint.md) | Artifact upload is a per-object minting endpoint; S3 cannot presign a prefix (refines ADR-002) | Accepted |
| [010](0010-ingest-refuses-an-unresolvable-evidence-graph.md) | Ingest refuses a result frame whose artifact handles collide or whose findings cite nothing | Accepted |

## When to write one

Write an ADR when a change:

- crosses a component boundary — backend ↔ daemon ↔ executor ↔ web;
- changes or freezes a contract in `packages/qa-schema`, the REST/WS surface,
  or the daemon envelope;
- adds a runtime dependency or an infrastructure service (a broker, a cache, a
  storage backend, a second database);
- introduces a second way to do something the codebase already does;
- picks a security or tenancy model, or relaxes one;
- is expensive to reverse later.

Do not write one for a decision the code already states plainly: a library
choice with no alternative worth naming, a refactor, a rename.

## Format

Keep it to one page. Sections, in order:

- **Status** — `Proposed` / `Accepted` / `Superseded by ADR-NNN` / `Deprecated`, with a date
- **Affected** — the components and issues that inherit the decision
- **Context** — the forces, stated so the decision looks inevitable in hindsight
- **Decision** — what we do, in the present tense
- **Alternatives considered** — at least one real option, with the reason it lost
- **Consequences** — what this costs, what it constrains, what now has to be true

An ADR describes a decision, not a design. Interface details belong with the
interface: schemas in `packages/qa-schema`, API shape in its own reference.

## Changing a decision

An ADR is immutable once accepted. History is not edited and files are not
deleted.

To change a decision:

1. Write a new ADR with the next number. Its **Context** states what changed
   since the original — new constraint, new information, a cost that turned
   out different.
2. Add `**Supersedes:** ADR-NNN` to the new file.
3. Set the old file's status to `Superseded by ADR-MMM (date)` — a one-line
   edit to the status line and nothing else — and update this index.

Corrections that do not change the decision (a typo, a broken link, a
clarifying sentence) may be edited in place.

**A pull request that contradicts an accepted ADR is not mergeable on its own.**
It either changes to conform, or it arrives together with the superseding ADR.
Reviewers cite the ADR by number; "this conflicts with ADR-004" is a blocking
review comment, not a preference. If a decision turns out wrong mid-stage, that
is a normal outcome — supersede it, and say so on the issue so the other
parallel tasks pick it up.
