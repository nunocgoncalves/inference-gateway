package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestID_GeneratesNewID(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(inner)

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	// Should be a 32-char hex string (16 bytes).
	require.Len(t, capturedID, 32)
	assert.Regexp(t, `^[0-9a-f]{32}$`, capturedID)

	// Response header should match.
	assert.Equal(t, capturedID, rec.Header().Get("X-Request-ID"))
}

func TestRequestID_UsesIncomingID(t *testing.T) {
	incomingID := "my-custom-request-id-12345"

	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(inner)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("X-Request-ID", incomingID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, incomingID, capturedID)
	assert.Equal(t, incomingID, rec.Header().Get("X-Request-ID"))
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	ids := make(map[string]bool)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		ids[id] = true
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(inner)

	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/health", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	// All 100 requests should have unique IDs.
	assert.Len(t, ids, 100)
}

func TestRequestID_EmptyHeaderGeneratesID(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "") // Explicitly empty.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Empty header should be treated as missing — generate a new one.
	require.Len(t, capturedID, 32)
	assert.Regexp(t, `^[0-9a-f]{32}$`, capturedID)
}

func TestRequestIDFromContext_NoID(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	id := RequestIDFromContext(req.Context())
	assert.Equal(t, "", id)
}

func TestRequestID_SetOnResponseEvenOnError(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	handler := RequestID(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	// Even on error, the response should have X-Request-ID.
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
}
