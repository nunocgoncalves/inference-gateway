package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
	"github.com/stretchr/testify/assert"
)

func TestRateLimit_Allows_UsesPerIdentityLimit(t *testing.T) {
	cache := &fakeReader{rateLimits: map[string]snapshot.IdentityRateLimits{"identity-1": {RPM: 60, TPM: 100000}}}
	limiter := &fakeLimiter{rpmAllowed: true}
	h := RateLimit(cache, limiter, RateLimitConfig{DefaultRPM: 10, DefaultTPM: 1000}, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 60, limiter.lastRPMLimit, "per-identity limit (60) must override the default (10)")
}

func TestRateLimit_Blocked(t *testing.T) {
	cache := &fakeReader{rateLimits: map[string]snapshot.IdentityRateLimits{"identity-1": {RPM: 60, TPM: 100000}}}
	h := RateLimit(cache, &fakeLimiter{rpmAllowed: false}, RateLimitConfig{DefaultRPM: 10}, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should block") }))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestRateLimit_DefaultWhenNoPolicy(t *testing.T) {
	cache := &fakeReader{} // no per-identity policy -> default
	limiter := &fakeLimiter{rpmAllowed: true}
	h := RateLimit(cache, limiter, RateLimitConfig{DefaultRPM: 10, DefaultTPM: 1000}, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-2"))
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, 10, limiter.lastRPMLimit, "default limit used when no per-identity policy")
}
