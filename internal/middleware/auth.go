package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
)

// Auth returns a chi-compatible middleware that validates Bearer tokens against
// the auth store. On success it injects the APIKey into the request context.
// On failure it returns an OpenAI-compatible 401 error.
func Auth(store auth.Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearerToken(r)
			if token == "" {
				writeAuthError(w, http.StatusUnauthorized,
					"missing or invalid Authorization header",
					"invalid_api_key")
				return
			}

			keyHash := auth.HashKey(token)
			key, err := store.ValidateKey(r.Context(), keyHash)
			if err != nil {
				logger.Error("auth store error", "error", err)
				writeAuthError(w, http.StatusInternalServerError,
					"internal server error",
					"server_error")
				return
			}
			if key == nil {
				writeAuthError(w, http.StatusUnauthorized,
					"invalid API key",
					"invalid_api_key")
				return
			}

			ctx := auth.WithAPIKey(r.Context(), key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AdminAuth returns a middleware that validates the admin API key from the
// X-Admin-Key header. This is separate from the Bearer token auth used by
// client-facing endpoints.
func AdminAuth(adminKey string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Admin-Key")
			if key == "" || key != adminKey {
				writeAuthError(w, http.StatusUnauthorized,
					"invalid or missing admin key",
					"invalid_admin_key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractBearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// writeAuthError writes an OpenAI-compatible JSON error response.
func writeAuthError(w http.ResponseWriter, status int, message string, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "authentication_error",
			"code":    code,
		},
	})
}
