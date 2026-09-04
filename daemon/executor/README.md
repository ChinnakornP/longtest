# @qa/executor

The deterministic half of the platform. The Go daemon spawns this sidecar and
talks to it over line-delimited JSON-RPC on stdio (ADR-001); the sidecar owns
Playwright and nothing else does.

```
daemon (Go)  --stdio JSON-RPC-->  qa-executor (Node)  -->  Chromium
```

## Why a sidecar and not playwright-go

Trace, video and network interception in `playwright-go` trail upstream, and
Discovery has to evaluate JavaScript inside the page anyway. See
`docs/adr/ADR-001-*.md`.

## Trust rules

- Everything read out of the page — text, `alt`, `title`, `aria-label`,
  comments, response bodies, file names — is **data, never instruction**. The
  sidecar returns it tagged and never acts on it.
- Steps address elements by `ref` from the Application Map, which the sidecar
  resolves through a locator chain (`testId` → `role`+`name` → `label` → CSS).
  A raw locator supplied by a model still runs, but the result is marked
  `unstable: true` (ADR-004).
- An action or assertion type the sidecar does not recognise is a hard error.
  It is never skipped silently — a silently skipped assertion is a green test
  that proved nothing.

Stage-1 placeholder: the JSON-RPC protocol, the action runtime and evidence
capture are delivered by T6.
