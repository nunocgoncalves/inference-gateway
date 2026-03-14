package middleware

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
)

// RateLimitConfig holds the global default rate limits.
type RateLimitConfig struct {
	DefaultRPM int
	DefaultTPM int
}

// RateLimit returns a chi-compatible middleware that enforces RPM rate limits.
// It reads the authenticated API key from context (set by the Auth middleware)
// and checks against the key's RPM limit (or the global default).
//
// TPM is checked here as a pre-flight check. The actual TPM increment happens
// after the response completes (in the proxy handler).
//
// The metrics parameter is optional — pass nil to disable rate limit hit tracking.
func RateLimit(limiter ratelimit.Limiter, cfg RateLimitConfig, m *metrics.Metrics, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.APIKeyFromContext(r.Context())
			if key == nil {
				// No key in context — auth middleware should have rejected.
				// Pass through; this shouldn't happen in normal flow.
				next.ServeHTTP(w, r)
				return
			}

			// Determine RPM limit: key-specific or global default.
			rpmLimit := cfg.DefaultRPM
			if key.RPMLimit != nil && *key.RPMLimit > 0 {
				rpmLimit = *key.RPMLimit
			}

			// Check RPM.
			rpmResult, err := limiter.CheckRPM(r.Context(), key.ID, rpmLimit)
			if err != nil {
				logger.Error("rate limit check failed", "error", err, "key_id", key.ID)
				// Fail open — don't block requests on rate limiter errors.
				next.ServeHTTP(w, r)
				return
			}

			// Set RPM headers regardless of outcome.
			setRateLimitHeaders(w, "Requests", rpmResult)

			if !rpmResult.Allowed {
				if m != nil {
					m.RateLimitHitsTotal.WithLabelValues(key.KeyPrefix, "rpm").Inc()
				}
				writeRateLimitError(w, rpmResult.ResetAt)
				return
			}

			// Pre-flight TPM check (non-blocking — just checks current usage).
			tpmLimit := cfg.DefaultTPM
			if key.TPMLimit != nil && *key.TPMLimit > 0 {
				tpmLimit = *key.TPMLimit
			}

			tpmResult, err := limiter.CheckTPM(r.Context(), key.ID, tpmLimit)
			if err != nil {
				logger.Error("TPM check failed", "error", err, "key_id", key.ID)
				next.ServeHTTP(w, r)
				return
			}

			setRateLimitHeaders(w, "Tokens", tpmResult)

			if !tpmResult.Allowed {
				if m != nil {
					m.RateLimitHitsTotal.WithLabelValues(key.KeyPrefix, "tpm").Inc()
				}
				writeRateLimitError(w, tpmResult.ResetAt)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func setRateLimitHeaders(w http.ResponseWriter, kind string, result *ratelimit.Result) {
	w.Header().Set(fmt.Sprintf("X-RateLimit-Limit-%s", kind),
		fmt.Sprintf("%d", result.Limit))
	w.Header().Set(fmt.Sprintf("X-RateLimit-Remaining-%s", kind),
		fmt.Sprintf("%d", result.Remaining))
	w.Header().Set(fmt.Sprintf("X-RateLimit-Reset-%s", kind),
		fmt.Sprintf("%.0fs", time.Until(result.ResetAt).Seconds()))
}

func writeRateLimitError(w http.ResponseWriter, resetAt time.Time) {
	retryAfter := time.Until(resetAt).Seconds()
	if retryAfter < 1 {
		retryAfter = 1
	}

	w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retryAfter))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "Rate limit exceeded. Please retry after the specified time.",
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
		},
	})
}
