package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

// identityIDKey is the context key for the authenticated identity id.
type identityIDKey struct{}

// WithIdentityID stores the authenticated identity id in the context.
func WithIdentityID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, identityIDKey{}, id)
}

// IdentityIDFromContext returns the authenticated identity id, or "" if unset.
func IdentityIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(identityIDKey{}).(string)
	return id
}

// HashKey returns the sha256 hex of an API key token (the form stored in
// identity.api_keys.key_hash).
func HashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Auth validates a Bearer API key against the control-plane identity snapshot
// (key_hash -> identity_id) and injects the identity_id into the request
// context. 401 on missing/invalid/revoked keys.
func Auth(cache snapshot.Reader, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearerToken(r)
			if token == "" {
				writeAuthError(w, http.StatusUnauthorized, "missing or invalid Authorization header", "invalid_api_key")
				return
			}
			identityID, ok := cache.IdentityByAPIKey(HashKey(token))
			if !ok {
				writeAuthError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentityID(r.Context(), identityID)))
		})
	}
}

// AdminAuth validates the admin API key from the X-Admin-Key header (env-configured).
// Used only for the debug endpoint; the legacy admin CRUD API is gone.
func AdminAuth(adminKey string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Admin-Key")
			if key == "" || key != adminKey {
				writeAuthError(w, http.StatusUnauthorized, "invalid or missing admin key", "invalid_admin_key")
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

func writeAuthError(w http.ResponseWriter, status int, message string, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "authentication_error",
			"code":    code,
		},
	}); err != nil {
		slog.Error("failed to write auth error response", "error", err)
	}
}
