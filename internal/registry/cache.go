package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Redis key prefixes for the registry cache.
	cacheKeyModelByName = "registry:model:name:"   // -> JSON-serialized Model
	cacheKeyModelByID   = "registry:model:id:"     // -> JSON-serialized Model
	cacheKeyModelList   = "registry:models:active" // -> JSON-serialized []Model
	cacheKeyVersion     = "registry:version"       // -> monotonic counter, bumped on invalidation
)

// Cache provides a Redis-backed read-through cache over the registry Store.
// It serves reads from Redis and refreshes from PostgreSQL periodically or on
// explicit invalidation. Because Redis is shared across gateway instances,
// invalidation is immediately visible to all pods.
type Cache struct {
	store    Store
	rdb      *redis.Client
	logger   *slog.Logger
	interval time.Duration
	ttl      time.Duration

	stopCh chan struct{}
	done   chan struct{}
}

// NewCache creates a new Redis-backed registry cache.
//   - store: the underlying PostgreSQL store
//   - rdb: a connected Redis client
//   - refreshInterval: how often to proactively refresh the cache from PG
//   - ttl: TTL for each cached entry in Redis (should be > refreshInterval)
func NewCache(store Store, rdb *redis.Client, logger *slog.Logger, refreshInterval time.Duration) *Cache {
	// TTL is 2x the refresh interval so entries survive between refreshes
	// even if a single refresh is slow.
	ttl := refreshInterval * 2
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}

	return &Cache{
		store:    store,
		rdb:      rdb,
		logger:   logger,
		interval: refreshInterval,
		ttl:      ttl,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start loads the initial data into Redis and begins the periodic refresh loop.
func (c *Cache) Start(ctx context.Context) error {
	if err := c.refresh(ctx); err != nil {
		return fmt.Errorf("initial cache load: %w", err)
	}

	go c.loop()
	return nil
}

// Stop signals the refresh loop to stop and waits for it to finish.
func (c *Cache) Stop() {
	close(c.stopCh)
	<-c.done
}

// GetModelByName returns the model with the given alias name, or nil if not
// found or inactive. Reads from Redis; falls back to PG on cache miss.
func (c *Cache) GetModelByName(ctx context.Context, name string) *Model {
	key := cacheKeyModelByName + name
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil {
		var m Model
		if json.Unmarshal(data, &m) == nil {
			if !m.Active {
				return nil
			}
			return &m
		}
	}

	// Cache miss — read through from PG.
	m, err := c.store.GetModelByName(ctx, name)
	if err != nil {
		c.logger.Debug("cache miss, store lookup failed", "name", name, "error", err)
		return nil
	}
	c.setModel(ctx, m)
	if !m.Active {
		return nil
	}
	return m
}

// GetModelByID returns the model with the given UUID, or nil if not found.
// Reads from Redis; falls back to PG on cache miss.
func (c *Cache) GetModelByID(ctx context.Context, id string) *Model {
	key := cacheKeyModelByID + id
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil {
		var m Model
		if json.Unmarshal(data, &m) == nil {
			return &m
		}
	}

	m, err := c.store.GetModelByID(ctx, id)
	if err != nil {
		c.logger.Debug("cache miss, store lookup failed", "id", id, "error", err)
		return nil
	}
	c.setModel(ctx, m)
	return m
}

// ListModels returns all active models. Reads from Redis; falls back to PG.
func (c *Cache) ListModels(ctx context.Context) []Model {
	data, err := c.rdb.Get(ctx, cacheKeyModelList).Bytes()
	if err == nil {
		var models []Model
		if json.Unmarshal(data, &models) == nil {
			return models
		}
	}

	// Cache miss — load from PG.
	models, err := c.store.ListModels(ctx, true)
	if err != nil {
		c.logger.Error("cache miss, failed to list models from store", "error", err)
		return nil
	}
	c.setModelList(ctx, models)
	return models
}

// Invalidate bumps the cache version and forces a full reload from PG into
// Redis. Because all gateway instances read from the same Redis, the new data
// is immediately visible everywhere.
func (c *Cache) Invalidate(ctx context.Context) error {
	// Bump version to signal staleness to any concurrent readers.
	c.rdb.Incr(ctx, cacheKeyVersion)

	return c.refresh(ctx)
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

func (c *Cache) loop() {
	defer close(c.done)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := c.refresh(ctx); err != nil {
				c.logger.Error("failed to refresh registry cache", "error", err)
			}
			cancel()
		}
	}
}

func (c *Cache) refresh(ctx context.Context) error {
	// Load all models (including inactive) so individual lookups hit cache.
	allModels, err := c.store.ListModels(ctx, false)
	if err != nil {
		return fmt.Errorf("listing models: %w", err)
	}

	pipe := c.rdb.Pipeline()

	// Cache each model by name and by ID.
	for i := range allModels {
		m := &allModels[i]
		data, err := json.Marshal(m)
		if err != nil {
			c.logger.Error("failed to marshal model for cache", "model", m.Name, "error", err)
			continue
		}
		pipe.Set(ctx, cacheKeyModelByName+m.Name, data, c.ttl)
		pipe.Set(ctx, cacheKeyModelByID+m.ID, data, c.ttl)
	}

	// Cache the active-only list.
	activeModels := make([]Model, 0, len(allModels))
	for _, m := range allModels {
		if m.Active {
			activeModels = append(activeModels, m)
		}
	}
	listData, err := json.Marshal(activeModels)
	if err != nil {
		return fmt.Errorf("marshaling model list: %w", err)
	}
	pipe.Set(ctx, cacheKeyModelList, listData, c.ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("writing cache to Redis: %w", err)
	}

	c.logger.Debug("registry cache refreshed", "model_count", len(allModels), "active_count", len(activeModels))
	return nil
}

// setModel caches a single model in Redis (fire-and-forget).
func (c *Cache) setModel(ctx context.Context, m *Model) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, cacheKeyModelByName+m.Name, data, c.ttl)
	pipe.Set(ctx, cacheKeyModelByID+m.ID, data, c.ttl)
	pipe.Exec(ctx) //nolint:errcheck
}

// setModelList caches the active model list in Redis (fire-and-forget).
func (c *Cache) setModelList(ctx context.Context, models []Model) {
	data, err := json.Marshal(models)
	if err != nil {
		return
	}
	c.rdb.Set(ctx, cacheKeyModelList, data, c.ttl) //nolint:errcheck
}
