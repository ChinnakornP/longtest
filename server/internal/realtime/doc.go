// Package realtime is the WebSocket half of the backend: one fan-out hub for
// browsers watching a run, one registry of the daemon control-plane
// connections, and the two handlers that drive them.
//
//	WS /api/v1/ws?runId=…   browser, read-only, session cookie + X-Org-ID
//	WS /api/v1/daemon       runtime, bidirectional, runtime bearer token
//
// The package knows nothing about runs, projects or SQL. Frames arriving from
// a daemon are validated against daemon-envelope@1 and handed to a
// ControlPlane; frames going out to browsers are opaque bytes somebody else
// marshalled. That is what keeps internal/run free to own every domain
// decision and keeps this package free of an import cycle with it.
//
// # Delivery
//
// A daemon's run.event stream is at-least-once and numbered per run, so this
// package never has to be exactly-once: internal/run deduplicates on
// (run_id, seq) in the database and only publishes what was actually new.
// A browser that falls behind is disconnected rather than buffered without
// bound — it reconnects with ?since={seq} and the handler replays the gap from
// the database before it forwards anything live.
package realtime
