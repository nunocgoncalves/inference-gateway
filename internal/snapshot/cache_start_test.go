package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is a Store fake that fails ListCatalog failRemaining times, then
// succeeds — simulating the control-plane schemas appearing after a delay.
type fakeStore struct {
	failRemaining int32
	catalog       []CatalogEntry
}

func (s *fakeStore) ListCatalog(ctx context.Context) ([]CatalogEntry, error) {
	if atomic.LoadInt32(&s.failRemaining) > 0 {
		atomic.AddInt32(&s.failRemaining, -1)
		return nil, errors.New("schemas not ready")
	}
	return s.catalog, nil
}
func (s *fakeStore) AllAPIKeys(ctx context.Context) ([]APIKey, error)                { return nil, nil }
func (s *fakeStore) AllCapabilities(ctx context.Context) ([]Capability, error)       { return nil, nil }
func (s *fakeStore) AllRateLimits(ctx context.Context) ([]IdentityRateLimits, error) { return nil, nil }

func TestCache_StartRetriesUntilSchemasExist(t *testing.T) {
	store := &fakeStore{failRemaining: 2, catalog: []CatalogEntry{{ModelID: "qwen3-27b", Available: true}}}
	cache := NewCache(store, "", slog.Default(), time.Hour)
	cache.retryInterval = 10 * time.Millisecond // fast retry for the test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, cache.Start(ctx))
	t.Cleanup(cache.Stop)

	// After the retries, the snapshot is loaded.
	e, ok := cache.CatalogEntry("qwen3-27b")
	require.True(t, ok)
	assert.True(t, e.Available)
}

func TestCache_StartCancelledWhileRetrying(t *testing.T) {
	store := &fakeStore{failRemaining: 1000} // always fails
	cache := NewCache(store, "", slog.Default(), time.Hour)
	cache.retryInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := cache.Start(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
