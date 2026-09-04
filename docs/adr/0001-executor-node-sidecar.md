# ADR-001: The executor is a Node sidecar over stdio JSON-RPC

- **Status:** Accepted — 2026-09-04
- **Affected:** `daemon/executor` (LONG-8), `daemon/runtime` (LONG-11), `e2e/`
- **Related:** [ADR-004](0004-element-ref-locator-chain.md)

## Context

The daemon is Go, but every deterministic browser action in the product runs
through Playwright: navigate, click, fill, assert, screenshot, trace, video,
network and console capture. Discovery additionally has to evaluate JavaScript
inside the page to extract the element inventory that becomes the Application
Map. Whichever language drives Playwright owns the reliability of the whole
product, because the executor is the only component the user's verdict
(`pass` / `fail`) actually depends on.

`playwright-go` is a community port that lags upstream, most visibly on the
features we depend on for evidence: tracing, video, and request interception.
Waiting on that lag would put the evidence pipeline — the input to every AI
failure analysis — on the critical path of someone else's release schedule.

## Decision

The daemon spawns `qa-executor`, a Node process from `daemon/executor`, as a
child of the run and talks to it over **line-delimited JSON-RPC on stdio**.
One executor process per run; the daemon owns its lifecycle and kills the
process tree on cancel or run end.

The executor is the only component allowed to import Playwright. The Go side
never models browser state — it sends a test case and receives step results,
assertion results, and artifact paths.

## Alternatives considered

- **`playwright-go` inside the daemon (single binary).** Rejected: feature lag
  on trace/video/interception, and the DOM extraction Discovery needs is
  JavaScript-in-page regardless, so the Node dependency does not disappear —
  it only moves somewhere less visible.
- **Executor as a local HTTP service.** Rejected: introduces port allocation,
  health checks, and orphan cleanup on a machine we do not administer. stdio
  gives us process-tree lifecycle and cancellation for free.
- **Executor as a separate remote service.** Rejected: it breaks the whole
  premise of the local runtime — the browser must sit inside the customer's
  network, next to the app under test.

## Consequences

- Customer machines need Node and Playwright browsers. `qa-agent setup` and
  the daemon's capability detection must report Node/Chromium presence in
  `hello.browsers`, and a missing runtime is a setup error, never a test
  failure.
- The JSON-RPC framing is a contract between Go and Node: one JSON object per
  line, requests carry an id, and unknown methods are rejected explicitly.
- Cancellation must kill the process *tree* — a leaked Chromium outlives the
  executor otherwise. LONG-11's acceptance criteria cover this.
- Crash of the executor is classified `ENVIRONMENT_ERROR`, not `PRODUCT_BUG`;
  the daemon must be able to tell the two apart from exit code and last event.
- Two languages in the runtime: Go tooling and Node tooling both run in CI.
