# @qa/schema

The versioned wire contracts every component agrees on. This package is the
single source of truth for:

| Schema               | Contract | Between                        |
| -------------------- | -------- | ------------------------------ |
| `test-case@1`        | A        | Test Planner → Executor        |
| `application-map@1`  | B        | Discovery → Planner → Backend  |
| `finding@1`          | F        | Failure Analyst → Report       |
| `daemon-envelope@1`  | D        | Backend ↔ Daemon control plane |

## Rules

- Schemas are the source; Go and TypeScript types are **generated**
  (`make gen`) and never hand-edited.
- The `action` and `assertion.type` enums are frozen at v1. Adding a member is
  a new minor version, and the executor must reject an action it does not
  recognise with an explicit error — never skip it silently.
- Everything that arrives from the application under test, or from an AI CLI,
  is validated against a schema before any other component touches it. A page
  can put arbitrary text into a label; a schema is what stops that text from
  being treated as structure.

Stage-1 placeholder: the schemas, the generator and the validator CLI are
delivered by T1.
