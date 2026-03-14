package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

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

// ---------------------------------------------------------------------------
// RPM tests
// ---------------------------------------------------------------------------

func TestRedisLimiter_RPM_UnderLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// 10 RPM limit, send 5 requests.
	for i := 0; i < 5; i++ {
		result, err := limiter.CheckRPM(ctx, "key-rpm-under", 10)
		require.NoError(t, err)
		assert.True(t, result.Allowed, "request %d should be allowed", i)
		assert.Equal(t, 10, result.Limit)
	}

	// Check remaining.
	result, err := limiter.CheckRPM(ctx, "key-rpm-under", 10)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 4, result.Remaining) // 10 - 6 = 4
}

func TestRedisLimiter_RPM_AtLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// 3 RPM limit, send 3 requests (should all pass).
	for i := 0; i < 3; i++ {
		result, err := limiter.CheckRPM(ctx, "key-rpm-at", 3)
		require.NoError(t, err)
		assert.True(t, result.Allowed, "request %d should be allowed", i)
	}

	// 4th request should be blocked.
	result, err := limiter.CheckRPM(ctx, "key-rpm-at", 3)
	require.NoError(t, err)
	assert.False(t, result.Allowed, "4th request should be blocked")
	assert.Equal(t, 0, result.Remaining)
}

func TestRedisLimiter_RPM_DifferentKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// Exhaust key-a.
	for i := 0; i < 2; i++ {
		limiter.CheckRPM(ctx, "key-a", 2)
	}
	resultA, _ := limiter.CheckRPM(ctx, "key-a", 2)
	assert.False(t, resultA.Allowed)

	// key-b should still be allowed.
	resultB, err := limiter.CheckRPM(ctx, "key-b", 2)
	require.NoError(t, err)
	assert.True(t, resultB.Allowed)
}

// ---------------------------------------------------------------------------
// TPM tests
// ---------------------------------------------------------------------------

func TestRedisLimiter_TPM_CheckAndIncrement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// Check TPM — should be at 0, under 1000 limit.
	result, err := limiter.CheckTPM(ctx, "key-tpm", 1000)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 1000, result.Remaining)

	// Increment by 500 tokens.
	require.NoError(t, limiter.IncrementTPM(ctx, "key-tpm", 500))

	// Check again — should show 500 remaining.
	result, err = limiter.CheckTPM(ctx, "key-tpm", 1000)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 500, result.Remaining)

	// Increment by 600 more (total 1100 > 1000 limit).
	require.NoError(t, limiter.IncrementTPM(ctx, "key-tpm", 600))

	// Should now be over limit.
	result, err = limiter.CheckTPM(ctx, "key-tpm", 1000)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, 0, result.Remaining)
}

func TestRedisLimiter_TPM_ZeroTokensNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// Zero tokens should be a no-op.
	require.NoError(t, limiter.IncrementTPM(ctx, "key-noop", 0))
	require.NoError(t, limiter.IncrementTPM(ctx, "key-noop", -5))

	result, err := limiter.CheckTPM(ctx, "key-noop", 100)
	require.NoError(t, err)
	assert.Equal(t, 100, result.Remaining)
}

func TestRedisLimiter_TPM_MultipleIncrements(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// Multiple small increments.
	for i := 0; i < 10; i++ {
		require.NoError(t, limiter.IncrementTPM(ctx, "key-multi", 100))
	}

	// Should be at 1000 total.
	result, err := limiter.CheckTPM(ctx, "key-multi", 1000)
	require.NoError(t, err)
	assert.False(t, result.Allowed, "should be at limit")
	assert.Equal(t, 0, result.Remaining)
}

// ---------------------------------------------------------------------------
// TPMBatcher tests
// ---------------------------------------------------------------------------

func TestTPMBatcher_BatchesAndFlushes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	batcher := NewTPMBatcher(limiter, "key-batcher", 100*time.Millisecond)
	batcher.Start()

	// Add tokens in small increments (simulating streaming).
	for i := 0; i < 20; i++ {
		batcher.Add(10) // 20 * 10 = 200 tokens total
	}

	// Wait for at least one flush.
	time.Sleep(250 * time.Millisecond)

	// Stop and do final flush.
	batcher.Stop(ctx)

	// Check that all 200 tokens were counted.
	result, err := limiter.CheckTPM(ctx, "key-batcher", 1000)
	require.NoError(t, err)
	assert.Equal(t, 800, result.Remaining, "should have counted 200 tokens")
}

func TestTPMBatcher_FinalFlushOnStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	// Long interval — will only flush on Stop.
	batcher := NewTPMBatcher(limiter, "key-final", 10*time.Minute)
	batcher.Start()

	batcher.Add(300)

	// Stop immediately — should flush the 300 tokens.
	batcher.Stop(ctx)

	result, err := limiter.CheckTPM(ctx, "key-final", 1000)
	require.NoError(t, err)
	assert.Equal(t, 700, result.Remaining, "final flush should have counted 300 tokens")
}

// ---------------------------------------------------------------------------
// Sliding window expiry test
// ---------------------------------------------------------------------------

func TestRedisLimiter_RPM_WindowExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Use a custom approach: we can't easily wait 60s in tests.
	// Instead verify that the sorted set has the right TTL.
	rdb, cleanup := setupTestRedis(t)
	defer cleanup()

	limiter := NewRedisLimiter(rdb)
	ctx := context.Background()

	limiter.CheckRPM(ctx, "key-ttl", 100)

	// The key should exist with a TTL.
	ttl, err := rdb.TTL(ctx, rpmKeyPrefix+"key-ttl").Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0, "key should have a TTL")
	assert.True(t, ttl <= 61*time.Second, "TTL should be ~61s")
}
