# ADR-010: Ingest refuses a result frame whose evidence graph does not resolve

- **Status:** Accepted — 2026-09-05
- **Affected:** `server/internal/run` (LONG-27), `daemon/runtime` (LONG-17),
  `packages/qa-schema` (`execution-result@1`, `finding@1`)
- **Related:** [ADR-009](0009-artifact-upload-is-a-minting-endpoint.md)

## Context

An artifact id in `run.result` is a run-local handle the daemon makes up.
Ingest builds one run-wide `handle -> artifacts.id` map from the frame and
resolves every `finding.evidence[]` citation through it. Two invariants hold
that together, and JSON Schema can express neither, which is why
`execution-result@1` writes them into a `$comment` instead: a handle is unique
within one run, and a duplicate "must be rejected, never last-write-wins".

Nothing enforced either one, and both failed silently.

The executor mints ids from a counter that restarts at zero for every test
case, so a run of forty cases produced forty artifacts called `screenshot-0`.
The map is run-wide, so the last one ingested won, and a finding about case 3
linked case 40's screenshot. That link opens, renders, and is a picture of a
different test — the failure mode where being wrong is indistinguishable from
being right.

Separately, ingest linked only the citations it could resolve and stored the
finding either way. A finding whose handles all failed to resolve was persisted
with no evidence at all, which contradicts `finding@1`'s own `minItems: 1` and
the reason it is there: a verdict with nothing behind it is a guess.

The daemon has a gate (`daemon/analysis/review.go`) that catches a model citing
an artifact it was never shown. It is worth having and it is not this. It
protects a daemon running that code, and a daemon is a customer-side process
holding a pairing token — "the producer already checked" is not a reason to
store what it sends.

Fixing this needs both halves to move, and moving them together would break
every daemon in the field on the day of the deploy.

## Decision

Expand → migrate → contract, with the producer first.

1. **Expand (LONG-17, `0957a66`).** `daemon/runtime/artifactids.go` namespaces
   every executor-minted handle to `e{n}-...`, where `n` is the execution's
   position in the run, and repoints every reference inside the same document.
   The backend still accepts everything.
2. **Contract (LONG-27, this ADR).** Ingest refuses a `run.result` whose
   evidence graph does not resolve, and the refusal is whole-frame:
   - one handle naming two different object keys → `duplicate_artifact_handle`;
   - a finding citing a handle the frame does not carry →
     `unknown_evidence_handle`;
   - a finding left with no evidence → `finding_without_evidence`.

   The check runs before the first `INSERT`, reports every problem in the frame
   at once, and returns an error so the ingest transaction rolls back. The run
   is then closed as `error` / `AGENT_OUTPUT_INVALID` with one `result_rejected`
   event carrying the rejections as `testcase.Rejection` — the same rule/detail
   shape a rejected plan uses, so one alert matches
   `data.rejections[].rule` whichever half of a frame was refused.

**A handle listed twice for the same object key is not a duplicate.** A real
`run.result` carries an execution's evidence in two places — the run-level
`artifacts[]` the daemon builds from its uploads, and the execution's own list
— and both entries describe one artifact, upserted onto one row by
`storage_key`. Only a handle naming two *different* objects is the violation.

**A daemon older than `0957a66` now fails its runs.** That is the point of the
ordering, not an accident of it: the alternative is that it keeps mislinking
evidence and nobody finds out.

## Alternatives considered

- **Keep resolving duplicates, pick the first writer instead of the last.**
  Deterministic, and still wrong — half the findings would cite the wrong test's
  screenshot, with no signal that anything happened. The contract says reject
  for this reason.
- **Reject the offending finding and store the rest.** This is what the code did
  for unresolvable citations, by dropping them. A report missing the evidence
  for one verdict reads exactly like a report whose verdict never had any, and
  T14 already settled the same question for plans: all-or-nothing, because a
  partially applied document is one nobody downstream can read the gaps out of.
- **Enforce it only in the daemon.** Already done, and it only binds daemons
  running that code. Ingest is the line that does not trust the producer.
- **Quarantine and retry a refused frame.** Worth having eventually; it needs a
  place to put the frame and a policy for replaying it, and neither exists yet.
  Failing the run is the honest interim: the run did not produce a readable
  result.

## Consequences

- A daemon that has not taken `0957a66` gets `error` /`AGENT_OUTPUT_INVALID`
  runs instead of quietly mislinked evidence. Its operator sees the rule and
  the two colliding object keys on the run's stream.
- A refused frame writes nothing. The run's terminal status is written by a
  second, minimal transaction, because the first one was rolled back and that
  rollback is the guarantee.
- `finding@1`'s `minItems: 1` now holds at the persistence layer as well as in
  the envelope validator, so loosening the schema or adding a second caller
  cannot put an unsupported verdict in the database.
- The rule strings are part of the operational surface. Renaming one breaks an
  alert, the same way renaming a `testcase` rule breaks the planner's retry
  prompt.
- Retry and quarantine of a refused frame are still unbuilt. Until they are,
  the cost of a bad producer is one failed run per delivery.
