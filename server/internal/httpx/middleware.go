package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Middleware wraps a handler. Chain applies them outermost-first.
type Middleware func(http.Handler) http.Handler

// Chain wraps h so that mw[0] is the outermost middleware:
//
//	Chain(h, RequestID, Recover, AccessLog)
//
// runs RequestID first, and its request id is therefore available to Recover
// and AccessLog.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// requestIDHeader is both read (to join a trace that started at the proxy) and
// written (so a user can quote it in a bug report).
const requestIDHeader = "X-Request-ID"

// RequestID gives every request an id and puts a logger tagged with it into
// the context.
//
// An inbound X-Request-ID is honoured only if it looks like one: it is echoed
// in a response header and written to logs, so an arbitrary client string
// would be a log-injection vector.
func RequestID(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitiseRequestID(r.Header.Get(requestIDHeader))
			if id == "" {
				id = uuid.NewString()
			}

			w.Header().Set(requestIDHeader, id)
			ctx := WithRequestID(r.Context(), id)
			ctx = WithLogger(ctx, logger.With(slog.String("request_id", id)))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sanitiseRequestID accepts a bounded, printable-ASCII id and rejects
// everything else (control characters, newlines, oversized values).
func sanitiseRequestID(raw string) string {
	const maxLen = 64
	if raw == "" || len(raw) > maxLen {
		return ""
	}
	for _, c := range raw {
		isAllowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !isAllowed {
			return ""
		}
	}
	return raw
}

// Recover turns a panic into a 500 with the standard envelope.
//
// A panic in one request must not take the process down and must not leak a
// stack trace to the client; the stack goes to the log, next to the request id.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				// http.ErrAbortHandler is the documented way to abandon a
				// response; re-panicking lets net/http handle it as intended.
				if p == http.ErrAbortHandler { //nolint:errorlint // sentinel identity, not an error value
					panic(p)
				}

				LoggerFrom(r.Context()).ErrorContext(r.Context(), "handler panicked",
					slog.Any("panic", p),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)

				if rec, ok := w.(*responseRecorder); ok && rec.wroteHeader {
					// The response is already on the wire; the best available
					// signal is the truncated body the client will see.
					return
				}
				WriteJSON(w, r, http.StatusInternalServerError, errorEnvelope{Error: errorBody{
					Code:    CodeInternal,
					Message: genericInternalMessage,
				}})
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// responseRecorder captures the status code and byte count for the access log,
// and lets Recover tell "nothing written yet" from "already committed".
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (rw *responseRecorder) WriteHeader(status int) {
	if rw.wroteHeader {
		return
	}
	rw.status = status
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseRecorder) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += n
	return n, err //nolint:wrapcheck // transparent pass-through of the writer's error
}

// Unwrap lets http.ResponseController reach the underlying writer, which is
// what the WebSocket upgrade in T09 needs to hijack the connection.
func (rw *responseRecorder) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// AccessLog logs one line per request after it completes.
//
// The query string is deliberately not logged: it is the one part of a URL a
// client controls and the most likely place for a token to end up.
func AccessLog() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.status >= http.StatusInternalServerError:
				level = slog.LevelError
			case rec.status >= http.StatusBadRequest:
				level = slog.LevelWarn
			}

			LoggerFrom(r.Context()).Log(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("took", time.Since(start).Round(time.Microsecond)),
			)
		})
	}
}

// Timeout bounds how long a handler may run.
//
// Every layer below takes the request context, so this is what eventually
// cancels a slow query rather than letting it hold a pool connection until the
// client gives up.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SecurityHeaders sets the handful that matter for a JSON API.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			// Nothing here is meant to be framed or rendered as a document.
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			next.ServeHTTP(w, r)
		})
	}
}

// CORSConfig configures the browser-facing origin allowlist.
type CORSConfig struct {
	// AllowedOrigins is an exact-match allowlist, e.g.
	// "http://localhost:3000". "*" is deliberately not supported: the session
	// is a cookie, and a wildcard cannot be combined with credentials.
	AllowedOrigins []string
	// MaxAge caps how long a browser may cache a preflight.
	MaxAge time.Duration
}

// corsAllowedHeaders is the request-header allowlist. X-Org-ID is on it
// because it is how the active organization is selected on every call, and
// Idempotency-Key because POST /runs is retried with one.
var corsAllowedHeaders = strings.Join([]string{
	"Content-Type", "X-Org-ID", "Idempotency-Key", requestIDHeader,
}, ", ")

var corsAllowedMethods = strings.Join([]string{
	http.MethodGet, http.MethodPost, http.MethodPatch,
	http.MethodDelete, http.MethodOptions,
}, ", ")

// CORS answers preflights and adds the response headers for allowed origins.
//
// Credentials are allowed because the session is an httpOnly cookie, which is
// exactly why the allowlist is exact-match: with Allow-Credentials, reflecting
// an arbitrary Origin would let any site read a signed-in user's data.
func CORS(cfg CORSConfig) Middleware {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o = strings.TrimSpace(o); o != "" && o != "*" {
			allowed[o] = true
		}
	}
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			// Vary regardless of the outcome: a cache must not serve a
			// same-origin response to a cross-origin request.
			w.Header().Add("Vary", "Origin")

			if origin != "" && allowed[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", requestIDHeader)
			}

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h := w.Header()
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				if origin != "" && allowed[origin] {
					h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
					h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
					h.Set("Access-Control-Max-Age", strconv.Itoa(int(maxAge/time.Second)))
				}
				// A preflight is answered here whether or not the origin is
				// allowed; a disallowed one simply carries no CORS headers, and
				// the browser blocks the real request.
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
