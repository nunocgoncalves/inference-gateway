package snapshot

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCache_refreshAndRead asserts the cache loads the snapshot on Start and
// serves reads from memory.
func TestCache_refreshAndRead(t *testing.T) {
	pool, connStr := setupTestDB(t)
	aliceID := seedFixture(t, pool)
	store := NewPGStore(pool)
	cache := NewCache(store, connStr, slog.Default(), time.Hour) // long poll; LISTEN drives updates
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, cache.Start(ctx))
	t.Cleanup(cache.Stop)

	// Freshly started -> snapshot is fresh (readiness gate).
	assert.True(t, cache.Fresh(time.Minute), "snapshot should be fresh after Start")

	// Catalog entry.
	e, ok := cache.CatalogEntry("qwen3-27b")
	require.True(t, ok)
	assert.Equal(t, "http://vllm", e.BackendURL)
	assert.Equal(t, "Qwen/Qwen3-27B", e.BackendModelID)

	// API key -> identity.
	id, ok := cache.IdentityByAPIKey("h1")
	require.True(t, ok)
	assert.Equal(t, aliceID, id)
	_, ok = cache.IdentityByAPIKey("nope")
	assert.False(t, ok)

	// Capabilities (broad-default wildcard).
	caps := cache.Capabilities(aliceID)
	require.Len(t, caps, 1)
	assert.Equal(t, "*", caps[0].Resource)

	// Rate limits; absent = unlimited.
	rl, ok := cache.RateLimits(aliceID)
	require.True(t, ok)
	assert.Equal(t, 60, rl.RPM)
	assert.Equal(t, 100000, rl.TPM)
	_, ok = cache.RateLimits("00000000-0000-0000-0000-000000000000")
	assert.False(t, ok, "no rate-limit policy -> unlimited")
}

// TestCache_listenPropagation asserts a DB change fires the NOTIFY channel and
// the cache refreshes promptly (without waiting on the poll interval).
func TestCache_listenPropagation(t *testing.T) {
	pool, connStr := setupTestDB(t)
	seedFixture(t, pool)
	store := NewPGStore(pool)
	cache := NewCache(store, connStr, slog.Default(), time.Hour) // poll disabled; LISTEN must drive this
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, cache.Start(ctx))
	t.Cleanup(cache.Stop)

	// Wait for LISTEN to be ready so the NOTIFY isn't missed (poll is disabled).
	select {
	case <-cache.ListenReady():
	case <-time.After(5 * time.Second):
		t.Fatal("LISTEN did not become ready")
	}

	// Initially only qwen3-27b exists.
	_, ok := cache.CatalogEntry("qwen3-27b-thinking")
	require.False(t, ok)

	// Insert a second alias on the same backend, then fire the catalog_changed
	// NOTIFY (the control-plane's trigger fires this in production; HOR-327 tests
	// the trigger. Here we test the cache's LISTEN->refresh, the gateway's concern).
	_, err := pool.Exec(ctx, `
		INSERT INTO catalog.models (key, namespace, model_id, backend_ref, available)
		VALUES ('default/thinking', 'default', 'qwen3-27b-thinking', 'qwen', true)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `SELECT pg_notify('catalog_changed', '{}')`)
	require.NoError(t, err)

	// The cache should pick it up via LISTEN (well within the poll interval).
	require.Eventually(t, func() bool {
		_, ok := cache.CatalogEntry("qwen3-27b-thinking")
		return ok
	}, 5*time.Second, 100*time.Millisecond, "cache should refresh on catalog_changed NOTIFY")
}
