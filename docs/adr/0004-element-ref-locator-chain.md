# ADR-004: Test steps address elements by `ref`, not model-authored CSS

- **Status:** Accepted — 2026-09-04
- **Affected:** `packages/qa-schema` (LONG-4), `daemon/executor` (LONG-8),
  `daemon/discovery` (LONG-15), LONG-16 (planner), `server/internal/testcase`
- **Related:** [ADR-001](0001-executor-node-sidecar.md),
  [ADR-003](0003-ai-cli-file-contract.md)

## Context

This is the decision that determines whether the product is deterministic.

If the planner emits raw selectors (`div.card > button:nth-child(3)`), the
model is guessing at a DOM it saw once, in prose. Those selectors break on the
next CSS refactor, and — worse — they break *silently into a failure*, so the
report says "Create employee failed" when the truth is "the test was written
against a page that no longer looks like that". A QA product that cannot
distinguish those two cases has no value.

Discovery already produces an Application Map in which every interactive
element has a stable id and an ordered list of real locators observed in the
page.

## Decision

A test step targets an element as `{ "ref": "emp.btn.add" }` — an id owned by
the Application Map, not by the model.

The executor resolves a `ref` through an ordered **locator fallback chain**,
taking the first that matches exactly one element:

1. `testId` — `data-testid` and friends
2. `role` + accessible name
3. `label` / associated form label
4. `text`
5. `css` — last resort, only if Discovery recorded one

Resolution outcomes are explicit: no match → step fails as `TEST_BUG` with the
chain that was tried; multiple matches → fails as ambiguous, never "first
one wins". If a test case does arrive with a raw locator instead of a `ref`,
the executor still runs it but marks the step `unstable: true`, and the run
report surfaces the count.

Elements carry `lastSeenRunId` so a later Discovery run can expire entries the
app no longer has, instead of letting the map accumulate ghosts.

## Alternatives considered

- **Model-authored CSS/XPath per step.** Rejected: non-deterministic across
  runs, unfixable without re-prompting, and it conflates product bugs with
  selector rot.
- **Single canonical locator per element, no fallback.** Rejected: real apps
  have no `data-testid` on most elements; a single strategy means Discovery
  can only see the instrumented parts of the app.
- **Resolve refs in the daemon (Go) and pass selectors to the executor.**
  Rejected: only the executor holds live page state, and resolution needs to
  retry against the current DOM, not a stale snapshot.

## Consequences

- The Application Map is a hard dependency of execution: no map, no runnable
  test. `mode: execute` requires an existing map for the project.
- `elements(page_id, ref) UNIQUE` in the schema; refs are stable identifiers
  and renaming one is a migration of the test cases that reference it.
- The planner's prompt gets the map as an input file and is instructed to use
  refs only — a step referencing an unknown ref fails schema/semantic
  validation before it ever reaches the browser.
- Approved regression test cases survive CSS refactors of the app under test.
  This is the mechanism that makes the "approve once, reuse forever" loop real.
- Discovery quality becomes the ceiling on test quality: an element missing
  from the map is an element no test can touch. LONG-19's golden-map benchmark
  exists to measure that.
