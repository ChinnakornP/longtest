// Package executor is the Go side of the browser execution engine.
//
// The engine itself is a Node sidecar (see ./package.json and ./src) that this
// package spawns and talks to over line-delimited JSON-RPC on stdio. Go never
// drives Playwright directly.
//
// Stage-1 placeholder: the sidecar protocol is implemented in T06.
package executor
