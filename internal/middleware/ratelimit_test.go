package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
)

// mockLimiter is a minimal mock for middleware tests.
type mockLimiter struct {
	rpmResult *ratelimit.Result
	tpmResult *ratelimit.Result
	rpmErr    error
	tpmErr    error
}

func (m *mockLimiter) CheckRPM(_ context.Context, _ string, _ int) (*ratelimit.Result, error) {
	return m.rpmResult, m.rpmErr
}
func (m *mockLimiter) CheckTPM(_ context.Context, _ string, _ int) (*ratelimit.Result, error) {
	return m.tpmResult, m.tpmErr
}
func (m *mockLimiter) IncrementTPM(_ context.Context, _ string, _ int) error { return nil }

func newTestRequest(keyID string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	key := &auth.APIKey{
		ID:     keyID,
		Name:   "test",
		Active: true,
	}
	ctx := auth.WithAPIKey(req.Context(), key)
	return req.WithContext(ctx)
}

func TestRateLimit_Allowed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	limiter := &mockLimiter{
		rpmResult: &ratelimit.Result{
			Allowed:   true,
			Limit:     60,
			Remaining: 59,
			ResetAt:   time.Now().Add(60 * time.Second),
		},
		tpmResult: &ratelimit.Result{
			Allowed:   true,
			Limit:     100000,
			Remaining: 99000,
			ResetAt:   time.Now().Add(60 * time.Second),
		},
	}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, nil, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := newTestRequest("key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRateLimit_RPMExceededIncrementsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	limiter := &mockLimiter{
		rpmResult: &ratelimit.Result{
			Allowed:   false,
			Limit:     60,
			Remaining: 0,
			ResetAt:   time.Now().Add(30 * time.Second),
		},
	}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, m, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	key := &auth.APIKey{
		ID:        "key-metric",
		Name:      "metric-test",
		KeyPrefix: "ml-abc123",
		Active:    true,
	}
	ctx := auth.WithAPIKey(req.Context(), key)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findRLFamily(families, "gateway_rate_limit_hits_total")
	require.NotNil(t, fam, "rate_limit_hits_total should exist")
	require.Len(t, fam.GetMetric(), 1)

	metric := fam.GetMetric()[0]
	labels := rlLabelMap(metric)
	assert.Equal(t, "ml-abc123", labels["key_prefix"])
	assert.Equal(t, "rpm", labels["limit_type"])
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestRateLimit_TPMExceededIncrementsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	limiter := &mockLimiter{
		rpmResult: &ratelimit.Result{
			Allowed:   true,
			Limit:     60,
			Remaining: 50,
			ResetAt:   time.Now().Add(60 * time.Second),
		},
		tpmResult: &ratelimit.Result{
			Allowed:   false,
			Limit:     100000,
			Remaining: 0,
			ResetAt:   time.Now().Add(45 * time.Second),
		},
	}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, m, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	key := &auth.APIKey{
		ID:        "key-tpm",
		Name:      "tpm-test",
		KeyPrefix: "ml-xyz789",
		Active:    true,
	}
	ctx := auth.WithAPIKey(req.Context(), key)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	families, err := reg.Gather()
	require.NoError(t, err)

	fam := findRLFamily(families, "gateway_rate_limit_hits_total")
	require.NotNil(t, fam)
	require.Len(t, fam.GetMetric(), 1)

	metric := fam.GetMetric()[0]
	labels := rlLabelMap(metric)
	assert.Equal(t, "ml-xyz789", labels["key_prefix"])
	assert.Equal(t, "tpm", labels["limit_type"])
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func findRLFamily(families []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func rlLabelMap(m *dto.Metric) map[string]string {
	out := make(map[string]string)
	for _, lp := range m.GetLabel() {
		out[lp.GetName()] = lp.GetValue()
	}
	return out
}

func TestRateLimit_RPMExceeded(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	limiter := &mockLimiter{
		rpmResult: &ratelimit.Result{
			Allowed:   false,
			Limit:     60,
			Remaining: 0,
			ResetAt:   time.Now().Add(30 * time.Second),
		},
	}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, nil, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := newTestRequest("key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "rate_limit_exceeded", errObj["code"])
	assert.Equal(t, "rate_limit_error", errObj["type"])
}

func TestRateLimit_TPMExceeded(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	limiter := &mockLimiter{
		rpmResult: &ratelimit.Result{
			Allowed:   true,
			Limit:     60,
			Remaining: 50,
			ResetAt:   time.Now().Add(60 * time.Second),
		},
		tpmResult: &ratelimit.Result{
			Allowed:   false,
			Limit:     100000,
			Remaining: 0,
			ResetAt:   time.Now().Add(45 * time.Second),
		},
	}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, nil, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := newTestRequest("key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestRateLimit_NoKeyInContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	limiter := &mockLimiter{}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, nil, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No API key in context — should pass through.
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRateLimit_CustomKeyLimits(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rpmLimit := 200
	tpmLimit := 500000

	limiter := &mockLimiter{
		rpmResult: &ratelimit.Result{
			Allowed:   true,
			Limit:     200,
			Remaining: 199,
			ResetAt:   time.Now().Add(60 * time.Second),
		},
		tpmResult: &ratelimit.Result{
			Allowed:   true,
			Limit:     500000,
			Remaining: 499000,
			ResetAt:   time.Now().Add(60 * time.Second),
		},
	}

	mw := RateLimit(limiter, RateLimitConfig{DefaultRPM: 60, DefaultTPM: 100000}, nil, logger)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	key := &auth.APIKey{
		ID:       "key-custom",
		Name:     "custom",
		Active:   true,
		RPMLimit: &rpmLimit,
		TPMLimit: &tpmLimit,
	}
	ctx := auth.WithAPIKey(req.Context(), key)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}
