# fixtures

Two buckets per contract:

- `valid/` — documents that must validate. These double as the worked examples
  of each contract; the ones under `test-case/` and `application-map/` are the
  payloads from the architecture thread.
- `invalid/` — documents that must be rejected, each one aimed at a specific
  mistake a component could plausibly make: an action the vocabulary does not
  have, a raw CSS selector that did not declare itself unstable, a precondition
  carrying a password, an artifact key outside its org prefix.

`expectations.json` records the exact error list every fixture must produce —
instance path, keyword and message. The TypeScript suite asserts against it and
so does the Go suite, which is what makes "the two validators agree" a test.

It is regenerated deliberately, never as part of `make gen`:

```
pnpm --filter @qa/schema run gen:expectations
```

Read the diff before committing it. A change there is a change in what the
platform accepts.
