package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

// TestServer_EndToEnd is the gateway-level E2E: a control-plane contract
// fixture (PG) + Redis + an httptest vLLM backend, wired through the real
// snapshot cache + proxy + middleware. Proves: API key -> identity, capability
// check, alias routing, model-field rewrite, per-identity rate limits, and the
// /readyz + /admin/v1/snapshot endpoints. Requires Docker.
func TestServer_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	logger := slog.Default()

	// --- Postgres with the control-plane contract fixture ---
	pgC, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("gw"), postgres.WithUsername("t"), postgres.WithPassword("t"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(30*time.Second)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	pgConn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, pgConn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, snapshot.FixtureSchema)
	require.NoError(t, err)

	// --- httptest vLLM backend (records the model field it receives) ---
	var backendGotModel string
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		backendGotModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + backendGotModel + `","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`))
	}))
	t.Cleanup(vllm.Close)

	// --- Seed: healthy backend + available alias (rewrite on) + identity + API key + rate-limit policy ---
	_, err = pool.Exec(ctx, `
		INSERT INTO catalog.backends (key, name, namespace, kind, model, service_url, deployed, healthy)
		VALUES ('default/qwen', 'qwen', 'default', 'vLLM', 'Qwen/Qwen3-27B', $1, true, true)`, vllm.URL)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO catalog.models (key, namespace, model_id, backend_ref, transforms, available)
		VALUES ('default/qwen3-27b', 'default', 'qwen3-27b', 'qwen', '{"rewrite_model_name":true}'::jsonb, true)`)
	require.NoError(t, err)
	var aliceID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind) VALUES ('default/alice', 'user') RETURNING id`).Scan(&aliceID))
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, scope) VALUES ($1, $2, 'gateway')`,
		aliceID, middleware.HashKey("test-key"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO permissions.policies (subject_kind, subject_key, rate_limits)
		VALUES ('user', 'default/alice', '{"rpm":60,"tpm":100000}'::jsonb)`)
	require.NoError(t, err)

	// --- Redis ---
	rC, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rC.Terminate(ctx) })
	redisURL, err := rC.ConnectionString(ctx)
	require.NoError(t, err)
	rOpts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	rdb := redis.NewClient(rOpts)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	// --- Wire the real snapshot cache + proxy + middleware + router ---
	store := snapshot.NewPGStore(pool)
	cache := snapshot.NewCache(store, pgConn, logger, 30*time.Second)
	require.NoError(t, cache.Start(ctx))
	t.Cleanup(cache.Stop)

	m := metrics.New(prometheus.NewRegistry())
	limiter := ratelimit.NewRedisLimiter(rdb)
	proxyHandler := proxy.NewHandler(cache, limiter, m, logger)
	deps := &Deps{
		ProxyHandler:       proxyHandler,
		Cache:              cache,
		Limiter:            limiter,
		RateLimitCfg:       middleware.RateLimitConfig{DefaultRPM: 10, DefaultTPM: 1000},
		AdminKey:           "admin-secret",
		ReadinessStaleness: 60 * time.Second,
	}
	srv := httptest.NewServer(newRouter(logger, m, deps))
	t.Cleanup(srv.Close)

	// --- POST /v1/chat/completions with the API key ---
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"qwen3-27b","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	respBody, _ := io.ReadAll(resp.Body)
	var r map[string]any
	require.NoError(t, json.Unmarshal(respBody, &r))
	assert.Equal(t, "qwen3-27b", r["model"], "response model is the alias")
	assert.Equal(t, "Qwen/Qwen3-27B", backendGotModel, "backend received the rewritten HF id")

	// --- /readyz (fresh -> 200) ---
	rr, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	rr.Body.Close()
	assert.Equal(t, http.StatusOK, rr.StatusCode)

	// --- /admin/v1/snapshot (debug; shows the consumed catalog) ---
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/v1/snapshot", nil)
	req2.Header.Set("X-Admin-Key", "admin-secret")
	rr2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer rr2.Body.Close()
	assert.Equal(t, http.StatusOK, rr2.StatusCode)
	snapBody, _ := io.ReadAll(rr2.Body)
	var snap map[string]any
	require.NoError(t, json.Unmarshal(snapBody, &snap))
	assert.True(t, snap["fresh"].(bool))
	catalog, ok := snap["catalog"].([]any)
	require.True(t, ok)
	require.Len(t, catalog, 1)
	assert.Equal(t, "qwen3-27b", catalog[0].(map[string]any)["model_id"])
}
