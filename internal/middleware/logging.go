package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
)

// Logging returns middleware that emits a structured log line for every
// completed request. The log includes:
//
//   - request_id  (from RequestID middleware)
//   - method      (HTTP method)
//   - path        (URL path)
//   - status      (HTTP status code)
//   - duration_ms (request duration in milliseconds)
//   - model       (from MetricsData, if set by proxy handler)
//   - key_prefix  (from authenticated APIKey context, if present)
//   - backend_url (from MetricsData, if set by proxy handler)
//   - streaming   (from MetricsData, if set by proxy handler)
//
// This middleware should be placed early in the chain (after RequestID)
// so it wraps the full request lifecycle.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap the response writer to capture the status code.
			// statusWriter is defined in metrics.go (same package).
			sw := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(sw, r)

			duration := time.Since(start)

			// Build structured log attributes.
			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.statusCode),
				slog.Float64("duration_ms", float64(duration.Milliseconds())),
			}

			// Enrich with proxy-level data if available.
			if md := GetMetricsData(r.Context()); md != nil {
				if md.Model != "" {
					attrs = append(attrs, slog.String("model", md.Model))
				}
				if md.BackendURL != "" {
					attrs = append(attrs, slog.String("backend_url", md.BackendURL))
				}
				if md.Streaming {
					attrs = append(attrs, slog.Bool("streaming", true))
				}
			}

			// Enrich with API key prefix if authenticated.
			if apiKey := auth.APIKeyFromContext(r.Context()); apiKey != nil {
				attrs = append(attrs, slog.String("key_prefix", apiKey.KeyPrefix))
			}

			// Choose log level based on status code.
			level := slog.LevelInfo
			if sw.statusCode >= 500 {
				level = slog.LevelError
			} else if sw.statusCode >= 400 {
				level = slog.LevelWarn
			}

			// Convert []slog.Attr to []any for LogAttrs.
			logger.LogAttrs(r.Context(), level, "request completed", attrs...)
		})
	}
}
