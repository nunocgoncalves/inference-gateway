package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RegistersAllMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	// Gather all registered metric families.
	families, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	// All metrics should not appear yet (no observations) — but they
	// should be registered. We verify by observing/incrementing each one
	// and then gathering.

	// Increment/observe all metrics with sample label values.
	m.RequestsTotal.WithLabelValues("test-model", "200", "false").Inc()
	m.RequestDuration.WithLabelValues("test-model", "false").Observe(0.5)
	m.TimeToFirstToken.WithLabelValues("test-model").Observe(0.1)
	m.InterTokenLatency.WithLabelValues("test-model").Observe(0.01)
	m.TokensPerSecond.WithLabelValues("test-model", "completion").Observe(50)
	m.TokensPerRequest.WithLabelValues("test-model", "prompt").Observe(1024)
	m.CompletionTokensPerSecondByPromptBucket.WithLabelValues("test-model", "le_1k").Observe(50)
	m.PromptTokensTotal.WithLabelValues("test-model").Add(100)
	m.CompletionTokensTotal.WithLabelValues("test-model").Add(200)
	m.ActiveStreams.WithLabelValues("test-model").Inc()
	m.BackendHealth.WithLabelValues("test-model", "http://localhost:8000").Set(1)
	m.RateLimitHitsTotal.WithLabelValues("ml-abc123", "rpm").Inc()
	m.BackendRequestDuration.WithLabelValues("test-model", "http://localhost:8000").Observe(0.3)

	families, err = reg.Gather()
	require.NoError(t, err)

	names = make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	expected := []string{
		"gateway_requests_total",
		"gateway_request_duration_seconds",
		"gateway_time_to_first_token_seconds",
		"gateway_inter_token_latency_seconds",
		"gateway_tokens_per_second",
		"gateway_tokens_per_request",
		"gateway_completion_tokens_per_second_by_prompt_bucket",
		"gateway_prompt_tokens_total",
		"gateway_completion_tokens_total",
		"gateway_active_streams",
		"gateway_backend_health",
		"gateway_rate_limit_hits_total",
		"gateway_backend_request_duration_seconds",
	}

	for _, name := range expected {
		assert.True(t, names[name], "metric %q should be registered", name)
	}
}

func TestNew_NilRegistry(t *testing.T) {
	m := New(nil)
	require.NotNil(t, m)
	require.NotNil(t, m.Registry)

	// Verify metrics work with the auto-created registry.
	m.RequestsTotal.WithLabelValues("m", "200", "false").Inc()
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	assert.NotEmpty(t, families)
}

func TestCounterIncrements(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	// Increment requests counter.
	m.RequestsTotal.WithLabelValues("gpt-4", "200", "true").Inc()
	m.RequestsTotal.WithLabelValues("gpt-4", "200", "true").Inc()
	m.RequestsTotal.WithLabelValues("gpt-4", "500", "false").Inc()

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_requests_total")
	require.NotNil(t, fam)

	// Should have 2 metric series.
	assert.Len(t, fam.GetMetric(), 2)

	// Find the 200/true series.
	for _, metric := range fam.GetMetric() {
		labels := labelMap(metric)
		if labels["status_code"] == "200" && labels["streaming"] == "true" {
			assert.Equal(t, 2.0, metric.GetCounter().GetValue())
		}
		if labels["status_code"] == "500" {
			assert.Equal(t, 1.0, metric.GetCounter().GetValue())
		}
	}
}

func TestHistogramObservations(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.RequestDuration.WithLabelValues("llama", "false").Observe(1.5)
	m.RequestDuration.WithLabelValues("llama", "false").Observe(2.5)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_request_duration_seconds")
	require.NotNil(t, fam)
	require.Len(t, fam.GetMetric(), 1)

	hist := fam.GetMetric()[0].GetHistogram()
	assert.Equal(t, uint64(2), hist.GetSampleCount())
	assert.InDelta(t, 4.0, hist.GetSampleSum(), 0.001)
}

func TestGaugeSetAndGet(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.BackendHealth.WithLabelValues("gpt-4", "http://backend1:8000").Set(1)
	m.BackendHealth.WithLabelValues("gpt-4", "http://backend2:8000").Set(0)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_backend_health")
	require.NotNil(t, fam)
	assert.Len(t, fam.GetMetric(), 2)

	for _, metric := range fam.GetMetric() {
		labels := labelMap(metric)
		if labels["backend_url"] == "http://backend1:8000" {
			assert.Equal(t, 1.0, metric.GetGauge().GetValue())
		} else {
			assert.Equal(t, 0.0, metric.GetGauge().GetValue())
		}
	}
}

func TestActiveStreamsGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.ActiveStreams.WithLabelValues("model-a").Inc()
	m.ActiveStreams.WithLabelValues("model-a").Inc()
	m.ActiveStreams.WithLabelValues("model-a").Dec()

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_active_streams")
	require.NotNil(t, fam)
	require.Len(t, fam.GetMetric(), 1)
	assert.Equal(t, 1.0, fam.GetMetric()[0].GetGauge().GetValue())
}

func TestTokensPerSecondLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.TokensPerSecond.WithLabelValues("model-x", "prompt").Observe(100)
	m.TokensPerSecond.WithLabelValues("model-x", "completion").Observe(50)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_tokens_per_second")
	require.NotNil(t, fam)
	assert.Len(t, fam.GetMetric(), 2)
}

func TestTokensPerRequestLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.TokensPerRequest.WithLabelValues("model-x", "prompt").Observe(2048)
	m.TokensPerRequest.WithLabelValues("model-x", "completion").Observe(128)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_tokens_per_request")
	require.NotNil(t, fam)
	assert.Len(t, fam.GetMetric(), 2)
}

func TestCompletionTokensPerSecondByPromptBucketLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.CompletionTokensPerSecondByPromptBucket.WithLabelValues("model-x", "8k_16k").Observe(25)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_completion_tokens_per_second_by_prompt_bucket")
	require.NotNil(t, fam)
	require.Len(t, fam.GetMetric(), 1)

	labels := labelMap(fam.GetMetric()[0])
	assert.Equal(t, "model-x", labels["model"])
	assert.Equal(t, "8k_16k", labels["prompt_tokens_bucket"])
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findFamily(families []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func labelMap(m *dto.Metric) map[string]string {
	out := make(map[string]string)
	for _, lp := range m.GetLabel() {
		out[lp.GetName()] = lp.GetValue()
	}
	return out
}
