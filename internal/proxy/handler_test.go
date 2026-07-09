package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReader is a snapshot.Reader fake for proxy unit tests.
type fakeReader struct {
	catalog map[string]snapshot.CatalogEntry
	caps    map[string][]snapshot.Capability
}

func (f *fakeReader) CatalogEntry(id string) (snapshot.CatalogEntry, bool) {
	e, ok := f.catalog[id]
	return e, ok
}
func (f *fakeReader) ListCatalog() []snapshot.CatalogEntry {
	out := make([]snapshot.CatalogEntry, 0, len(f.catalog))
	for _, e := range f.catalog {
		out = append(out, e)
	}
	return out
}
func (f *fakeReader) IdentityByAPIKey(string) (string, bool)       { return "identity-1", true }
func (f *fakeReader) Capabilities(id string) []snapshot.Capability { return f.caps[id] }
func (f *fakeReader) RateLimits(string) (snapshot.IdentityRateLimits, bool) {
	return snapshot.IdentityRateLimits{}, false
}
func (f *fakeReader) Fresh(time.Duration) bool { return true }
func (f *fakeReader) LastRefresh() time.Time   { return time.Now() }

// newTestHandler wires a Handler against a fake cache + an httptest backend that
// echoes the received model field and returns a minimal completion.
func newTestHandler(t *testing.T, cache snapshot.Reader) (*Handler, *httptest.Server, *string) {
	t.Helper()
	var gotModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if m, ok := req["model"].(string); ok {
			gotModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + gotModel + `","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`))
	}))
	return NewHandler(cache, nil, nil, slog.Default()), backend, &gotModel
}

func TestHandler_RoutesAndRewritesModel(t *testing.T) {
	cache := &fakeReader{
		catalog: map[string]snapshot.CatalogEntry{
			"qwen3-27b": {ModelID: "qwen3-27b", BackendModelID: "Qwen/Qwen3-27B", Available: true, Transforms: snapshot.Transforms{RewriteModelName: true}},
		},
		caps: map[string][]snapshot.Capability{"identity-1": {{Resource: "*", Action: "*"}}},
	}
	h, backend, gotModel := newTestHandler(t, cache)
	defer backend.Close()
	e := cache.catalog["qwen3-27b"]
	e.BackendURL = backend.URL
	cache.catalog["qwen3-27b"] = e

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`)))
	req = req.WithContext(middleware.WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Qwen/Qwen3-27B", *gotModel, "request model rewritten to backend HF id")
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "qwen3-27b", resp["model"], "response model rewritten back to alias")
}

func TestHandler_CapabilityDenied(t *testing.T) {
	cache := &fakeReader{
		catalog: map[string]snapshot.CatalogEntry{"qwen3-27b": {ModelID: "qwen3-27b", BackendURL: "http://x", Available: true}},
		caps:    map[string][]snapshot.Capability{"identity-1": {}}, // no capabilities
	}
	h, backend, _ := newTestHandler(t, cache)
	defer backend.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"qwen3-27b","messages":[]}`)))
	req = req.WithContext(middleware.WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandler_ModelNotFound(t *testing.T) {
	cache := &fakeReader{
		catalog: map[string]snapshot.CatalogEntry{},
		caps:    map[string][]snapshot.Capability{"identity-1": {{Resource: "*"}}},
	}
	h, backend, _ := newTestHandler(t, cache)
	defer backend.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"nope","messages":[]}`)))
	req = req.WithContext(middleware.WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandler_ModelUnavailable(t *testing.T) {
	cache := &fakeReader{
		catalog: map[string]snapshot.CatalogEntry{"qwen3-27b": {ModelID: "qwen3-27b", BackendURL: "http://x", Available: false}},
		caps:    map[string][]snapshot.Capability{"identity-1": {{Resource: "*"}}},
	}
	h, backend, _ := newTestHandler(t, cache)
	defer backend.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"qwen3-27b","messages":[]}`)))
	req = req.WithContext(middleware.WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestHandler_ListModels_OnlyAvailable(t *testing.T) {
	cache := &fakeReader{catalog: map[string]snapshot.CatalogEntry{
		"a": {ModelID: "a", Available: true},
		"b": {ModelID: "b", Available: false},
	}}
	h, backend, _ := newTestHandler(t, cache)
	defer backend.Close()
	rec := httptest.NewRecorder()
	h.ListModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	assert.Equal(t, "a", resp.Data[0].ID)
}
