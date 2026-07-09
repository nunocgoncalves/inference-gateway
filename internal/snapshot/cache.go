package snapshot

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cache is the gateway's in-memory snapshot of control-plane state (catalog,
// API keys, capabilities, rate limits), refreshed from the Store on start, on
// LISTEN/NOTIFY (catalog_changed, api_keys_changed, permissions_changed), and on
// a periodic poll (fallback). The request path reads from memory (no PG/Redis
// round-trip); Redis remains for the rate-limit windows. This honors HOR-243's
// "consumers own their cache + freshness via LISTEN/NOTIFY" contract.
type Cache struct {
	store    Store
	connStr  string
	logger   *slog.Logger
	interval time.Duration

	mu      sync.RWMutex
	catalog map[string]CatalogEntry
	apiKeys map[string]string
	caps    map[string][]Capability
	rl      map[string]IdentityRateLimits

	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	listenReady chan struct{}
	readyOnce   sync.Once
}

// NewCache wraps a Store. connStr is used for the dedicated LISTEN connection
// (pool connections aren't held for LISTEN).
func NewCache(store Store, connStr string, logger *slog.Logger, refreshInterval time.Duration) *Cache {
	return &Cache{store: store, connStr: connStr, logger: logger, interval: refreshInterval, listenReady: make(chan struct{})}
}

// Start loads the initial snapshot and starts the LISTEN + poll loops.
func (c *Cache) Start(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	if err := c.refresh(c.ctx); err != nil {
		c.cancel()
		return err
	}
	c.wg.Add(2)
	go c.listenLoop()
	go c.pollLoop()
	return nil
}

// Stop cancels the loops and waits for them to exit.
func (c *Cache) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}

// refresh reloads all four views from the Store and atomically swaps the maps.
func (c *Cache) refresh(ctx context.Context) error {
	entries, err := c.store.ListCatalog(ctx)
	if err != nil {
		return err
	}
	keys, err := c.store.AllAPIKeys(ctx)
	if err != nil {
		return err
	}
	caps, err := c.store.AllCapabilities(ctx)
	if err != nil {
		return err
	}
	rl, err := c.store.AllRateLimits(ctx)
	if err != nil {
		return err
	}

	cat := make(map[string]CatalogEntry, len(entries))
	for _, e := range entries {
		cat[e.ModelID] = e
	}
	kh := make(map[string]string, len(keys))
	for _, k := range keys {
		kh[k.KeyHash] = k.IdentityID
	}
	capMap := make(map[string][]Capability)
	for _, cp := range caps {
		capMap[cp.IdentityID] = append(capMap[cp.IdentityID], cp)
	}
	rlMap := make(map[string]IdentityRateLimits, len(rl))
	for _, r := range rl {
		rlMap[r.IdentityID] = r
	}

	c.mu.Lock()
	c.catalog, c.apiKeys, c.caps, c.rl = cat, kh, capMap, rlMap
	c.mu.Unlock()

	c.logger.Debug("snapshot refreshed",
		"catalog", len(cat), "api_keys", len(kh), "identities_caps", len(capMap), "rate_limits", len(rlMap))
	return nil
}

// listenLoop holds a dedicated connection that LISTENs on the three change
// channels and refreshes on any notification. Reconnects with backoff on error;
// the poll loop is the fallback if LISTEN is unavailable.
func (c *Cache) listenLoop() {
	defer c.wg.Done()
	const backoff = 5 * time.Second
	for {
		if c.ctx.Err() != nil {
			return
		}
		conn, err := pgx.Connect(c.ctx, c.connStr)
		if err != nil {
			c.logger.Error("snapshot listen: connect failed", "error", err)
			if !c.sleep(backoff) {
				return
			}
			continue
		}
		if !c.serveConn(conn, backoff) {
			return // ctx cancelled
		}
	}
}

// serveConn LISTENs on the change channels and refreshes on notifications.
// Returns false if the cache is stopping (ctx cancelled); true to reconnect.
func (c *Cache) serveConn(conn *pgx.Conn, backoff time.Duration) bool {
	defer func() { _ = conn.Close(c.ctx) }()
	channels := []string{"catalog_changed", "api_keys_changed", "permissions_changed"}
	for _, ch := range channels {
		if _, err := conn.Exec(c.ctx, "LISTEN "+ch); err != nil {
			c.logger.Error("snapshot listen: LISTEN failed", "channel", ch, "error", err)
			c.sleep(backoff)
			return true // reconnect
		}
	}
	c.logger.Info("snapshot LISTEN started", "channels", channels)
	c.readyOnce.Do(func() { close(c.listenReady) })
	for {
		n, err := conn.WaitForNotification(c.ctx)
		if err != nil {
			if c.ctx.Err() != nil {
				return false
			}
			c.logger.Error("snapshot listen: wait failed", "error", err)
			c.sleep(backoff)
			return true // reconnect
		}
		c.logger.Debug("snapshot change notification", "channel", n.Channel)
		if err := c.refresh(c.ctx); err != nil {
			c.logger.Error("snapshot refresh on notify failed", "error", err)
		}
	}
}

// pollLoop refreshes on a ticker as a fallback (in case LISTEN misses or is
// unavailable).
func (c *Cache) pollLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.refresh(c.ctx); err != nil {
				c.logger.Error("snapshot poll refresh failed", "error", err)
			}
		}
	}
}

// sleep returns false if the cache is stopping (ctx cancelled).
func (c *Cache) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- Request-path reads (memory; no PG/Redis round-trip) ---

// ListenReady returns a channel that is closed once the LISTEN connection is
// established. Callers that need to assert prompt NOTIFY propagation can wait
// on it before mutating the DB.
func (c *Cache) ListenReady() <-chan struct{} {
	return c.listenReady
}

// CatalogEntry returns the catalog entry for an alias.
func (c *Cache) CatalogEntry(modelID string) (CatalogEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.catalog[modelID]
	return e, ok
}

// IdentityByAPIKey resolves an API key hash to its identity_id.
func (c *Cache) IdentityByAPIKey(keyHash string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.apiKeys[keyHash]
	return id, ok
}

// Capabilities returns the capability rows granted to an identity (nil = none/denied).
func (c *Cache) Capabilities(identityID string) []Capability {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps[identityID]
}

// RateLimits returns the per-identity throughput limit; ok=false means unlimited.
func (c *Cache) RateLimits(identityID string) (IdentityRateLimits, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rl, ok := c.rl[identityID]
	return rl, ok
}
