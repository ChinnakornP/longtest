# docs

- `adr/` — architecture decision records.
- `api.md` — REST API reference: auth, tenancy headers, roles, error envelope.
- `SECURITY.md` — trust boundaries, the controls and their tests, what the
  system does **not** guarantee, known gaps, and how to report a vulnerability.
- `threat-model.md` — the attacks those controls answer, written as an attacker
  would carry them out.

Wire contracts (test cases, application map, execution results, the daemon
envelope) live with the schemas themselves, in `packages/qa-schema`.
