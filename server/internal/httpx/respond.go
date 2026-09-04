package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorEnvelope is the frozen wire shape of a failure. See the package doc.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteJSON renders v as the response body.
//
// The body is marshalled before the status line is written, so a value that
// cannot be marshalled becomes a 500 instead of a 200 with a truncated body.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Nothing has been written yet, so this can still be turned into a
		// clean error response.
		WriteError(w, r, Internal(err))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Responses are per-caller and frequently carry another tenant's data on
	// the next request over the same connection.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The client hanging up mid-write is not actionable and not an error the
	// handler can do anything about; the access log already records the status.
	_, _ = w.Write(body)
}

// WriteNoContent ends a request that has nothing to say.
func WriteNoContent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// WriteError renders err as the error envelope.
//
// This is the only place in the backend that turns an error into a status
// code. A 5xx is logged here with its cause and the request id; the client
// receives the generic message and nothing else.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := AsError(err)
	if apiErr == nil {
		return
	}

	if apiErr.Status >= http.StatusInternalServerError {
		LoggerFrom(r.Context()).ErrorContext(r.Context(), "request failed",
			slog.String("code", string(apiErr.Code)),
			slog.Int("status", apiErr.Status),
			// The cause, not the rendered message: this is the only copy of it.
			slog.Any("err", errCause(apiErr)),
		)
	}

	if apiErr.Status == StatusClientClosedRequest {
		// There is nobody left to read a response body. Recording the status on
		// the wrapped writer keeps the access log honest.
		if rec, ok := w.(*responseRecorder); ok {
			rec.status = StatusClientClosedRequest
		}
		return
	}

	WriteJSON(w, r, apiErr.Status, errorEnvelope{Error: errorBody{
		Code:    apiErr.Code,
		Message: apiErr.Message,
		Details: apiErr.Details,
	}})
}

// errCause returns the cause for logging, falling back to the error itself so
// a 500 without an attached cause is still logged with something useful.
func errCause(e *Error) error {
	if e.cause != nil {
		return e.cause
	}
	return e
}
