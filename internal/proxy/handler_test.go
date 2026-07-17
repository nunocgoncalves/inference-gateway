package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestHandler_NonStreaming_ContentLengthMatchesRewrittenBody reproduces HOR-374:
// when rewrite_model_name changes the response body length, the gateway must not
// forward the backend's stale Content-Length. A too-large Content-Length makes
// ingress-nginx read past EOF and return 502.
func TestHandler_NonStreaming_ContentLengthMatchesRewrittenBody(t *testing.T) {
	cache := &fakeReader{
		catalog: map[string]snapshot.CatalogEntry{
			// alias "qwen3.6-27b" (11 chars) rewrites from a much longer backend id.
			"qwen3.6-27b": {ModelID: "qwen3.6-27b", BackendModelID: "Qwen/Qwen3.6-27B-FP8", Available: true, Transforms: snapshot.Transforms{RewriteModelName: true}},
		},
		caps: map[string][]snapshot.Capability{"identity-1": {{Resource: "*"}}},
	}

	// Backend returns a body whose model field is the long backend id, and
	// explicitly sets Content-Length to that body's length (as vLLM does).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respBody := []byte(`{"model":"Qwen/Qwen3.6-27B-FP8","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	}))
	defer backend.Close()
	e := cache.catalog["qwen3.6-27b"]
	e.BackendURL = backend.URL
	cache.catalog["qwen3.6-27b"] = e

	h := NewHandler(cache, nil, nil, slog.Default())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"qwen3.6-27b","messages":[{"role":"user","content":"hi"}]}`)))
	req = req.WithContext(middleware.WithIdentityID(req.Context(), "identity-1"))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "non-streaming should succeed")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "qwen3.6-27b", resp["model"], "response model rewritten to alias")

	// The recorder does not synthesize Content-Length like a real net/http
	// server does, so we assert the precise defect directly: the backend's
	// stale Content-Length (sized for the longer, pre-rewrite body) must NOT
	// be forwarded. A real net/http server then computes it from the body.
	result := rec.Result()
	defer result.Body.Close()
	assert.Empty(t, result.Header.Get("Content-Length"),
		"stale backend Content-Length must not be forwarded after body rewrite")
	// And the rewritten body itself must be intact and shorter than the
	// backend's — proving a forwarded stale value would have been too large.
	backendBodyLen := len(`{"model":"Qwen/Qwen3.6-27B-FP8","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`)
	assert.Equal(t, rec.Body.Len(), len(rec.Body.Bytes()),
		"rewritten body length is internally consistent")
	assert.Less(t, rec.Body.Len(), backendBodyLen,
		"test precondition: rewritten body is shorter than the backend body, so a stale Content-Length would be too large")
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
