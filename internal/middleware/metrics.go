package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
)

// metricsContextKey is used to store request metadata for metrics.
type metricsContextKey struct{}

// MetricsData holds per-request data collected during processing and
// consumed by the metrics middleware when the response completes.
type MetricsData struct {
	Model     string
	Streaming bool
}

// Metrics returns middleware that records gateway_requests_total and
// gateway_request_duration_seconds for every request passing through.
//
// The model and streaming labels are populated from MetricsData stored
// in the request context by the proxy handler. If no MetricsData is
// present (e.g. /v1/models, error responses), model defaults to "unknown".
func Metrics(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap the response writer to capture the status code.
			sw := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(sw, r)

			duration := time.Since(start).Seconds()

			// Extract model/streaming from context if the proxy handler set it.
			model := "unknown"
			streaming := "false"
			if md, ok := r.Context().Value(metricsContextKey{}).(*MetricsData); ok && md != nil {
				if md.Model != "" {
					model = md.Model
				}
				if md.Streaming {
					streaming = "true"
				}
			}

			statusCode := fmt.Sprintf("%d", sw.statusCode)

			m.RequestsTotal.WithLabelValues(model, statusCode, streaming).Inc()
			m.RequestDuration.WithLabelValues(model, streaming).Observe(duration)
		})
	}
}

// statusWriter is an http.ResponseWriter wrapper that captures the status code.
type statusWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.statusCode = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

// Flush implements http.Flusher — required for SSE streaming to work
// through the metrics middleware.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// SetMetricsData stores per-request metrics data in the context.
// Called by the proxy handler once the model and streaming flag are known.
func SetMetricsData(ctx context.Context, data *MetricsData) context.Context {
	return context.WithValue(ctx, metricsContextKey{}, data)
}

// GetMetricsData retrieves per-request metrics data from the context.
func GetMetricsData(ctx context.Context) *MetricsData {
	md, _ := ctx.Value(metricsContextKey{}).(*MetricsData)
	return md
}
