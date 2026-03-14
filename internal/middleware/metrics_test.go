package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
)

func TestMetrics_RecordsRequestsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(m)(inner)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_requests_total")
	require.NotNil(t, fam, "gateway_requests_total should exist")
	require.Len(t, fam.GetMetric(), 1)

	metric := fam.GetMetric()[0]
	labels := labelMap(metric)
	assert.Equal(t, "unknown", labels["model"])
	assert.Equal(t, "200", labels["status_code"])
	assert.Equal(t, "false", labels["streaming"])
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestMetrics_RecordsWithMetricsData(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the proxy handler setting MetricsData in context.
		md := &MetricsData{Model: "gpt-4", Streaming: true}
		ctx := SetMetricsData(r.Context(), md)
		*r = *r.WithContext(ctx)
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(m)(inner)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_requests_total")
	require.NotNil(t, fam)
	require.Len(t, fam.GetMetric(), 1)

	labels := labelMap(fam.GetMetric()[0])
	assert.Equal(t, "gpt-4", labels["model"])
	assert.Equal(t, "200", labels["status_code"])
	assert.Equal(t, "true", labels["streaming"])
}

func TestMetrics_RecordsRequestDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(m)(inner)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_request_duration_seconds")
	require.NotNil(t, fam, "gateway_request_duration_seconds should exist")
	require.Len(t, fam.GetMetric(), 1)

	hist := fam.GetMetric()[0].GetHistogram()
	assert.Equal(t, uint64(1), hist.GetSampleCount())
	assert.Greater(t, hist.GetSampleSum(), 0.0)
}

func TestMetrics_Captures4xxStatusCode(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	handler := Metrics(m)(inner)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_requests_total")
	require.NotNil(t, fam)
	labels := labelMap(fam.GetMetric()[0])
	assert.Equal(t, "404", labels["status_code"])
}

func TestMetrics_Captures5xxStatusCode(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	handler := Metrics(m)(inner)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_requests_total")
	require.NotNil(t, fam)
	labels := labelMap(fam.GetMetric()[0])
	assert.Equal(t, "502", labels["status_code"])
}

func TestMetrics_FlusherDelegation(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	flushed := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(m)(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, flushed, "Flusher should be delegated through statusWriter")
}

func TestMetrics_MultipleRequests(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(m)(inner)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findFamily(families, "gateway_requests_total")
	require.NotNil(t, fam)
	assert.Equal(t, 5.0, fam.GetMetric()[0].GetCounter().GetValue())

	histFam := findFamily(families, "gateway_request_duration_seconds")
	require.NotNil(t, histFam)
	assert.Equal(t, uint64(5), histFam.GetMetric()[0].GetHistogram().GetSampleCount())
}

func TestSetGetMetricsData(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)

	// No data initially.
	md := GetMetricsData(req.Context())
	assert.Nil(t, md)

	// Set data.
	data := &MetricsData{Model: "llama-3", Streaming: true}
	ctx := SetMetricsData(req.Context(), data)

	md = GetMetricsData(ctx)
	require.NotNil(t, md)
	assert.Equal(t, "llama-3", md.Model)
	assert.True(t, md.Streaming)
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
