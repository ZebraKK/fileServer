// Package observe provides structured logging and Prometheus metrics
// shared across all layers of the CDN cache service.
package observe

import (
	"context"
	"log/slog"
	"os"
)

type ctxKey struct{}

// NewLogger creates a JSON slog logger writing to stdout.
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// WithLogger attaches logger to ctx so middleware and handlers can retrieve it.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored in ctx, or the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
