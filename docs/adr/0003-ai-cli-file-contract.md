# ADR-003: AI CLIs exchange files, never parsed stdout

- **Status:** Accepted — 2026-09-04
- **Affected:** `daemon/agent/*` (LONG-12), `daemon/workspace` (LONG-11),
  `packages/qa-schema` (LONG-4), LONG-14 (secret handling), LONG-15..17
- **Related:** [ADR-001](0001-executor-node-sidecar.md)

## Context

The product deliberately does not call an LLM API. It invokes whichever coding
CLI the user already installed and authenticated — `claude`, `opencode`,
`agy` — through an `AgentProvider` abstraction. Those CLIs have different,
unversioned, human-oriented stdout: banners, spinners, streamed prose, ANSI.
Parsing three of them, and re-parsing after every upstream release, is a debt
we would pay forever.

Separately, the AI's output feeds structured consumers: an Application Map, a
test plan, a finding. "Mostly-JSON prose" is not a usable interface for a
component whose next step is a database write.

## Decision

Every agent invocation is a **file exchange inside a per-run workspace
directory** (`/workspaces/{project}/{run}/{phase}/`):

- The provider writes `prompt.md` plus input files (`application-map.json`,
  `execution.json`, …). Inputs are referenced by filename from the prompt;
  large context is **never** inlined into the prompt string.
- The prompt instructs the CLI to write `out.json` and nothing else that
  matters.
- The provider validates `out.json` against a named schema version
  (`application-map@1`, `test-plan@1`, `finding@1`).
- Invalid output is retried at most **twice**, each retry appending the
  concrete validation errors to the prompt. After that the task fails with
  `agent_output_invalid`.

stdout is captured to the run log for debugging and streamed as progress
events. It is never the source of truth.

## Alternatives considered

- **Parse stdout / stream JSON from the CLI.** Rejected: three incompatible
  formats, no stability guarantee, and no natural place to put large inputs.
- **Call provider HTTP APIs directly.** Rejected: it breaks the core product
  premise (use the CLI the customer already pays for and authenticated) and
  puts us in the key-management business.
- **Trust the output without schema validation.** Rejected: a malformed test
  plan would surface as a mystery executor crash three stages downstream.

## Consequences

- Every AI-produced artifact needs a versioned JSON Schema in
  `packages/qa-schema` *before* the producing feature is built.
- The workspace is the agent's blast radius. Agents get no filesystem access
  beyond their run's workspace, which is also the enforcement point for
  prompt-injection hardening (LONG-14): DOM text copied into the workspace is
  data, wrapped and marked untrusted, never instructions.
- Target-app credentials never enter the workspace or a prompt; test cases
  reference them indirectly (`fixture:logged_in_as_admin`) and the executor
  resolves them.
- Workspaces are per-run and disposable, but must be retainable on failure for
  debugging — they are the only reproduction of what the model actually saw.
- Adding a provider means implementing `Detect`/`Run` and a launch command; no
  new parsing code.
