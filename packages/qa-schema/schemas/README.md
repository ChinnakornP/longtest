# schemas

One `*.schema.json` per contract, JSON Schema draft 2020-12. The file name must
match the `$id` name, and the `$id` ends in the major version:

```
test-case.schema.json  →  $id: https://qa.local/schema/test-case/1  →  test-case@1
```

These files are the source of truth. Everything else — the Go structs, the
TypeScript types, `SCHEMA_IDS`, the copies embedded in each Go module — is
generated from them by `make gen`.

House rules, all of them enforced by the generator or the validator rather than
by review:

- Every object is closed: `additionalProperties: false`.
- Inline object shapes are lifted into `$defs` so they get a generated type.
- Only the keyword subset in [../README.md](../README.md) is allowed; anything
  else fails to load in both languages rather than being ignored in one.
- `pattern` and `format: regex` must compile in both JavaScript and Go's RE2, so
  no lookaround and no backreferences.
- A discriminated union carries `x-codegen.union`; a def that exists only to
  constrain, not to describe a shape, carries `x-codegen: "skip"`.

Changing anything here: [../VERSIONING.md](../VERSIONING.md).
