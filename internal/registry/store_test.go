package registry

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// setupTestDB starts a Postgres container, runs migrations, and returns a
// connection pool plus cleanup function.
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

	// Run migrations.
	require.NoError(t, database.MigrateUp(connStr))

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)

	cleanup := func() {
		pool.Close()
		require.NoError(t, pgContainer.Terminate(ctx))
	}
	return pool, cleanup
}

// setupTestRedis starts a Redis container and returns a connected client.
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

func ptrFloat64(v float64) *float64 { return &v }
func ptrInt(v int) *int             { return &v }
func ptrBool(v bool) *bool          { return &v }

// ---------------------------------------------------------------------------
// Store tests
// ---------------------------------------------------------------------------

func TestPGStore_CreateAndGetModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{
		Name:    "llama-70b",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		Active:  true,
		DefaultParams: DefaultParams{
			Temperature: ptrFloat64(0.7),
			TopP:        ptrFloat64(0.9),
			MaxTokens:   ptrInt(4096),
		},
		ReasoningConfig: ReasoningConfig{
			EnableThinking: ptrBool(true),
		},
		Transforms: Transforms{
			SystemPromptPrefix: "You are a helpful assistant.",
			RewriteModelName:   true,
		},
		RateLimits: ModelRateLimits{
			RPM: ptrInt(100),
			TPM: ptrInt(50000),
		},
	}

	// Create.
	err := store.CreateModel(ctx, m)
	require.NoError(t, err)
	assert.NotEmpty(t, m.ID)
	assert.False(t, m.CreatedAt.IsZero())

	// Get by name.
	got, err := store.GetModelByName(ctx, "llama-70b")
	require.NoError(t, err)
	assert.Equal(t, m.ID, got.ID)
	assert.Equal(t, "meta-llama/Llama-3.1-70B-Instruct", got.ModelID)
	assert.Equal(t, 0.7, *got.DefaultParams.Temperature)
	assert.Equal(t, 4096, *got.DefaultParams.MaxTokens)
	assert.Equal(t, true, *got.ReasoningConfig.EnableThinking)
	assert.Equal(t, "You are a helpful assistant.", got.Transforms.SystemPromptPrefix)
	assert.Equal(t, 100, *got.RateLimits.RPM)

	// Get by ID.
	got2, err := store.GetModelByID(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "llama-70b", got2.Name)
}

func TestPGStore_ReasoningConfigThinkingDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{
		Name:    "llama-fast",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		Active:  true,
		ReasoningConfig: ReasoningConfig{
			EnableThinking: ptrBool(false),
		},
	}
	require.NoError(t, store.CreateModel(ctx, m))

	got, err := store.GetModelByName(ctx, "llama-fast")
	require.NoError(t, err)
	require.NotNil(t, got.ReasoningConfig.EnableThinking)
	assert.False(t, *got.ReasoningConfig.EnableThinking)
}

func TestPGStore_ReasoningConfigPassthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{
		Name:    "llama-passthrough",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		Active:  true,
		// ReasoningConfig left as zero value — Enabled is nil (passthrough).
	}
	require.NoError(t, store.CreateModel(ctx, m))

	got, err := store.GetModelByName(ctx, "llama-passthrough")
	require.NoError(t, err)
	assert.Nil(t, got.ReasoningConfig.EnableThinking, "nil EnableThinking means passthrough")
}

func TestPGStore_UpdateModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{
		Name:    "update-test",
		ModelID: "test/model-v1",
		Active:  true,
	}
	require.NoError(t, store.CreateModel(ctx, m))

	// Update.
	m.ModelID = "test/model-v2"
	m.DefaultParams.Temperature = ptrFloat64(0.5)
	m.Active = false
	require.NoError(t, store.UpdateModel(ctx, m))

	got, err := store.GetModelByID(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "test/model-v2", got.ModelID)
	assert.Equal(t, 0.5, *got.DefaultParams.Temperature)
	assert.False(t, got.Active)
	assert.True(t, got.UpdatedAt.After(got.CreatedAt))
}

func TestPGStore_DeleteModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{Name: "delete-me", ModelID: "test/model", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))

	require.NoError(t, store.DeleteModel(ctx, m.ID))

	_, err := store.GetModelByID(ctx, m.ID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestPGStore_DeleteModelNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	err := store.DeleteModel(ctx, "00000000-0000-0000-0000-000000000000")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestPGStore_ListModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	require.NoError(t, store.CreateModel(ctx, &Model{Name: "active-1", ModelID: "m1", Active: true}))
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "active-2", ModelID: "m2", Active: true}))
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "inactive", ModelID: "m3", Active: false}))

	// All models.
	all, err := store.ListModels(ctx, false)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// Active only.
	active, err := store.ListModels(ctx, true)
	require.NoError(t, err)
	assert.Len(t, active, 2)
}

// ---------------------------------------------------------------------------
// Backend tests
// ---------------------------------------------------------------------------

func TestPGStore_BackendCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{Name: "backend-test", ModelID: "test/model", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))

	// Create backends.
	b1 := &Backend{ModelID: m.ID, URL: "http://vllm-1:8000", Weight: 3, Active: true}
	b2 := &Backend{ModelID: m.ID, URL: "http://vllm-2:8000", Weight: 1, Active: true}
	require.NoError(t, store.CreateBackend(ctx, b1))
	require.NoError(t, store.CreateBackend(ctx, b2))
	assert.NotEmpty(t, b1.ID)
	assert.True(t, b1.Healthy) // default

	// List.
	backends, err := store.ListBackends(ctx, m.ID)
	require.NoError(t, err)
	assert.Len(t, backends, 2)

	// Update.
	b1.Weight = 5
	require.NoError(t, store.UpdateBackend(ctx, b1))
	backends, _ = store.ListBackends(ctx, m.ID)
	for _, b := range backends {
		if b.ID == b1.ID {
			assert.Equal(t, 5, b.Weight)
		}
	}

	// Update health.
	require.NoError(t, store.UpdateBackendHealth(ctx, b1.ID, false))
	backends, _ = store.ListBackends(ctx, m.ID)
	for _, b := range backends {
		if b.ID == b1.ID {
			assert.False(t, b.Healthy)
			assert.NotNil(t, b.LastHealthCheck)
		}
	}

	// Delete.
	require.NoError(t, store.DeleteBackend(ctx, b2.ID))
	backends, _ = store.ListBackends(ctx, m.ID)
	assert.Len(t, backends, 1)
}

func TestPGStore_BackendCascadeDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{Name: "cascade-test", ModelID: "test/model", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))
	require.NoError(t, store.CreateBackend(ctx, &Backend{ModelID: m.ID, URL: "http://vllm:8000", Weight: 1, Active: true}))

	// Deleting the model should cascade-delete the backend.
	require.NoError(t, store.DeleteModel(ctx, m.ID))

	backends, err := store.ListBackends(ctx, m.ID)
	require.NoError(t, err)
	assert.Len(t, backends, 0)
}

func TestPGStore_ModelWithBackendsLoaded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	m := &Model{Name: "with-backends", ModelID: "test/model", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))
	require.NoError(t, store.CreateBackend(ctx, &Backend{ModelID: m.ID, URL: "http://vllm-1:8000", Weight: 1, Active: true}))
	require.NoError(t, store.CreateBackend(ctx, &Backend{ModelID: m.ID, URL: "http://vllm-2:8000", Weight: 2, Active: true}))

	// GetModelByName should include backends.
	got, err := store.GetModelByName(ctx, "with-backends")
	require.NoError(t, err)
	assert.Len(t, got.Backends, 2)

	// ListModels should include backends.
	all, err := store.ListModels(ctx, false)
	require.NoError(t, err)
	for _, model := range all {
		if model.Name == "with-backends" {
			assert.Len(t, model.Backends, 2)
		}
	}
}

// ---------------------------------------------------------------------------
// Cache tests (Redis-backed)
// ---------------------------------------------------------------------------

func TestCache_GetModelByName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Seed data.
	m := &Model{
		Name:    "cached-model",
		ModelID: "test/cached",
		Active:  true,
		DefaultParams: DefaultParams{
			Temperature: ptrFloat64(0.8),
		},
	}
	require.NoError(t, store.CreateModel(ctx, m))
	require.NoError(t, store.CreateBackend(ctx, &Backend{ModelID: m.ID, URL: "http://vllm:8000", Weight: 1, Active: true}))

	cache := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	// Read from cache.
	got := cache.GetModelByName(ctx, "cached-model")
	require.NotNil(t, got)
	assert.Equal(t, "test/cached", got.ModelID)
	assert.Equal(t, 0.8, *got.DefaultParams.Temperature)
	assert.Len(t, got.Backends, 1)

	// Non-existent model.
	assert.Nil(t, cache.GetModelByName(ctx, "nonexistent"))
}

func TestCache_GetModelByID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	m := &Model{Name: "by-id-test", ModelID: "test/byid", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))

	cache := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	got := cache.GetModelByID(ctx, m.ID)
	require.NotNil(t, got)
	assert.Equal(t, "by-id-test", got.Name)
}

func TestCache_ListModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	require.NoError(t, store.CreateModel(ctx, &Model{Name: "list-a", ModelID: "m1", Active: true}))
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "list-b", ModelID: "m2", Active: true}))
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "list-inactive", ModelID: "m3", Active: false}))

	cache := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	// ListModels returns only active models.
	models := cache.ListModels(ctx)
	assert.Len(t, models, 2)
}

func TestCache_InactiveModelNotReturned(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	require.NoError(t, store.CreateModel(ctx, &Model{Name: "deactivated", ModelID: "m1", Active: false}))

	cache := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	// GetModelByName should not return inactive models.
	assert.Nil(t, cache.GetModelByName(ctx, "deactivated"))
}

func TestCache_Invalidate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cache := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	// Initially empty.
	assert.Len(t, cache.ListModels(ctx), 0)

	// Insert directly into the store (simulating admin API).
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "new-model", ModelID: "m1", Active: true}))

	// Cache still shows 0 (stale Redis).
	assert.Len(t, cache.ListModels(ctx), 0)

	// Manual invalidation forces reload from PG into Redis.
	require.NoError(t, cache.Invalidate(ctx))
	models := cache.ListModels(ctx)
	assert.Len(t, models, 1)
	assert.NotNil(t, cache.GetModelByName(ctx, "new-model"))
}

func TestCache_PeriodicRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Very short refresh interval for testing.
	cache := NewCache(store, rdb, logger, 200*time.Millisecond)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	assert.Len(t, cache.ListModels(ctx), 0)

	// Insert a model.
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "periodic-test", ModelID: "m1", Active: true}))

	// Wait for periodic refresh to pick it up.
	require.Eventually(t, func() bool {
		return cache.GetModelByName(ctx, "periodic-test") != nil
	}, 2*time.Second, 100*time.Millisecond, "cache should pick up new model via periodic refresh")
}

func TestCache_ReadThroughOnMiss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Start with empty cache, don't pre-load.
	cache := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache.Start(ctx))
	defer cache.Stop()

	// Insert a model after cache start.
	m := &Model{Name: "read-through", ModelID: "test/rt", Active: true}
	require.NoError(t, store.CreateModel(ctx, m))

	// Flush Redis to simulate eviction.
	rdb.FlushDB(ctx)

	// GetModelByName should fall back to PG and cache the result.
	got := cache.GetModelByName(ctx, "read-through")
	require.NotNil(t, got, "should read through to PG on cache miss")
	assert.Equal(t, "test/rt", got.ModelID)

	// Now it should be in Redis.
	got2 := cache.GetModelByName(ctx, "read-through")
	require.NotNil(t, got2)
	assert.Equal(t, "test/rt", got2.ModelID)
}

func TestCache_SharedAcrossInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, dbCleanup := setupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := setupTestRedis(t)
	defer redisCleanup()

	store := NewPGStore(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Simulate two gateway instances sharing the same Redis.
	cache1 := NewCache(store, rdb, logger, 1*time.Hour)
	cache2 := NewCache(store, rdb, logger, 1*time.Hour)
	require.NoError(t, cache1.Start(ctx))
	defer cache1.Stop()
	require.NoError(t, cache2.Start(ctx))
	defer cache2.Stop()

	// Add a model and invalidate via cache1.
	require.NoError(t, store.CreateModel(ctx, &Model{Name: "shared-model", ModelID: "m1", Active: true}))
	require.NoError(t, cache1.Invalidate(ctx))

	// cache2 should immediately see it because they share Redis.
	got := cache2.GetModelByName(ctx, "shared-model")
	require.NotNil(t, got, "cache2 should see model after cache1 invalidation (shared Redis)")
	assert.Equal(t, "m1", got.ModelID)
}
