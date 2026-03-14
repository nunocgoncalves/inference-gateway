package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
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

type testEnv struct {
	handler  *Handler
	router   *chi.Mux
	store    *registry.PGStore
	keyStore *auth.PGStore
	cache    *registry.Cache
	cleanup  func()
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool, dbCleanup := setupTestDB(t)
	rdb, redisCleanup := setupTestRedis(t)

	store := registry.NewPGStore(pool)
	keyStore := auth.NewPGStore(pool)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()

	cache := registry.NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))

	handler := NewHandler(store, keyStore, cache, logger)

	r := chi.NewRouter()
	handler.RegisterRoutes(r)
	handler.RegisterKeyRoutes(r)

	return &testEnv{
		handler:  handler,
		router:   r,
		store:    store,
		keyStore: keyStore,
		cache:    cache,
		cleanup: func() {
			cache.Stop()
			redisCleanup()
			dbCleanup()
		},
	}
}

func doRequest(r *chi.Mux, method, path string, body any) *httptest.ResponseRecorder {
	var reqBody *bytes.Buffer
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(data)
	} else {
		reqBody = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))
	return result
}

func decodeJSONArray(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var result []any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))
	return result
}

// ---------------------------------------------------------------------------
// Model CRUD tests
// ---------------------------------------------------------------------------

func TestCreateModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name:    "llama-70b",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "llama-70b", body["name"])
	assert.Equal(t, "meta-llama/Llama-3.1-70B-Instruct", body["model_id"])
	assert.NotEmpty(t, body["id"])
	assert.Equal(t, true, body["active"])
}

func TestCreateModel_WithConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	temp := 0.7
	maxTok := 4096
	enabled := true
	rec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name:    "configured-model",
		ModelID: "test/model",
		DefaultParams: &registry.DefaultParams{
			Temperature: &temp,
			MaxTokens:   &maxTok,
		},
		ReasoningConfig: &registry.ReasoningConfig{
			Enabled:         &enabled,
			ReasoningEffort: "medium",
		},
		Transforms: &registry.Transforms{
			SystemPromptPrefix: "Be helpful.",
		},
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	body := decodeJSON(t, rec)

	dp := body["default_params"].(map[string]any)
	assert.Equal(t, 0.7, dp["temperature"])
	assert.Equal(t, float64(4096), dp["max_tokens"])

	rc := body["reasoning_config"].(map[string]any)
	assert.Equal(t, true, rc["enabled"])
	assert.Equal(t, "medium", rc["reasoning_effort"])

	tf := body["transforms"].(map[string]any)
	assert.Equal(t, "Be helpful.", tf["system_prompt_prefix"])
}

func TestCreateModel_MissingName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		ModelID: "test/model",
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateModel_DuplicateName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "dupe", ModelID: "test/model",
	})
	rec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "dupe", ModelID: "test/model2",
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	body := decodeJSON(t, rec)
	errMsg := body["error"].(map[string]any)["message"].(string)
	assert.Contains(t, errMsg, "already exists")
}

func TestGetModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "get-test", ModelID: "test/model",
	})
	created := decodeJSON(t, createRec)
	id := created["id"].(string)

	rec := doRequest(env.router, "GET", "/models/"+id, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "get-test", body["name"])
}

func TestGetModel_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "GET", "/models/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	doRequest(env.router, "POST", "/models", CreateModelRequest{Name: "m1", ModelID: "test/1"})
	doRequest(env.router, "POST", "/models", CreateModelRequest{Name: "m2", ModelID: "test/2"})

	rec := doRequest(env.router, "GET", "/models", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	models := decodeJSONArray(t, rec)
	assert.Len(t, models, 2)
}

func TestUpdateModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "before", ModelID: "test/v1",
	})
	created := decodeJSON(t, createRec)
	id := created["id"].(string)

	newName := "after"
	newModelID := "test/v2"
	active := false
	rec := doRequest(env.router, "PUT", "/models/"+id, UpdateModelRequest{
		Name:    &newName,
		ModelID: &newModelID,
		Active:  &active,
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "after", body["name"])
	assert.Equal(t, "test/v2", body["model_id"])
	assert.Equal(t, false, body["active"])
}

func TestUpdateModel_PartialUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "partial", ModelID: "test/original",
	})
	created := decodeJSON(t, createRec)
	id := created["id"].(string)

	// Only update active, leave name and model_id unchanged.
	active := false
	rec := doRequest(env.router, "PUT", "/models/"+id, UpdateModelRequest{
		Active: &active,
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "partial", body["name"], "name should be unchanged")
	assert.Equal(t, "test/original", body["model_id"], "model_id should be unchanged")
	assert.Equal(t, false, body["active"])
}

func TestUpdateModel_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	name := "x"
	rec := doRequest(env.router, "PUT", "/models/00000000-0000-0000-0000-000000000000", UpdateModelRequest{
		Name: &name,
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "to-delete", ModelID: "test/model",
	})
	created := decodeJSON(t, createRec)
	id := created["id"].(string)

	rec := doRequest(env.router, "DELETE", "/models/"+id, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Confirm deleted.
	getRec := doRequest(env.router, "GET", "/models/"+id, nil)
	assert.Equal(t, http.StatusNotFound, getRec.Code)
}

func TestDeleteModel_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "DELETE", "/models/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Backend CRUD tests
// ---------------------------------------------------------------------------

func createTestModel(t *testing.T, env *testEnv) string {
	t.Helper()
	rec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "backend-test-model", ModelID: "test/model",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	return decodeJSON(t, rec)["id"].(string)
}

func TestCreateBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)

	rec := doRequest(env.router, "POST", "/models/"+modelID+"/backends", CreateBackendRequest{
		URL:    "http://vllm-1:8000",
		Weight: 3,
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "http://vllm-1:8000", body["url"])
	assert.Equal(t, float64(3), body["weight"])
	assert.Equal(t, true, body["active"])
	assert.NotEmpty(t, body["id"])
}

func TestCreateBackend_InvalidWeight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)

	rec := doRequest(env.router, "POST", "/models/"+modelID+"/backends", CreateBackendRequest{
		URL:    "http://vllm:8000",
		Weight: 0,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateBackend_MissingURL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)

	rec := doRequest(env.router, "POST", "/models/"+modelID+"/backends", CreateBackendRequest{
		Weight: 1,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateBackend_ModelNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "POST", "/models/00000000-0000-0000-0000-000000000000/backends", CreateBackendRequest{
		URL: "http://vllm:8000", Weight: 1,
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUpdateBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)
	createRec := doRequest(env.router, "POST", "/models/"+modelID+"/backends", CreateBackendRequest{
		URL: "http://vllm:8000", Weight: 1,
	})
	backendID := decodeJSON(t, createRec)["id"].(string)

	newWeight := 5
	active := false
	rec := doRequest(env.router, "PUT", "/models/"+modelID+"/backends/"+backendID, UpdateBackendRequest{
		Weight: &newWeight,
		Active: &active,
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, float64(5), body["weight"])
	assert.Equal(t, false, body["active"])
}

func TestUpdateBackend_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)

	w := 1
	rec := doRequest(env.router, "PUT", "/models/"+modelID+"/backends/00000000-0000-0000-0000-000000000000", UpdateBackendRequest{
		Weight: &w,
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)
	createRec := doRequest(env.router, "POST", "/models/"+modelID+"/backends", CreateBackendRequest{
		URL: "http://vllm:8000", Weight: 1,
	})
	backendID := decodeJSON(t, createRec)["id"].(string)

	rec := doRequest(env.router, "DELETE", "/models/"+modelID+"/backends/"+backendID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDeleteBackend_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	modelID := createTestModel(t, env)

	rec := doRequest(env.router, "DELETE", "/models/"+modelID+"/backends/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Cache invalidation test
// ---------------------------------------------------------------------------

func TestCacheInvalidatedOnCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	ctx := context.Background()

	// Cache should be empty initially.
	assert.Len(t, env.cache.ListModels(ctx), 0)

	// Create model via admin API.
	doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "cache-test", ModelID: "test/model",
	})

	// Cache should now have the model (invalidation happened).
	models := env.cache.ListModels(ctx)
	assert.Len(t, models, 1)
	assert.Equal(t, "cache-test", models[0].Name)
}

func TestCacheInvalidatedOnDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	ctx := context.Background()

	createRec := doRequest(env.router, "POST", "/models", CreateModelRequest{
		Name: "del-cache", ModelID: "test/model",
	})
	id := decodeJSON(t, createRec)["id"].(string)
	assert.Len(t, env.cache.ListModels(ctx), 1)

	doRequest(env.router, "DELETE", "/models/"+id, nil)
	assert.Len(t, env.cache.ListModels(ctx), 0)
}

// ---------------------------------------------------------------------------
// API Key CRUD tests
// ---------------------------------------------------------------------------

func TestCreateKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{
		Name: "test-key",
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	body := decodeJSON(t, rec)

	// Plaintext key should be returned.
	key, ok := body["key"].(string)
	assert.True(t, ok)
	assert.Contains(t, key, "ml-")
	assert.True(t, len(key) > 20)

	// api_key metadata should be present.
	apiKey := body["api_key"].(map[string]any)
	assert.Equal(t, "test-key", apiKey["name"])
	assert.NotEmpty(t, apiKey["id"])
	assert.NotEmpty(t, apiKey["key_prefix"])
	assert.Equal(t, true, apiKey["active"])
}

func TestCreateKey_WithLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rpm := 200
	tpm := 500000
	rec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{
		Name:          "limited-key",
		RPMLimit:      &rpm,
		TPMLimit:      &tpm,
		AllowedModels: []string{"model-a", "model-b"},
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	body := decodeJSON(t, rec)
	apiKey := body["api_key"].(map[string]any)
	assert.Equal(t, float64(200), apiKey["rpm_limit"])
	assert.Equal(t, float64(500000), apiKey["tpm_limit"])
	models := apiKey["allowed_models"].([]any)
	assert.Len(t, models, 2)
}

func TestCreateKey_MissingName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "get-test"})
	created := decodeJSON(t, createRec)
	apiKey := created["api_key"].(map[string]any)
	id := apiKey["id"].(string)

	rec := doRequest(env.router, "GET", "/keys/"+id, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "get-test", body["name"])
	assert.NotEmpty(t, body["key_prefix"])

	// Key hash should NOT be in the response.
	_, hasHash := body["key_hash"]
	assert.False(t, hasHash, "key_hash should never be exposed")
}

func TestGetKey_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "GET", "/keys/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "key-1"})
	doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "key-2"})
	doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "key-3"})

	rec := doRequest(env.router, "GET", "/keys", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	keys := decodeJSONArray(t, rec)
	assert.Len(t, keys, 3)

	// Verify no key has key_hash exposed.
	for _, k := range keys {
		kMap := k.(map[string]any)
		_, hasHash := kMap["key_hash"]
		assert.False(t, hasHash)
	}
}

func TestUpdateKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "before-update"})
	apiKey := decodeJSON(t, createRec)["api_key"].(map[string]any)
	id := apiKey["id"].(string)

	newName := "after-update"
	newRPM := 999
	rec := doRequest(env.router, "PUT", "/keys/"+id, UpdateKeyRequest{
		Name:          &newName,
		RPMLimit:      &newRPM,
		AllowedModels: []string{"only-this-model"},
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "after-update", body["name"])
	assert.Equal(t, float64(999), body["rpm_limit"])
	models := body["allowed_models"].([]any)
	assert.Len(t, models, 1)
	assert.Equal(t, "only-this-model", models[0])
}

func TestUpdateKey_PartialUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rpm := 100
	createRec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{
		Name:     "partial-key",
		RPMLimit: &rpm,
	})
	apiKey := decodeJSON(t, createRec)["api_key"].(map[string]any)
	id := apiKey["id"].(string)

	// Only update active, leave name and limits unchanged.
	active := false
	rec := doRequest(env.router, "PUT", "/keys/"+id, UpdateKeyRequest{
		Active: &active,
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	body := decodeJSON(t, rec)
	assert.Equal(t, "partial-key", body["name"], "name should be unchanged")
	assert.Equal(t, float64(100), body["rpm_limit"], "rpm_limit should be unchanged")
	assert.Equal(t, false, body["active"])
}

func TestUpdateKey_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	name := "x"
	rec := doRequest(env.router, "PUT", "/keys/00000000-0000-0000-0000-000000000000", UpdateKeyRequest{
		Name: &name,
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	createRec := doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "to-revoke"})
	created := decodeJSON(t, createRec)
	plaintext := created["key"].(string)
	apiKey := created["api_key"].(map[string]any)
	id := apiKey["id"].(string)

	// Delete (soft revoke).
	rec := doRequest(env.router, "DELETE", "/keys/"+id, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Key should still be gettable but inactive.
	getRec := doRequest(env.router, "GET", "/keys/"+id, nil)
	assert.Equal(t, http.StatusOK, getRec.Code)
	body := decodeJSON(t, getRec)
	assert.Equal(t, false, body["active"])

	// Key should no longer validate.
	ctx := context.Background()
	validated, err := env.keyStore.ValidateKey(ctx, auth.HashKey(plaintext))
	require.NoError(t, err)
	assert.Nil(t, validated, "revoked key should not validate")
}

func TestDeleteKey_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	rec := doRequest(env.router, "DELETE", "/keys/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCreateKey_KeyIsUnique(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)
	defer env.cleanup()

	// Create two keys and verify they have different plaintexts.
	rec1 := doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "key-a"})
	rec2 := doRequest(env.router, "POST", "/keys", CreateKeyRequest{Name: "key-b"})

	key1 := decodeJSON(t, rec1)["key"].(string)
	key2 := decodeJSON(t, rec2)["key"].(string)
	assert.NotEqual(t, key1, key2)
}
