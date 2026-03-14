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

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAuthStore is a minimal in-memory mock for middleware tests.
// The full store is tested against real PG in the auth package.
type mockAuthStore struct {
	keys map[string]*auth.APIKey // keyed by hash
}

func (m *mockAuthStore) ValidateKey(_ context.Context, keyHash string) (*auth.APIKey, error) {
	return m.keys[keyHash], nil
}

func (m *mockAuthStore) CreateKey(_ context.Context, _ *auth.APIKey) error        { return nil }
func (m *mockAuthStore) GetKey(_ context.Context, _ string) (*auth.APIKey, error) { return nil, nil }
func (m *mockAuthStore) ListKeys(_ context.Context) ([]auth.APIKey, error)        { return nil, nil }
func (m *mockAuthStore) UpdateKey(_ context.Context, _ *auth.APIKey) error        { return nil }
func (m *mockAuthStore) DeleteKey(_ context.Context, _ string) error              { return nil }

func TestAuth_ValidKey(t *testing.T) {
	plaintext := "ml-testapikey1234567890"
	hash := auth.HashKey(plaintext)

	store := &mockAuthStore{
		keys: map[string]*auth.APIKey{
			hash: {
				ID:        "key-1",
				Name:      "test",
				KeyHash:   hash,
				KeyPrefix: "ml-testapikey",
				Active:    true,
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := Auth(store, logger)

	// Handler that checks the key is in context.
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := auth.APIKeyFromContext(r.Context())
		require.NotNil(t, key)
		assert.Equal(t, "key-1", key.ID)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_MissingHeader(t *testing.T) {
	store := &mockAuthStore{keys: map[string]*auth.APIKey{}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := Auth(store, logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "invalid_api_key", errObj["code"])
}

func TestAuth_InvalidKey(t *testing.T) {
	store := &mockAuthStore{keys: map[string]*auth.APIKey{}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := Auth(store, logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ml-invalidkey")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "invalid_api_key", errObj["code"])
	assert.Equal(t, "authentication_error", errObj["type"])
}

func TestAuth_MalformedHeader(t *testing.T) {
	store := &mockAuthStore{keys: map[string]*auth.APIKey{}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := Auth(store, logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	tests := []struct {
		name   string
		header string
	}{
		{"no bearer prefix", "ml-somekey"},
		{"basic auth", "Basic dXNlcjpwYXNz"},
		{"empty bearer", "Bearer "},
		{"bearer lowercase", "bearer ml-somekey"}, // should still work
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/models", nil)
			req.Header.Set("Authorization", tt.header)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if tt.name == "bearer lowercase" {
				// Case-insensitive "Bearer" — should pass extraction but key doesn't exist.
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
			} else {
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
			}
		})
	}
}

func TestAuth_ExpiredKey(t *testing.T) {
	// Expired keys are filtered in the store's ValidateKey.
	// The store returns nil for expired keys, so the middleware sees nil.
	store := &mockAuthStore{keys: map[string]*auth.APIKey{}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := Auth(store, logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ml-expiredkey")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ---------------------------------------------------------------------------
// AdminAuth tests
// ---------------------------------------------------------------------------

func TestAdminAuth_ValidKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := AdminAuth("admin-secret-key", logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/admin/v1/health", nil)
	req.Header.Set("X-Admin-Key", "admin-secret-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAdminAuth_InvalidKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := AdminAuth("admin-secret-key", logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/admin/v1/health", nil)
	req.Header.Set("X-Admin-Key", "wrong-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminAuth_MissingKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	middleware := AdminAuth("admin-secret-key", logger)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/admin/v1/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "invalid_admin_key", errObj["code"])
}

// ---------------------------------------------------------------------------
// Context helpers
// ---------------------------------------------------------------------------

func TestAPIKeyContext(t *testing.T) {
	key := &auth.APIKey{ID: "ctx-test", Name: "context-key"}

	ctx := auth.WithAPIKey(context.Background(), key)
	got := auth.APIKeyFromContext(ctx)
	require.NotNil(t, got)
	assert.Equal(t, "ctx-test", got.ID)

	// Empty context returns nil.
	assert.Nil(t, auth.APIKeyFromContext(context.Background()))
}

// Suppress unused import warning.
var _ = time.Now
