// Package metrics defines all Prometheus metrics for the inference gateway.
// Metrics are registered against a custom registry (not the global default)
// so tests can create isolated registries.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus collectors for the gateway.
type Metrics struct {
	// RequestsTotal counts total requests by model, status code, and streaming.
	RequestsTotal *prometheus.CounterVec

	// RequestDuration observes total request duration (including streaming).
	RequestDuration *prometheus.HistogramVec

	// TimeToFirstToken observes TTFT: time from request to first content chunk.
	TimeToFirstToken *prometheus.HistogramVec

	// InterTokenLatency observes ITL: time between consecutive content chunks.
	InterTokenLatency *prometheus.HistogramVec

	// TokensPerSecond observes tokens generated per second per request.
	TokensPerSecond *prometheus.HistogramVec

	// TokensPerRequest observes token counts per request.
	TokensPerRequest *prometheus.HistogramVec

	// CompletionTokensPerSecondByPromptBucket observes completion throughput
	// grouped by bounded prompt token buckets.
	CompletionTokensPerSecondByPromptBucket *prometheus.HistogramVec

	// PromptTokensTotal counts total prompt tokens processed.
	PromptTokensTotal *prometheus.CounterVec

	// CompletionTokensTotal counts total completion tokens generated.
	CompletionTokensTotal *prometheus.CounterVec

	// ActiveStreams tracks currently active streaming connections.
	ActiveStreams *prometheus.GaugeVec

	// BackendHealth tracks backend health: 1=healthy, 0=unhealthy.
	BackendHealth *prometheus.GaugeVec

	// RateLimitHitsTotal counts rate limit rejections.
	RateLimitHitsTotal *prometheus.CounterVec

	// BackendRequestDuration observes backend response time.
	BackendRequestDuration *prometheus.HistogramVec

	// Registry is the Prometheus registry these metrics are registered with.
	Registry *prometheus.Registry
}

// New creates and registers all gateway metrics with the given registry.
// If reg is nil, a new registry is created.
func New(reg *prometheus.Registry) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}

	m := &Metrics{
		Registry: reg,

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total requests processed by the gateway.",
		}, []string{"model", "status_code", "streaming"}),

		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Total request duration including streaming.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "streaming"}),

		TimeToFirstToken: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_time_to_first_token_seconds",
			Help:    "Time from request to first content chunk (TTFT).",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"model"}),

		InterTokenLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_inter_token_latency_seconds",
			Help:    "Time between consecutive content chunks (ITL).",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, []string{"model"}),

		TokensPerSecond: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_tokens_per_second",
			Help:    "Tokens generated per second per request.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"model", "type"}),

		TokensPerRequest: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_tokens_per_request",
			Help: "Token counts per request.",
			Buckets: []float64{
				1, 8, 16, 32, 64, 128, 256, 512,
				1024, 2048, 4096, 8192, 16384, 32768,
				65536, 131072,
			},
		}, []string{"model", "type"}),

		CompletionTokensPerSecondByPromptBucket: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_completion_tokens_per_second_by_prompt_bucket",
			Help:    "Completion tokens per second grouped by prompt token bucket.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"model", "prompt_tokens_bucket"}),

		PromptTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_prompt_tokens_total",
			Help: "Total prompt tokens processed.",
		}, []string{"model"}),

		CompletionTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_completion_tokens_total",
			Help: "Total completion tokens generated.",
		}, []string{"model"}),

		ActiveStreams: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_active_streams",
			Help: "Currently active streaming connections.",
		}, []string{"model"}),

		BackendHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_backend_health",
			Help: "Backend health status: 1=healthy, 0=unhealthy.",
		}, []string{"model", "backend_url"}),

		RateLimitHitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limit_hits_total",
			Help: "Total rate limit rejections.",
		}, []string{"key_prefix", "limit_type"}),

		BackendRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_backend_request_duration_seconds",
			Help:    "Backend response time.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "backend_url"}),
	}

	// Register all collectors.
	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.TimeToFirstToken,
		m.InterTokenLatency,
		m.TokensPerSecond,
		m.TokensPerRequest,
		m.CompletionTokensPerSecondByPromptBucket,
		m.PromptTokensTotal,
		m.CompletionTokensTotal,
		m.ActiveStreams,
		m.BackendHealth,
		m.RateLimitHitsTotal,
		m.BackendRequestDuration,
	)

	return m
}
