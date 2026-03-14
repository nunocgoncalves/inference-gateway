package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ---------------------------------------------------------------------------
// Test infrastructure (streaming-specific)
// ---------------------------------------------------------------------------

func setupStreamTestDB(t *testing.T) (*pgxpool.Pool, func()) {
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

func setupStreamTestRedis(t *testing.T) (*redis.Client, func()) {
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

// mockStreamingVLLM creates a test server that returns SSE streaming responses.
func mockStreamingVLLM(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		model, _ := req["model"].(string)

		// Verify stream_options were injected.
		streamOpts, _ := req["stream_options"].(map[string]any)
		includeUsage, _ := streamOpts["include_usage"].(bool)
		continuousStats, _ := streamOpts["continuous_usage_stats"].(bool)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher := w.(http.Flusher)

		// Chunk 1: role
		chunk1 := map[string]any{
			"id":      "chatcmpl-stream-test",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil},
			},
		}
		if includeUsage && continuousStats {
			chunk1["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 0, "total_tokens": 10}
		}
		writeSSEChunk(w, flusher, chunk1)

		// Chunk 2: content "Hello"
		chunk2 := map[string]any{
			"id":     "chatcmpl-stream-test",
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": nil},
			},
		}
		if includeUsage && continuousStats {
			chunk2["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11}
		}
		writeSSEChunk(w, flusher, chunk2)

		// Chunk 3: content " world"
		chunk3 := map[string]any{
			"id":     "chatcmpl-stream-test",
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": " world"}, "finish_reason": nil},
			},
		}
		if includeUsage && continuousStats {
			chunk3["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
		}
		writeSSEChunk(w, flusher, chunk3)

		// Chunk 4: finish
		chunk4 := map[string]any{
			"id":     "chatcmpl-stream-test",
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			},
		}
		if includeUsage && continuousStats {
			chunk4["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
		}
		writeSSEChunk(w, flusher, chunk4)

		// Final usage chunk (if include_usage).
		if includeUsage {
			usageChunk := map[string]any{
				"id":      "chatcmpl-stream-test",
				"object":  "chat.completion.chunk",
				"model":   model,
				"choices": []any{},
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
			}
			writeSSEChunk(w, flusher, usageChunk)
		}

		// [DONE]
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func writeSSEChunk(w http.ResponseWriter, flusher http.Flusher, chunk map[string]any) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func setupStreamHandler(t *testing.T, vllmURL string, limiter ratelimit.Limiter) (*Handler, *registry.Cache, func()) {
	t.Helper()
	pool, dbCleanup := setupStreamTestDB(t)
	rdb, redisCleanup := setupStreamTestRedis(t)

	store := registry.NewPGStore(pool)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()

	m := &registry.Model{
		Name:    "stream-model",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		Active:  true,
	}
	require.NoError(t, store.CreateModel(ctx, m))
	require.NoError(t, store.CreateBackend(ctx, &registry.Backend{
		ModelID: m.ID, URL: vllmURL, Weight: 1, Active: true,
	}))

	cache := registry.NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))

	handler := NewHandler(cache, limiter, nil, logger)

	cleanup := func() {
		cache.Stop()
		redisCleanup()
		dbCleanup()
	}
	return handler, cache, cleanup
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestStreaming_FullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	vllm := mockStreamingVLLM(t)
	defer vllm.Close()

	handler, _, cleanup := setupStreamHandler(t, vllm.URL, nil)
	defer cleanup()

	body := `{"model": "stream-model", "messages": [{"role": "user", "content": "Hello"}], "stream": true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	key := &auth.APIKey{ID: "test-key", Name: "test", Active: true}
	ctx := auth.WithAPIKey(req.Context(), key)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))

	// Parse SSE chunks.
	chunks := parseSSEResponse(t, rec.Body.String())

	// Should have content chunks + [DONE].
	require.True(t, len(chunks) >= 4, "expected at least 4 chunks, got %d", len(chunks))

	// All chunks should have model rewritten to alias.
	for _, chunk := range chunks {
		if model, ok := chunk["model"].(string); ok {
			assert.Equal(t, "stream-model", model, "model should be rewritten to alias")
		}
	}

	// Verify content was streamed.
	var content strings.Builder
	for _, chunk := range chunks {
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			content.WriteString(c)
		}
	}
	assert.Equal(t, "Hello world", content.String())

	// Verify [DONE] was sent.
	assert.True(t, strings.Contains(rec.Body.String(), "data: [DONE]"))
}

func TestStreaming_ModelRewrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	var receivedModel string
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		receivedModel, _ = req["model"].(string)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunk := map[string]any{
			"model":   receivedModel,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}
		writeSSEChunk(w, flusher, chunk)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer vllm.Close()

	handler, _, cleanup := setupStreamHandler(t, vllm.URL, nil)
	defer cleanup()

	body := `{"model": "stream-model", "messages": [{"role": "user", "content": "Hi"}], "stream": true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	key := &auth.APIKey{ID: "test-key", Name: "test", Active: true}
	req = req.WithContext(auth.WithAPIKey(req.Context(), key))

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	// vLLM should have received the actual model ID.
	assert.Equal(t, "meta-llama/Llama-3.1-70B-Instruct", receivedModel)

	// Response chunks should have the alias.
	chunks := parseSSEResponse(t, rec.Body.String())
	for _, chunk := range chunks {
		if model, ok := chunk["model"].(string); ok {
			assert.Equal(t, "stream-model", model)
		}
	}
}

func TestStreaming_InjectStreamOptions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	var receivedOpts map[string]any
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		receivedOpts, _ = req["stream_options"].(map[string]any)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer vllm.Close()

	handler, _, cleanup := setupStreamHandler(t, vllm.URL, nil)
	defer cleanup()

	// Client does NOT set stream_options.
	body := `{"model": "stream-model", "messages": [{"role": "user", "content": "Hi"}], "stream": true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	key := &auth.APIKey{ID: "test-key", Name: "test", Active: true}
	req = req.WithContext(auth.WithAPIKey(req.Context(), key))

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	// Gateway should have injected stream_options.
	require.NotNil(t, receivedOpts, "stream_options should be injected")
	assert.Equal(t, true, receivedOpts["include_usage"])
	assert.Equal(t, true, receivedOpts["continuous_usage_stats"])
}

func TestStreaming_TPMTracking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	vllm := mockStreamingVLLM(t)
	defer vllm.Close()

	// Set up a real Redis limiter for TPM tracking.
	rdb, redisCleanup := setupStreamTestRedis(t)
	defer redisCleanup()
	limiter := ratelimit.NewRedisLimiter(rdb)

	handler, _, cleanup := setupStreamHandler(t, vllm.URL, limiter)
	defer cleanup()

	body := `{"model": "stream-model", "messages": [{"role": "user", "content": "Hello"}], "stream": true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	key := &auth.APIKey{ID: "tpm-track-key", Name: "test", Active: true}
	req = req.WithContext(auth.WithAPIKey(req.Context(), key))

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	// Give the batcher time to flush (it flushes on Stop which is deferred).
	time.Sleep(100 * time.Millisecond)

	// Check that TPM was incremented. The mock sends total_tokens=12.
	result, err := limiter.CheckTPM(context.Background(), "tpm-track-key", 100000)
	require.NoError(t, err)
	// Should have consumed some tokens (12 from the mock).
	assert.True(t, result.Remaining < 100000,
		"TPM should have been decremented, remaining=%d", result.Remaining)
}

func TestStreaming_BackendError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Backend that returns 500.
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "internal error", "type": "server_error"},
		})
	}))
	defer vllm.Close()

	handler, _, cleanup := setupStreamHandler(t, vllm.URL, nil)
	defer cleanup()

	body := `{"model": "stream-model", "messages": [{"role": "user", "content": "Hi"}], "stream": true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	key := &auth.APIKey{ID: "test-key", Name: "test", Active: true}
	req = req.WithContext(auth.WithAPIKey(req.Context(), key))

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	// Should forward the 500 error.
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ---------------------------------------------------------------------------
// Unit tests for helpers
// ---------------------------------------------------------------------------

func TestInjectStreamOptions(t *testing.T) {
	// No existing stream_options.
	body := []byte(`{"model": "test", "stream": true}`)
	result := injectStreamOptions(body)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	opts := parsed["stream_options"].(map[string]any)
	assert.Equal(t, true, opts["include_usage"])
	assert.Equal(t, true, opts["continuous_usage_stats"])
}

func TestInjectStreamOptions_PreservesExisting(t *testing.T) {
	body := []byte(`{"model": "test", "stream": true, "stream_options": {"some_other": true}}`)
	result := injectStreamOptions(body)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	opts := parsed["stream_options"].(map[string]any)
	assert.Equal(t, true, opts["include_usage"])
	assert.Equal(t, true, opts["continuous_usage_stats"])
	assert.Equal(t, true, opts["some_other"])
}

func TestProcessStreamChunk(t *testing.T) {
	data := `{"id":"test","model":"meta-llama/Llama-3.1-70B","choices":[{"delta":{"content":"Hi"}}],"usage":{"total_tokens":15}}`
	chunk, rewritten := processStreamChunk(data, "my-alias")

	require.NotNil(t, chunk)
	assert.Equal(t, 15, chunk.Usage.TotalTokens)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(rewritten), &parsed))
	assert.Equal(t, "my-alias", parsed["model"])
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseSSEResponse(t *testing.T, body string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}
