package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/ChinnakornP/longtest/server/internal/db"
)

// Code is the stable, machine-readable half of the error envelope.
//
// The web app and the daemon switch on these strings, so they are part of the
// public contract: adding one is compatible, renaming one is not.
type Code string

// The error codes this API can return. Adding one is a compatible change;
// renaming one breaks every client that switches on it.
const (
	CodeBadRequest           Code = "bad_request"
	CodeValidationFailed     Code = "validation_failed"
	CodeUnauthorized         Code = "unauthorized"
	CodeForbidden            Code = "forbidden"
	CodeNotFound             Code = "not_found"
	CodeMethodNotAllowed     Code = "method_not_allowed"
	CodeConflict             Code = "conflict"
	CodeUnsupportedMediaType Code = "unsupported_media_type"
	CodePayloadTooLarge      Code = "payload_too_large"
	CodeRateLimited          Code = "rate_limited"
	CodeInternal             Code = "internal"
	CodeUnavailable          Code = "unavailable"
	CodeTimeout              Code = "timeout"
)

// genericInternalMessage is what a 5xx says to the client. The real cause is
// logged with the request id and never rendered: an unclassified error is
// frequently a driver error, and a pgconn message carries the SQL statement
// and the constraint name.
const genericInternalMessage = "internal server error"

// Error is an API error: a status code, a stable code, a message that is safe
// to show a user, and an optional cause that is logged but never rendered.
//
// Handlers return this rather than writing a response, so there is one place
// (WriteError) that decides what a failure looks like on the wire.
type Error struct {
	Status  int
	Code    Code
	Message string
	// Details carries structured context the client can act on, e.g.
	// {"fields": {...}} for a validation failure. It must never contain
	// anything the caller is not already allowed to see.
	Details map[string]any

	// cause is the underlying error. It is logged, never serialised.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s (%d %s): %v", e.Code, e.Status, e.Message, e.cause)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// Unwrap exposes the cause so errors.Is/As reach it, e.g. to tell a
// context.Canceled apart from a real failure in the access log.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches the underlying error for logging. The rendered body is
// unchanged, so this is always safe to call with a driver error.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

// WithDetails attaches structured, client-safe context.
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}

func newError(status int, code Code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// The constructors below are the whole vocabulary a handler needs. Anything
// that does not fit one of them is a bug and should be returned unwrapped so
// it surfaces as a logged 500 rather than a mislabelled 4xx.

// BadRequest is a malformed request: unparsable JSON, a bad uuid in a path.
func BadRequest(format string, args ...any) *Error {
	return newError(http.StatusBadRequest, CodeBadRequest, format, args...)
}

// Unauthorized means "not signed in", never "signed in but not allowed".
func Unauthorized(format string, args ...any) *Error {
	return newError(http.StatusUnauthorized, CodeUnauthorized, format, args...)
}

// Forbidden means the caller is authenticated but may not do this. Note that a
// resource belonging to another organization is a 404, not a 403: confirming
// that an id exists somewhere else is itself a cross-tenant leak.
func Forbidden(format string, args ...any) *Error {
	return newError(http.StatusForbidden, CodeForbidden, format, args...)
}

// NotFound covers both "no such row" and "that row is not yours".
func NotFound(format string, args ...any) *Error {
	return newError(http.StatusNotFound, CodeNotFound, format, args...)
}

// Conflict is a uniqueness or state conflict: the request is well-formed but
// the current state refuses it.
func Conflict(format string, args ...any) *Error {
	return newError(http.StatusConflict, CodeConflict, format, args...)
}

// RateLimited is 429. The caller sets Retry-After itself when it knows one.
func RateLimited(format string, args ...any) *Error {
	return newError(http.StatusTooManyRequests, CodeRateLimited, format, args...)
}

// Internal wraps an unexpected failure. The cause is logged; the client is
// told nothing beyond "internal server error".
func Internal(cause error) *Error {
	return (&Error{
		Status:  http.StatusInternalServerError,
		Code:    CodeInternal,
		Message: genericInternalMessage,
	}).WithCause(cause)
}

// FieldErrors is the details payload of a validation failure: field name to a
// sentence explaining what is wrong with it.
type FieldErrors map[string]string

// Invalid is a semantically invalid request body: the JSON parsed, the values
// are not acceptable. 422, so a client can tell it apart from malformed JSON.
func Invalid(fields FieldErrors) *Error {
	details := map[string]any{}
	if len(fields) > 0 {
		details["fields"] = map[string]string(fields)
	}
	return &Error{
		Status:  http.StatusUnprocessableEntity,
		Code:    CodeValidationFailed,
		Message: "the request body failed validation",
		Details: details,
	}
}

// InvalidField is Invalid for the common single-field case.
func InvalidField(field, problem string) *Error {
	return Invalid(FieldErrors{field: problem})
}

// AsError maps any error onto the API error it should be rendered as. It is
// the single mapping point named in the task contract: every layer below
// returns domain errors, and only this function decides their status code.
//
// An error it does not recognise becomes a 500 with the cause attached, so an
// unhandled failure is loud in the logs and silent on the wire.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}

	// A cancelled request is not a server fault; nothing is written to a
	// client that has already gone away, but the status drives the access log.
	if errors.Is(err, context.Canceled) {
		return (&Error{
			Status:  StatusClientClosedRequest,
			Code:    CodeUnavailable,
			Message: "client closed the request",
		}).WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return (&Error{
			Status:  http.StatusGatewayTimeout,
			Code:    CodeTimeout,
			Message: "the request took too long",
		}).WithCause(err)
	}

	// Database sentinels. internal/db already stripped the driver detail off
	// these; what is left is a classification, not a message.
	switch {
	case errors.Is(err, db.ErrNotFound):
		return NotFound("not found").WithCause(err)
	case errors.Is(err, db.ErrConflict):
		return Conflict("that already exists").WithCause(err)
	case errors.Is(err, db.ErrInvalidReference):
		return Conflict("the request refers to something that does not exist").WithCause(err)
	case errors.Is(err, db.ErrInvalidValue):
		return Invalid(nil).WithCause(err)
	case errors.Is(err, db.ErrSerializationFailure):
		return Conflict("the request lost a write race, retry it").WithCause(err)
	}

	return Internal(err)
}
