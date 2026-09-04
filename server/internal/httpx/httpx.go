// Package httpx is the HTTP contract of the backend: one error envelope, one
// place where an error becomes a status code, and the middleware every route
// runs behind.
//
// It deliberately knows nothing about organizations, sessions or the database
// schema. `internal/auth` and `internal/org` (and, from T08, every domain
// package) depend on this package; it depends on none of them, so there is
// exactly one implementation of the wire contract and no import cycle.
//
// The error envelope is frozen here:
//
//	{"error": {"code": "not_found", "message": "run not found"}}
//	{"error": {"code": "validation_failed", "message": "...",
//	           "details": {"fields": {"email": "must be an e-mail address"}}}}
//
// `code` is a stable machine-readable string - the web app switches on it, so
// adding a code is a compatible change and renaming one is not. `message` is
// human-readable English, safe to show to a user, and never contains a driver
// message, a SQL fragment or a constraint name.
package httpx

import "net/http"

// StatusClientClosedRequest is nginx's non-standard code for "the client hung
// up mid-request". Nothing is written to a disconnected client; it exists so
// the access log can tell an abandoned request from a served one.
const StatusClientClosedRequest = 499

// Handler is a handler that may fail. Returning an error is how a handler
// hands the response over to WriteError, so no handler needs to remember the
// status code for "the row was not found".
type Handler func(w http.ResponseWriter, r *http.Request) error

// ServeHTTP makes Handler an http.Handler: the returned error is rendered as
// the error envelope, and nothing else in the handler has to touch the
// response on a failure path.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		WriteError(w, r, err)
	}
}
