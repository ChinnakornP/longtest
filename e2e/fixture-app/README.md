# Fixture app

The deterministic target for qa-executor integration tests.

It is a deliberately tiny CRUD app: sign in, see a list, create / edit /
delete employees. State lives in memory; every restart is a clean slate.

## Run it

```
pnpm --filter @qa/fixture-app start
```

The server prints `FIXTURE_PORT=<n>` on stdout and keeps running. Set
`FIXTURE_USER` / `FIXTURE_PASSWORD` to override the default credentials
(`admin@example.test` / `letmein`).

## Injected bugs

`FIXTURE_BUGS` turns on deliberate defects, comma-separated. They exist so the
Failure Analyst can be measured: a classifier is only checkable against
failures whose true cause somebody already knows.

```
FIXTURE_BUGS=create-500,edit-not-synced pnpm --filter @qa/fixture-app start
```

| Name | What breaks | Why it is there |
| --- | --- | --- |
| `create-500` | `POST /employees` answers 500 and stores nothing. Validation still runs first. | The loud case. A 5xx sits in the network log, so the analyst does not have to reason about the page to find it. |
| `edit-not-synced` | The update saves and the detail page shows it; the list keeps rendering the value from creation. | The hard case. Every request is 200 and the page the test lands on is correct, so the only evidence is one assertion on the list disagreeing with what was just saved. A classifier that reads status codes and nothing else calls this a TEST_BUG. |

Both are `PRODUCT_BUG`. With no `FIXTURE_BUGS` set the app is honest, which is
what every other suite here depends on.
`daemon/executor/test/fixture-bugs.test.ts` holds them to the descriptions
above — an analyst benchmark built on a defect that quietly stopped happening
goes green for the wrong reason.

## What it is used for

- `daemon/executor/test/integration.test.ts` — three back-to-back runs of
  the same test case must produce the same outcome (deterministic).
- `daemon/executor/test/resilience.test.ts` — crash / redirect-loop /
  timeout cases must end as `error`, not hang.
- The benchmark in `e2e/eval` (T17) — golden dataset of test cases that
  exercise every enum v1 action and assertion.
- The Failure Analyst (T15) — run it with `FIXTURE_BUGS` and every failed
  execution should come back classified `PRODUCT_BUG`.

## Why not Express / Fastify

The fixture should be transparent. `node:http` is small enough that a
reader can see every status code, every cookie, and every form field
without scrolling past framework boilerplate.
