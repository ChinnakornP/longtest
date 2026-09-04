package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// The daemon runs on somebody else's machine, where the only record of what
// happened is what it wrote down. Logs are therefore structured and go to a
// file by default, with a human-readable copy on stderr when the operator ran
// it in a terminal. Run events sent to the backend are a summary of this, not
// a replacement (ADR-002 makes every event a stored row).

// LogOptions configure the daemon logger.
type LogOptions struct {
	// Path is the structured log file. Empty disables file logging.
	Path string
	// Level is the minimum level recorded.
	Level slog.Level
	// Console adds a human-readable copy on stderr.
	Console bool
}

// NewLogger builds the daemon logger and returns a closer for the log file.
func NewLogger(opts LogOptions) (*slog.Logger, io.Closer, error) {
	var handlers []slog.Handler
	var closer io.Closer

	if opts.Path != "" {
		if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
			return nil, nil, fmt.Errorf("runtime: create log directory: %w", err)
		}
		file, err := os.OpenFile(opts.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator's own state dir
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: open log file: %w", err)
		}
		handlers = append(handlers, slog.NewJSONHandler(file, &slog.HandlerOptions{Level: opts.Level}))
		closer = file
	}
	if opts.Console || len(handlers) == 0 {
		handlers = append(handlers, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: opts.Level}))
	}

	logger := slog.New(fanout(handlers))
	return logger, closer, nil
}

// fanout writes each record to every handler.
type fanoutHandler []slog.Handler

func fanout(handlers []slog.Handler) slog.Handler {
	if len(handlers) == 1 {
		return handlers[0]
	}
	return fanoutHandler(handlers)
}

func (f fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range f {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range f {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		// Each handler gets its own clone: a handler that consumes the
		// record's attributes must not empty it for the next one.
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (f fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanoutHandler, len(f))
	for i, handler := range f {
		out[i] = handler.WithAttrs(attrs)
	}
	return out
}

func (f fanoutHandler) WithGroup(name string) slog.Handler {
	out := make(fanoutHandler, len(f))
	for i, handler := range f {
		out[i] = handler.WithGroup(name)
	}
	return out
}
