package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Logging returns middleware that emits a structured log line for every
// completed request. The log includes:
//
//   - request_id   (from RequestID middleware)
//   - method       (HTTP method)
//   - path         (URL Path)
//   - status       (HTTP status code)
//   - duration_ms  (request duration in milliseconds)
//   - disposition  (completed | denied | error | canceled) — a canceled stream
//     is distinguishable from a successful completion even though both write
//     HTTP 200 (HOR-398: cancellation reports an attributable outcome).
//   - model        (from MetricsData, if set by proxy handler)
//   - identity_id  (from the authenticated context, if present)
//   - backend_url  (from MetricsData, if set by proxy handler)
//   - streaming    (from MetricsData, if set by proxy handler)
//   - run_id, turn_id, pool_key (from WorkloadContext, on the mTLS workload path)
//
// This middleware should be placed early in the chain (after RequestID)
// so it wraps the full request lifecycle. Auth/workload middleware that runs
// inside it must propagate their context by mutating r in place (see
// WorkloadAuth) so the enriched scope is visible here at completion.
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
				slog.String("disposition", disposition(sw.statusCode, r.Context())),
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

			// Enrich with the authenticated identity id, if present.
			if identityID := IdentityIDFromContext(r.Context()); identityID != "" {
				attrs = append(attrs, slog.String("identity_id", identityID))
			}

			// Enrich with the workload (supervisor mTLS) scope, if present. This
			// keeps denials and outcomes attributable to the run/turn (SCN-009).
			if wc, ok := WorkloadContextFromContext(r.Context()); ok {
				attrs = append(attrs, slog.String("run_id", wc.RunID))
				attrs = append(attrs, slog.String("turn_id", wc.TurnID))
				attrs = append(attrs, slog.String("pool_key", wc.PoolKey))
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

// disposition classifies the request outcome. A client cancellation (the
// stream's request context is canceled) is reported as "canceled" even though
// the handler already wrote HTTP 200 for the stream — without this, a canceled
// workload stream would log as a successful completion (HOR-398).
func disposition(status int, ctx context.Context) string {
	if ctx != nil && ctx.Err() == context.Canceled {
		return "canceled"
	}
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "denied"
	default:
		return "completed"
	}
}
