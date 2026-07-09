package middleware

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

// RateLimit enforces the per-identity RPM/TPM limits from the control-plane
// (permissions.effective_rate_limits). There are NO gateway-side defaults: an
// identity with no rate-limit policy is unlimited (the control-plane's rule).
// The counter is in Redis, keyed by identity_id (shared across the identity's
// keys and across pods). TPM is a pre-flight check; the actual TPM increment
// happens after the response (in the proxy handler).
func RateLimit(cache snapshot.Reader, limiter ratelimit.Limiter, m *metrics.Metrics, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identityID := IdentityIDFromContext(r.Context())
			if identityID == "" {
				next.ServeHTTP(w, r)
				return
			}
			rl, ok := cache.RateLimits(identityID)
			if !ok {
				// No per-identity rate-limit policy -> unlimited (control-plane rule).
				next.ServeHTTP(w, r)
				return
			}

			rpmResult, err := limiter.CheckRPM(r.Context(), identityID, rl.RPM)
			if err != nil {
				logger.Error("rate limit check failed", "error", err, "identity_id", identityID)
				next.ServeHTTP(w, r) // fail open
				return
			}
			setRateLimitHeaders(w, "Requests", rpmResult)
			if !rpmResult.Allowed {
				if m != nil {
					m.RateLimitHitsTotal.WithLabelValues(identityID, "rpm").Inc()
				}
				writeRateLimitError(w, rpmResult.ResetAt)
				return
			}

			tpmResult, err := limiter.CheckTPM(r.Context(), identityID, rl.TPM)
			if err != nil {
				logger.Error("TPM check failed", "error", err, "identity_id", identityID)
				next.ServeHTTP(w, r)
				return
			}
			setRateLimitHeaders(w, "Tokens", tpmResult)
			if !tpmResult.Allowed {
				if m != nil {
					m.RateLimitHitsTotal.WithLabelValues(identityID, "tpm").Inc()
				}
				writeRateLimitError(w, tpmResult.ResetAt)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func setRateLimitHeaders(w http.ResponseWriter, kind string, result *ratelimit.Result) {
	w.Header().Set(fmt.Sprintf("X-RateLimit-Limit-%s", kind), fmt.Sprintf("%d", result.Limit))
	w.Header().Set(fmt.Sprintf("X-RateLimit-Remaining-%s", kind), fmt.Sprintf("%d", result.Remaining))
	w.Header().Set(fmt.Sprintf("X-RateLimit-Reset-%s", kind), fmt.Sprintf("%.0fs", time.Until(result.ResetAt).Seconds()))
}

func writeRateLimitError(w http.ResponseWriter, resetAt time.Time) {
	retryAfter := time.Until(resetAt).Seconds()
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retryAfter))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "Rate limit exceeded. Please retry after the specified time.",
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
		},
	}); err != nil {
		slog.Error("failed to write rate limit error response", "error", err)
	}
}
