// Package qaschema is the Go binding for the wire contracts in
// packages/qa-schema: the JSON Schema documents, the generated types and a
// validator that agrees with the TypeScript one field for field.
//
// Anything that crosses a component boundary — a test plan an AI CLI wrote, a
// frame off the daemon socket, an execution result from the Node executor — is
// validated here before any other package looks at it. Nothing in this repo may
// re-declare one of these shapes: the types in types.gen.go are generated from
// the same files this package embeds, so a struct that drifts from the contract
// cannot compile.
//
// Layout:
//
//	validator.go   draft 2020-12 over the keyword subset the contracts use
//	registry.go    embedded documents, $ref resolution, the Validate entry points
//	types.gen.go   generated Go types (make gen)
//	ids.gen.go     generated schema ids and contract versions (make gen)
//
// The same package is mirrored into the daemon module by
// packages/qa-schema/scripts/generate.mjs. Edit the copy under server/ and run
// `make gen`; the daemon copy carries a header saying so.
package qaschema
