package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nunocgoncalves/inference-gateway/internal/admin"
	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
	"github.com/nunocgoncalves/inference-gateway/internal/loadbalancer"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

const testAdminKey = "test-admin-secret-key"

type testEnv struct {
	handler       http.Handler
	pool          *pgxpool.Pool
	rdb           *redis.Client
	registryStore *registry.PGStore
	authStore     *auth.PGStore
	cache         *registry.Cache
	vllm          *httptest.Server
	m             *metrics.Metrics
	cleanup       func()
}

func setupIntegration(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// --- Postgres ---
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

	// --- Redis ---
	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	redisURL, err := redisContainer.ConnectionString(ctx)
	require.NoError(t, err)
	opts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	rdb := redis.NewClient(opts)
	require.NoError(t, rdb.Ping(ctx).Err())

	// --- Stores ---
	registryStore := registry.NewPGStore(pool)
	authStore := auth.NewPGStore(pool)

	// --- Cache ---
	cache := registry.NewCache(registryStore, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))

	// --- Rate limiter ---
	limiter := ratelimit.NewRedisLimiter(rdb)

	// --- Metrics ---
	m := metrics.New(prometheus.NewRegistry())

	// --- Mock vLLM ---
	vllm := mockVLLMServer(t)

	// --- Health checker (no-op for integration tests) ---
	hc := loadbalancer.NewHealthChecker(loadbalancer.HealthCheckConfig{
		Interval:           10 * time.Minute,
		Timeout:            5 * time.Second,
		HealthyThreshold:   1,
		UnhealthyThreshold: 1,
	}, logger, nil, m)

	// --- Handlers ---
	proxyHandler := proxy.NewHandler(cache, limiter, hc, m, logger)
	adminHandler := admin.NewHandler(registryStore, authStore, cache, logger)

	// --- Build router ---
	deps := &Deps{
		ProxyHandler: proxyHandler,
		AdminHandler: adminHandler,
		AuthStore:    authStore,
		Limiter:      limiter,
		RateLimitCfg: middleware.RateLimitConfig{
			DefaultRPM: 60,
			DefaultTPM: 100000,
		},
		AdminKey: testAdminKey,
	}
	router := newRouter(logger, m, deps)

	env := &testEnv{
		handler:       router,
		pool:          pool,
		rdb:           rdb,
		registryStore: registryStore,
		authStore:     authStore,
		cache:         cache,
		vllm:          vllm,
		m:             m,
		cleanup: func() {
			cache.Stop()
			vllm.Close()
			pool.Close()
			rdb.Close()
			pgContainer.Terminate(ctx)
			redisContainer.Terminate(ctx)
		},
	}

	return env
}

func mockVLLMServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.URL.Path == "/v1/chat/completions" {
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			model, _ := req["model"].(string)
			stream, _ := req["stream"].(bool)

			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				flusher := w.(http.Flusher)

				// Send a few content chunks.
				chunks := []string{"Hello", "!", " How", " can", " I help?"}
				for _, c := range chunks {
					chunk := map[string]any{
						"id":      "chatcmpl-stream",
						"object":  "chat.completion.chunk",
						"model":   model,
						"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": c}}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}

				// Final chunk with usage.
				finalChunk := map[string]any{
					"id":      "chatcmpl-stream",
					"object":  "chat.completion.chunk",
					"model":   model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
					"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
				}
				data, _ := json.Marshal(finalChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()

				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}

			// Non-streaming response.
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

// seedModel creates a model + backend via admin API.
func (env *testEnv) seedModel(t *testing.T, name, modelID string) string {
	t.Helper()
	ctx := context.Background()
	m := &registry.Model{Name: name, ModelID: modelID, Active: true}
	require.NoError(t, env.registryStore.CreateModel(ctx, m))
	require.NoError(t, env.registryStore.CreateBackend(ctx, &registry.Backend{
		ModelID: m.ID,
		URL:     env.vllm.URL,
		Weight:  1,
		Active:  true,
		Healthy: true,
	}))
	require.NoError(t, env.cache.Invalidate(ctx))
	// Give cache a moment to refresh.
	time.Sleep(50 * time.Millisecond)
	return m.ID
}

// createAPIKey creates a real API key and returns the plaintext.
func (env *testEnv) createAPIKey(t *testing.T, name string, allowedModels []string) string {
	t.Helper()
	ctx := context.Background()
	plaintext, _, _, err := auth.GenerateKey()
	require.NoError(t, err)
	hash := auth.HashKey(plaintext)
	prefix := plaintext[:len("ml-")+8]

	key := &auth.APIKey{
		Name:          name,
		KeyHash:       hash,
		KeyPrefix:     prefix,
		Active:        true,
		AllowedModels: allowedModels,
	}
	require.NoError(t, env.authStore.CreateKey(ctx, key))
	return plaintext
}

func doRequest(env *testEnv, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestIntegration_HealthEndpoint(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	rec := doRequest(env, "GET", "/health", nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
}

func TestIntegration_MetricsEndpoint(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	// Seed a model + key and make a request so metrics get observed.
	env.seedModel(t, "metrics-model", "meta-llama/Llama-3")
	apiKey := env.createAPIKey(t, "metrics-key", nil)
	doRequest(env, "GET", "/v1/models", nil, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})

	rec := doRequest(env, "GET", "/metrics", nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "gateway_requests_total")
	assert.Contains(t, body, "gateway_request_duration_seconds")
}

func TestIntegration_Auth_MissingKey(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	rec := doRequest(env, "GET", "/v1/models", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestIntegration_Auth_InvalidKey(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	rec := doRequest(env, "GET", "/v1/models", nil, map[string]string{
		"Authorization": "Bearer ml-invalid-key",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestIntegration_Auth_ValidKey(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	apiKey := env.createAPIKey(t, "test-key", nil)
	rec := doRequest(env, "GET", "/v1/models", nil, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestIntegration_ListModels(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	env.seedModel(t, "my-model", "meta-llama/Llama-3")
	apiKey := env.createAPIKey(t, "test-key", nil)

	rec := doRequest(env, "GET", "/v1/models", nil, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "list", body["object"])
	data := body["data"].([]any)
	assert.GreaterOrEqual(t, len(data), 1)
}

func TestIntegration_ChatCompletions_NonStreaming(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	env.seedModel(t, "test-model", "meta-llama/Llama-3")
	apiKey := env.createAPIKey(t, "test-key", nil)

	rec := doRequest(env, "POST", "/v1/chat/completions", map[string]any{
		"model": "test-model",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	// Model should be rewritten back to alias.
	assert.Equal(t, "test-model", body["model"])
	assert.NotEmpty(t, body["choices"])
	assert.NotEmpty(t, body["usage"])
}

func TestIntegration_ChatCompletions_Streaming(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	env.seedModel(t, "stream-model", "meta-llama/Llama-3")
	apiKey := env.createAPIKey(t, "test-key", nil)

	rec := doRequest(env, "POST", "/v1/chat/completions", map[string]any{
		"model":  "stream-model",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")

	// Parse SSE events.
	scanner := bufio.NewScanner(rec.Body)
	var events []string
	var gotDone bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				gotDone = true
				break
			}
			events = append(events, data)

			// Verify model is rewritten in chunks.
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) == nil {
				if m, ok := chunk["model"].(string); ok {
					assert.Equal(t, "stream-model", m)
				}
			}
		}
	}

	assert.True(t, gotDone, "should receive [DONE] sentinel")
	assert.GreaterOrEqual(t, len(events), 2, "should receive multiple chunks")
}

func TestIntegration_ChatCompletions_ModelNotFound(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	apiKey := env.createAPIKey(t, "test-key", nil)

	rec := doRequest(env, "POST", "/v1/chat/completions", map[string]any{
		"model": "nonexistent-model",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIntegration_ChatCompletions_ModelAccessDenied(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	env.seedModel(t, "restricted-model", "meta-llama/Llama-3")
	// Key that can only access "other-model".
	apiKey := env.createAPIKey(t, "restricted-key", []string{"other-model"})

	rec := doRequest(env, "POST", "/v1/chat/completions", map[string]any{
		"model": "restricted-model",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestIntegration_RateLimit_RPMExceeded(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	env.seedModel(t, "rl-model", "meta-llama/Llama-3")
	apiKey := env.createAPIKey(t, "rl-key", nil)

	headers := map[string]string{"Authorization": "Bearer " + apiKey}
	reqBody := map[string]any{
		"model": "rl-model",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}

	// The default RPM is 60. If we send 61 requests, the last should be rate-limited.
	// For efficiency, use a very low RPM by updating the key.
	ctx := context.Background()
	keys, _ := env.authStore.ListKeys(ctx)
	rpm := 2
	for _, k := range keys {
		if k.Name == "rl-key" {
			k.RPMLimit = &rpm
			env.authStore.UpdateKey(ctx, &k)
			break
		}
	}

	// First 2 should succeed.
	for i := 0; i < 2; i++ {
		rec := doRequest(env, "POST", "/v1/chat/completions", reqBody, headers)
		assert.Equal(t, http.StatusOK, rec.Code, "request %d should succeed", i+1)
	}

	// Third should be rate limited.
	rec := doRequest(env, "POST", "/v1/chat/completions", reqBody, headers)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestIntegration_RequestID_Generated(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	rec := doRequest(env, "GET", "/health", nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
}

func TestIntegration_RequestID_Propagated(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	customID := "my-custom-request-id"
	rec := doRequest(env, "GET", "/health", nil, map[string]string{
		"X-Request-ID": customID,
	})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, customID, rec.Header().Get("X-Request-ID"))
}

// ---------------------------------------------------------------------------
// Admin API tests
// ---------------------------------------------------------------------------

func TestIntegration_Admin_RequiresAuth(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	// No admin key.
	rec := doRequest(env, "GET", "/admin/v1/models", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Wrong admin key.
	rec = doRequest(env, "GET", "/admin/v1/models", nil, map[string]string{
		"X-Admin-Key": "wrong-key",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestIntegration_Admin_ModelCRUD(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	adminHeaders := map[string]string{"X-Admin-Key": testAdminKey}

	// Create model.
	rec := doRequest(env, "POST", "/admin/v1/models", map[string]any{
		"name":     "admin-test-model",
		"model_id": "meta-llama/Llama-3",
	}, adminHeaders)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var model map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&model))
	modelID := model["id"].(string)
	assert.Equal(t, "admin-test-model", model["name"])

	// List models.
	rec = doRequest(env, "GET", "/admin/v1/models", nil, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Get model.
	rec = doRequest(env, "GET", "/admin/v1/models/"+modelID, nil, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Update model.
	newName := "updated-model"
	rec = doRequest(env, "PUT", "/admin/v1/models/"+modelID, map[string]any{
		"name": &newName,
	}, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updated map[string]any
	json.NewDecoder(rec.Body).Decode(&updated)
	assert.Equal(t, "updated-model", updated["name"])

	// Delete model.
	rec = doRequest(env, "DELETE", "/admin/v1/models/"+modelID, nil, adminHeaders)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify deleted.
	rec = doRequest(env, "GET", "/admin/v1/models/"+modelID, nil, adminHeaders)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIntegration_Admin_BackendCRUD(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	adminHeaders := map[string]string{"X-Admin-Key": testAdminKey}

	// Create model first.
	rec := doRequest(env, "POST", "/admin/v1/models", map[string]any{
		"name":     "backend-test-model",
		"model_id": "meta-llama/Llama-3",
	}, adminHeaders)
	require.Equal(t, http.StatusCreated, rec.Code)
	var model map[string]any
	json.NewDecoder(rec.Body).Decode(&model)
	modelID := model["id"].(string)

	// Create backend.
	rec = doRequest(env, "POST", "/admin/v1/models/"+modelID+"/backends", map[string]any{
		"url":    env.vllm.URL,
		"weight": 1,
	}, adminHeaders)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var backend map[string]any
	json.NewDecoder(rec.Body).Decode(&backend)
	backendID := backend["id"].(string)

	// Update backend.
	newWeight := 5
	rec = doRequest(env, "PUT", "/admin/v1/models/"+modelID+"/backends/"+backendID, map[string]any{
		"weight": &newWeight,
	}, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Delete backend.
	rec = doRequest(env, "DELETE", "/admin/v1/models/"+modelID+"/backends/"+backendID, nil, adminHeaders)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestIntegration_Admin_KeyCRUD(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	adminHeaders := map[string]string{"X-Admin-Key": testAdminKey}

	// Create key.
	rec := doRequest(env, "POST", "/admin/v1/keys", map[string]any{
		"name": "e2e-test-key",
	}, adminHeaders)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var keyResp map[string]any
	json.NewDecoder(rec.Body).Decode(&keyResp)
	assert.NotEmpty(t, keyResp["key"], "plaintext key should be returned on create")
	apiKeyObj := keyResp["api_key"].(map[string]any)
	keyID := apiKeyObj["id"].(string)

	// List keys.
	rec = doRequest(env, "GET", "/admin/v1/keys", nil, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Get key.
	rec = doRequest(env, "GET", "/admin/v1/keys/"+keyID, nil, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Update key.
	rec = doRequest(env, "PUT", "/admin/v1/keys/"+keyID, map[string]any{
		"name": "renamed-key",
	}, adminHeaders)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Delete key.
	rec = doRequest(env, "DELETE", "/admin/v1/keys/"+keyID, nil, adminHeaders)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestIntegration_Failover_UnhealthyBackend(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	ctx := context.Background()

	// Create model with two backends: one dead, one alive.
	m := &registry.Model{Name: "failover-model", ModelID: "meta-llama/Llama-3", Active: true}
	require.NoError(t, env.registryStore.CreateModel(ctx, m))

	// Dead backend (port 1 — nothing listening).
	deadBackend := &registry.Backend{
		ModelID: m.ID,
		URL:     "http://127.0.0.1:1",
		Weight:  1,
		Active:  true,
	}
	require.NoError(t, env.registryStore.CreateBackend(ctx, deadBackend))
	// Mark it unhealthy in the DB (CreateBackend defaults to healthy=true).
	require.NoError(t, env.registryStore.UpdateBackendHealth(ctx, deadBackend.ID, false))

	// Alive backend.
	require.NoError(t, env.registryStore.CreateBackend(ctx, &registry.Backend{
		ModelID: m.ID,
		URL:     env.vllm.URL,
		Weight:  1,
		Active:  true,
	}))

	require.NoError(t, env.cache.Invalidate(ctx))
	time.Sleep(50 * time.Millisecond)

	apiKey := env.createAPIKey(t, "failover-key", nil)

	rec := doRequest(env, "POST", "/v1/chat/completions", map[string]any{
		"model": "failover-model",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})

	// Should succeed by routing to the healthy backend.
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "failover-model", body["model"])
}

func TestIntegration_Admin_HealthEndpoint(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	rec := doRequest(env, "GET", "/admin/v1/health", nil, map[string]string{
		"X-Admin-Key": testAdminKey,
	})
	assert.Equal(t, http.StatusOK, rec.Code)
}
