package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAuth_ValidKey(t *testing.T) {
	cache := &fakeReader{apiKeys: map[string]string{HashKey("secret-1"): "identity-1"}}
	called := false
	h := Auth(cache, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, "identity-1", IdentityIDFromContext(r.Context()))
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.True(t, called)
}

func TestAuth_InvalidKey(t *testing.T) {
	h := Auth(&fakeReader{apiKeys: map[string]string{}}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call next")
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_MissingHeader(t *testing.T) {
	h := Auth(&fakeReader{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call next")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
