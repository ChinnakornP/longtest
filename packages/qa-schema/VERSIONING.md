# Versioning the contracts

Five components — Planner, Executor, Analyst, Backend, Web — are built in
parallel against these documents and deployed on different schedules. The rules
below exist so that "which version am I talking to" is always answerable, and so
that a mismatch fails at the boundary instead of halfway through a run.

## The version lives in two places

- **`$id` carries the major**: `https://qa.local/schema/test-case/1`, addressed
  in code as `test-case@1`. A new major is a new file and a new id; both can be
  served at once while consumers migrate.
- **`x-contract-version` carries the full semver**: `1.2.0`. It is exposed as
  `CONTRACT_VERSIONS['test-case@1']` in TypeScript and
  `qaschema.ContractVersions["test-case@1"]` in Go, and the generator refuses a
  version whose major does not match the `$id`.

## What counts as what

| Change                                            | Bump  | Notes                                                             |
| ------------------------------------------------- | ----- | ----------------------------------------------------------------- |
| Adding an enum member                             | minor | Old consumers **must reject** the new member — see below           |
| Adding an optional property                       | minor | Every object is closed, so old consumers reject it: expand first   |
| Loosening a constraint (raising a max, say)       | minor |                                                                    |
| Fixing a description, `$comment`, `title`         | patch |                                                                    |
| Removing or renaming a property                   | major |                                                                    |
| Making an optional property required              | major |                                                                    |
| Removing an enum member                           | major |                                                                    |
| Tightening a pattern or a bound                   | major | It can reject documents that were valid yesterday                  |

## Adding an enum member

The `action` and `assertion.type` vocabularies in `test-case@1`, the element
types in `application-map@1` and the `failureClass` values in `finding@1` are
frozen at v1. To add one:

1. Add the member to the schema and bump the minor.
2. Regenerate: `make gen`. Every consumer picks the member up as a typed value.
3. Ship the **consumers first**, producers second.

A consumer that meets a member it does not know **must fail loudly**. It must
never skip the step, bucket the value as `UNKNOWN`, or fall through to a
default. An action an executor silently skipped is a test that passed without
testing anything, which is worse than a red build.

The generated code makes that the easy path:

```go
if !step.Action.IsValid() {
    return fmt.Errorf("step %d: unsupported action %q (this build knows %v)",
        i, step.Action, qaschema.StepActionValues)
}
```

```ts
if (!STEP_ACTION_VALUES.includes(step.action)) {
  throw new Error(`unsupported action ${step.action}`);
}
```

Validation already enforces this for anything that arrives as JSON: an unknown
member is an `enum` error naming the field, not a pass.

## Adding a property

Every object in these schemas is closed (`additionalProperties: false`), so a
producer that emits a new field breaks a consumer validating against the older
minor. Roll it out expand → migrate → contract, the same way as a database
column:

1. **Expand** — add the optional property, bump the minor, deploy the schema
   package to every consumer.
2. **Migrate** — start producing the field once every consumer is on that minor.
3. **Contract** — only in a later major may it become required, or may the old
   field it replaced be removed.

This is deliberate. The alternative — open objects — means a typo in a field
name is accepted in silence, and that is exactly the failure mode this contract
package exists to prevent.

## Introducing a new major

1. Copy the file to `<name>.schema.json` with `$id` ending `/2` and
   `x-contract-version` `2.0.0`. Both majors are separate ids and can coexist.
2. `make gen` — both are generated, and both stay validatable.
3. Migrate producers and consumers; the run/execution rows already record which
   contract version produced them.
4. Delete the old file only once nothing in the database still references it.

## Compatibility checklist for a schema change

- [ ] `make gen` run, and re-running it leaves the working tree clean
- [ ] a fixture added under `valid/` for the new shape, and one under `invalid/`
      for the mistake the change is meant to catch
- [ ] `pnpm --filter @qa/schema run gen:expectations` run **only if** the change
      to the error output was intended; the diff read line by line
- [ ] `make test-go` and `make test-js` pass — including the cross-language
      fixture test, which is what proves Go and TypeScript still agree
- [ ] `x-contract-version` bumped according to the table above
