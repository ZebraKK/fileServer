// Package server wires the HTTP servers together and provides shared middleware.
package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"fileServer/internal/observe"
)

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += n
	return n, err
}

// RequestLogger returns a chi-compatible middleware that emits one structured
// log line per request after it completes.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		// Inject a child logger that carries the request ID.
		reqID := middleware.GetReqID(r.Context())
		logger := observe.FromContext(r.Context()).With(
			slog.String("request_id", reqID),
		)
		r = r.WithContext(observe.WithLogger(r.Context(), logger))

		next.ServeHTTP(rw, r)

		// Extract domain: use Host header, fall back to RemoteAddr.
		domain := r.Host

		latency := time.Since(start)
		logger.Info("request",
			slog.String("domain", domain),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rw.status),
			slog.Int("bytes", rw.bytes),
			slog.String("client_ip", r.RemoteAddr),
			slog.Int64("latency_ms", latency.Milliseconds()),
		)

		// Update Prometheus metrics.
		statusStr := fmt.Sprintf("%d", rw.status)
		observe.RequestsTotal.WithLabelValues(domain, statusStr).Inc()
		observe.RequestDuration.WithLabelValues(domain, "").Observe(latency.Seconds())
	})
}

// Recovery returns a middleware that catches panics, logs them, and returns 500.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				observe.FromContext(r.Context()).Error("panic recovered",
					slog.Any("error", rec),
					slog.String("path", r.URL.Path),
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
