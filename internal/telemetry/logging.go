// Package telemetry wires up Conduit's observability. For now that is
// structured logging; metrics and tracing land here in later phases.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/darshpatel1052/conduit/internal/config"
)

// NewLogger builds the process logger.
//
// JSON is the default because gateway logs are read by machines first: a log
// pipeline can index model, provider and latency as real fields, which is
// impossible with a formatted string. Text output stays available for local
// development where a human is the reader.
func NewLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.Source}

	var handler slog.Handler
	if strings.ToLower(cfg.Format) == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

type ctxKey struct{}

// WithLogger stores a logger on the context so handlers deep in the stack can
// log with the request's fields (request ID, model, route) already attached.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// LoggerFrom returns the request-scoped logger, falling back to the default so
// callers never have to nil-check.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && log != nil {
		return log
	}
	return slog.Default()
}
