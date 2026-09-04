// Package project owns projects and the Application Map that accumulates
// against them.
//
// The map is assembled here rather than in internal/run because it is a
// property of the project, not of the run that last observed it: discovery
// stamps last_seen_run_id and never deletes, so the map a plan run is handed
// is the same document GET /projects/{id}/appmap returns.
package project
