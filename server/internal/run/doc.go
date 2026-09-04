// Package run owns everything about a run: creating one, queueing it, handing
// it to a daemon, ingesting what comes back, and streaming the result.
//
// Per ADR-005 this package is the ONLY one allowed to read runs.status to make
// a scheduling decision. Keeping every queue access behind this boundary is
// what makes replacing Postgres with a real broker later a contained change
// rather than a repository-wide one.
//
// The pieces:
//
//	service.go       what an HTTP handler calls: create, get, cancel, events
//	controlplane.go  what a daemon's WebSocket calls: hello, heartbeat, events
//	ingest.go        run.result -> executions, artifacts, findings, app map
//	scheduler.go     the claim loop, and the lease sweeper next to it
//
// Every write that touches more than one row goes through db.Store.WithTx, and
// every read is org-scoped by a bound parameter rather than by a filter this
// package remembers to apply.
package run
