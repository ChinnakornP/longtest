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

## What it is used for

- `daemon/executor/test/integration.test.ts` — three back-to-back runs of
  the same test case must produce the same outcome (deterministic).
- `daemon/executor/test/resilience.test.ts` — crash / redirect-loop /
  timeout cases must end as `error`, not hang.
- The benchmark in `e2e/eval` (T17) — golden dataset of test cases that
  exercise every enum v1 action and assertion.

## Why not Express / Fastify

The fixture should be transparent. `node:http` is small enough that a
reader can see every status code, every cookie, and every form field
without scrolling past framework boilerplate.
