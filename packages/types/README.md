# @qa/types

Hand-written TypeScript types that are shared by the web app and the executor
sidecar but are **not** part of a versioned wire contract.

Anything that crosses a process boundary — test cases, the Application Map,
findings, the daemon envelope — belongs in `@qa/schema` instead, where it is
defined as JSON Schema and generated for both Go and TypeScript.
