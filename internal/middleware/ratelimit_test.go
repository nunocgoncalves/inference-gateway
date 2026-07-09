package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
	"github.com/stretchr/testify/assert"
)

func TestRateLimit_EnforcesPerIdentityLimit(t *testing.T) {
	cache := &fakeReader{rateLimits: map[string]snapshot.IdentityRateLimits{"identity-1": {RPM: 60, TPM: 100000}}}
	limiter := &fakeLimiter{rpmAllowed: true}
	h := RateLimit(cache, limiter, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 60, limiter.lastRPMLimit, "limit comes from the control-plane policy, not a gateway default")
}

func TestRateLimit_Blocked(t *testing.T) {
	cache := &fakeReader{rateLimits: map[string]snapshot.IdentityRateLimits{"identity-1": {RPM: 60, TPM: 100000}}}
	h := RateLimit(cache, &fakeLimiter{rpmAllowed: false}, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should block") }))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestRateLimit_NoPolicyIsUnlimited(t *testing.T) {
	// No per-identity rate-limit policy -> unlimited (control-plane rule). The
	// limiter must NOT be consulted (rpmAllowed=false would block if it were).
	cache := &fakeReader{} // no rateLimits
	limiter := &fakeLimiter{rpmAllowed: false}
	h := RateLimit(cache, limiter, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-2"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "no policy -> unlimited (limiter not consulted)")
	assert.Equal(t, 0, limiter.lastRPMLimit, "limiter.CheckRPM not called")
}
