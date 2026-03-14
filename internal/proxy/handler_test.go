package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()
	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("gateway_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	require.NoError(t, err)
	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, database.MigrateUp(connStr))
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	cleanup := func() {
		pool.Close()
		require.NoError(t, pgContainer.Terminate(ctx))
	}
	return pool, cleanup
}

func setupTestRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()
	ctx := context.Background()
	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	connStr, err := redisContainer.ConnectionString(ctx)
	require.NoError(t, err)
	opts, err := redis.ParseURL(connStr)
	require.NoError(t, err)
	rdb := redis.NewClient(opts)
	require.NoError(t, rdb.Ping(ctx).Err())
	cleanup := func() {
		rdb.Close()
		require.NoError(t, redisContainer.Terminate(ctx))
	}
	return rdb, cleanup
}

// mockVLLM creates a test HTTP server that mimics vLLM responses.
func mockVLLM(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.URL.Path == "/v1/chat/completions" {
			// Parse request to get model name.
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)

			model, _ := req["model"].(string)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test123",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "Hello! How can I help you?",
						},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     10,
					"completion_tokens": 8,
					"total_tokens":      18,
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
}

// seedTestModel creates a model with a backend pointing to the mock vLLM.
func seedTestModel(t *testing.T, store *registry.PGStore, mockURL string) *registry.Model {
	t.Helper()
	ctx := context.Background()

	m := &registry.Model{
		Name:    "test-model",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		Active:  true,
	}
	require.NoError(t, store.CreateModel(ctx, m))
	require.NoError(t, store.CreateBackend(ctx, &registry.Backend{
		ModelID: m.ID,
		URL:     mockURL,
		Weight:  1,
		Active:  true,
	}))
	return m
}

func setupHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	pool, dbCleanup := setupTestDB(t)
	rdb, redisCleanup := setupTestRedis(t)

	store := registry.NewPGStore(pool)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()

	vllm := mockVLLM(t)
	seedTestModel(t, store, vllm.URL)

	cache := registry.NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))

	handler := NewHandler(cache, nil, nil, nil, logger)

	cleanup := func() {
		cache.Stop()
		vllm.Close()
		redisCleanup()
		dbCleanup()
	}
	return handler, cleanup
}

func makeAuthContext(req *http.Request) *http.Request {
	key := &auth.APIKey{
		ID:     "test-key-id",
		Name:   "test",
		Active: true,
	}
	ctx := auth.WithAPIKey(req.Context(), key)
	return req.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// ChatCompletions tests
// ---------------------------------------------------------------------------

func TestChatCompletions_NonStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	handler, cleanup := setupHandler(t)
	defer cleanup()

	body := `{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = makeAuthContext(req)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// Model name should be rewritten back to the alias.
	assert.Equal(t, "test-model", resp["model"])
	assert.Equal(t, "chat.completion", resp["object"])

	choices := resp["choices"].([]any)
	assert.Len(t, choices, 1)

	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Contains(t, msg["content"], "Hello")

	usage := resp["usage"].(map[string]any)
	assert.Equal(t, float64(18), usage["total_tokens"])
}

func TestChatCompletions_ModelRewrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Custom vLLM that echoes back the model it received.
	var receivedModel string
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   receivedModel,
			"object":  "chat.completion",
			"choices": []any{},
			"usage":   map[string]any{"total_tokens": 0},
		})
	}))
	defer vllm.Close()

	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := registry.NewPGStore(pool)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()

	m := &registry.Model{
		Name:    "my-alias",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		Active:  true,
	}
	require.NoError(t, store.CreateModel(ctx, m))
	require.NoError(t, store.CreateBackend(ctx, &registry.Backend{
		ModelID: m.ID, URL: vllm.URL, Weight: 1, Active: true,
	}))

	cache := registry.NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	handler := NewHandler(cache, nil, nil, nil, logger)

	body := `{"model": "my-alias", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req = makeAuthContext(req)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	// vLLM should have received the actual model ID.
	assert.Equal(t, "meta-llama/Llama-3.1-70B-Instruct", receivedModel)

	// Response should have the alias name.
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "my-alias", resp["model"])
}

func TestChatCompletions_ModelNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	handler, cleanup := setupHandler(t)
	defer cleanup()

	body := `{"model": "nonexistent", "messages": [{"role": "user", "content": "Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req = makeAuthContext(req)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "model_not_found", errObj["code"])
}

func TestChatCompletions_MissingModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	handler, cleanup := setupHandler(t)
	defer cleanup()

	body := `{"messages": [{"role": "user", "content": "Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req = makeAuthContext(req)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestChatCompletions_InvalidJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	handler, cleanup := setupHandler(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("not json"))
	req = makeAuthContext(req)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestChatCompletions_ModelNotAllowed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	handler, cleanup := setupHandler(t)
	defer cleanup()

	body := `{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))

	// Key that only allows "other-model".
	key := &auth.APIKey{
		ID:            "restricted-key",
		Name:          "restricted",
		Active:        true,
		AllowedModels: []string{"other-model"},
	}
	ctx := auth.WithAPIKey(req.Context(), key)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestChatCompletions_BackendDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := registry.NewPGStore(pool)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()

	m := &registry.Model{Name: "down-model", ModelID: "test/model", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))
	// Backend URL points to nothing.
	require.NoError(t, store.CreateBackend(ctx, &registry.Backend{
		ModelID: m.ID, URL: "http://127.0.0.1:1", Weight: 1, Active: true,
	}))

	cache := registry.NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	handler := NewHandler(cache, nil, nil, nil, logger)

	body := `{"model": "down-model", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req = makeAuthContext(req)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// ---------------------------------------------------------------------------
// ListModels tests
// ---------------------------------------------------------------------------

func TestListModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	handler, cleanup := setupHandler(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req = makeAuthContext(req)
	rec := httptest.NewRecorder()

	handler.ListModels(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "list", resp["object"])

	data := resp["data"].([]any)
	assert.Len(t, data, 1)

	model := data[0].(map[string]any)
	assert.Equal(t, "test-model", model["id"])
	assert.Equal(t, "model", model["object"])
	assert.Equal(t, "inference-gateway", model["owned_by"])
}

func TestListModels_FilteredByKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := registry.NewPGStore(pool)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()

	require.NoError(t, store.CreateModel(ctx, &registry.Model{Name: "model-a", ModelID: "m1", Active: true}))
	require.NoError(t, store.CreateModel(ctx, &registry.Model{Name: "model-b", ModelID: "m2", Active: true}))
	require.NoError(t, store.CreateModel(ctx, &registry.Model{Name: "model-c", ModelID: "m3", Active: true}))

	cache := registry.NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	handler := NewHandler(cache, nil, nil, nil, logger)

	// Key that only allows model-a and model-c.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	key := &auth.APIKey{
		ID:            "filtered-key",
		Name:          "filtered",
		Active:        true,
		AllowedModels: []string{"model-a", "model-c"},
	}
	ctx2 := auth.WithAPIKey(req.Context(), key)
	req = req.WithContext(ctx2)
	rec := httptest.NewRecorder()

	handler.ListModels(rec, req)

	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	data := resp["data"].([]any)
	assert.Len(t, data, 2)

	names := make([]string, len(data))
	for i, d := range data {
		names[i] = d.(map[string]any)["id"].(string)
	}
	assert.Contains(t, names, "model-a")
	assert.Contains(t, names, "model-c")
	assert.NotContains(t, names, "model-b")
}

// ---------------------------------------------------------------------------
// Rewrite helpers tests
// ---------------------------------------------------------------------------

func TestRewriteModelInRequest(t *testing.T) {
	body := []byte(`{"model": "my-alias", "messages": [{"role": "user", "content": "hi"}], "temperature": 0.7}`)
	rewritten := rewriteModelInRequest(body, "meta-llama/Llama-3.1-70B-Instruct")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &parsed))
	assert.Equal(t, "meta-llama/Llama-3.1-70B-Instruct", parsed["model"])
	assert.Equal(t, 0.7, parsed["temperature"])
}

func TestRewriteModelInResponse(t *testing.T) {
	body := []byte(`{"model": "meta-llama/Llama-3.1-70B-Instruct", "object": "chat.completion"}`)
	rewritten := rewriteModelInResponse(body, "my-alias")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &parsed))
	assert.Equal(t, "my-alias", parsed["model"])
	assert.Equal(t, "chat.completion", parsed["object"])
}
