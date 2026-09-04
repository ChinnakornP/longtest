package httpx

import (
	"context"
	"log/slog"
)

type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
)

// WithRequestID stores the request id for the rest of the request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request id, or "" outside a served request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithLogger stores the per-request logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFrom returns the per-request logger, already tagged with the request
// id. Outside a served request (or in a test that skipped the middleware) it
// returns slog.Default() rather than nil, so no call site needs a guard.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
