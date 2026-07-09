package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// fixtureSchema is a test-only mirror of the control-plane contract (the 4 views
// the gateway reads), NOT the control-plane's actual migrations — so the gateway
// tests stay decoupled from the control-plane repo (B+C approach).
const fixtureSchema = `
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS permissions;
CREATE SCHEMA IF NOT EXISTS catalog;

CREATE TABLE identity.identities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE,
    kind text NOT NULL,
    deleted_at timestamptz
);
CREATE TABLE identity.api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id uuid NOT NULL REFERENCES identity.identities(id) ON DELETE CASCADE,
    key_hash text NOT NULL UNIQUE,
    scope text NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz
);
CREATE TABLE permissions.policies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_kind text NOT NULL,
    subject_key text NOT NULL,
    rate_limits jsonb,
    deleted_at timestamptz
);
CREATE VIEW permissions.effective_capabilities AS
    SELECT i.id AS identity_id, '*'::text AS resource, '*'::text AS action
    FROM identity.identities i WHERE i.deleted_at IS NULL;
CREATE VIEW permissions.effective_rate_limits AS
    SELECT i.id AS identity_id,
           MIN((p.rate_limits->>'rpm')::int) AS rpm,
           MIN((p.rate_limits->>'tpm')::int) AS tpm
    FROM permissions.policies p
    JOIN identity.identities i ON i.key = p.subject_key
    WHERE p.deleted_at IS NULL AND i.deleted_at IS NULL AND p.rate_limits IS NOT NULL
    GROUP BY i.id;
CREATE TABLE catalog.backends (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE,
    name text NOT NULL,
    namespace text NOT NULL,
    kind text NOT NULL,
    model text,
    service_url text NOT NULL,
    deployed boolean NOT NULL DEFAULT false,
    healthy boolean NOT NULL DEFAULT false,
    deleted_at timestamptz
);
CREATE TABLE catalog.models (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE,
    namespace text NOT NULL,
    model_id text NOT NULL,
    display_name text,
    context_length integer,
    capabilities jsonb NOT NULL DEFAULT '[]'::jsonb,
    backend_ref text NOT NULL,
    default_params jsonb NOT NULL DEFAULT '{}'::jsonb,
    reasoning_config jsonb NOT NULL DEFAULT '{}'::jsonb,
    transforms jsonb NOT NULL DEFAULT '{}'::jsonb,
    rate_limits jsonb NOT NULL DEFAULT '{}'::jsonb,
    available boolean NOT NULL DEFAULT false,
    deleted_at timestamptz
);
CREATE VIEW catalog.effective_catalog AS
    SELECT m.model_id, m.display_name, m.context_length, m.capabilities, m.backend_ref,
           b.kind AS backend_kind, b.model AS backend_model_id, b.service_url AS backend_url,
           m.default_params, m.reasoning_config, m.transforms, m.rate_limits,
           (m.available AND b.healthy) AS available
    FROM catalog.models m
    JOIN catalog.backends b ON b.name = m.backend_ref AND b.namespace = m.namespace AND b.deleted_at IS NULL
    WHERE m.deleted_at IS NULL;
`

// setupTestDB starts a Postgres container, applies the contract fixture, and
// returns a pool + connection string (the cache needs a connStr for its LISTEN
// connection).
func setupTestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("snapshot_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(30*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	connStr, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, fixtureSchema)
	require.NoError(t, err)
	return pool, connStr
}

// seedFixture inserts a healthy backend, an available model alias (reasoning
// off), an identity + API key, and a rate-limit policy. Returns aliceID.
func seedFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO catalog.backends (key, name, namespace, kind, model, service_url, deployed, healthy)
		VALUES ('default/qwen', 'qwen', 'default', 'vLLM', 'Qwen/Qwen3-27B', 'http://vllm', true, true)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO catalog.models (key, namespace, model_id, display_name, context_length, backend_ref, reasoning_config, available)
		VALUES ('default/qwen3-27b', 'default', 'qwen3-27b', 'Qwen3 27B', 131072, 'qwen', '{"enable_thinking":false}'::jsonb, true)`)
	require.NoError(t, err)
	var aliceID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key, kind) VALUES ('default/alice', 'user') RETURNING id`).Scan(&aliceID))
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.api_keys (identity_id, key_hash, scope) VALUES ($1, 'h1', 'gateway')`, aliceID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO permissions.policies (subject_kind, subject_key, rate_limits)
		VALUES ('user', 'default/alice', '{"rpm":60,"tpm":100000}'::jsonb)`)
	require.NoError(t, err)
	return aliceID
}

// TestPGStore exercises the Store reads against the contract fixture.
func TestPGStore(t *testing.T) {
	pool, _ := setupTestDB(t)
	store := NewPGStore(pool)
	ctx := context.Background()
	aliceID := seedFixture(t, pool)

	// Catalog: alias -> backend_url + backend_model_id (rewrite target) + available.
	entries, err := store.ListCatalog(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, "qwen3-27b", e.ModelID)
	assert.Equal(t, "http://vllm", e.BackendURL)
	assert.Equal(t, "Qwen/Qwen3-27B", e.BackendModelID, "backend_model_id is the HF id the gateway rewrites to")
	assert.True(t, e.Available)
	require.NotNil(t, e.ReasoningConfig.EnableThinking)
	assert.False(t, *e.ReasoningConfig.EnableThinking)

	// API keys.
	keys, err := store.AllAPIKeys(ctx)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "h1", keys[0].KeyHash)
	assert.Equal(t, aliceID, keys[0].IdentityID)

	// Capabilities (broad-default wildcard).
	caps, err := store.AllCapabilities(ctx)
	require.NoError(t, err)
	require.Len(t, caps, 1)
	assert.Equal(t, Capability{IdentityID: aliceID, Resource: "*", Action: "*"}, caps[0])

	// Rate limits.
	rl, err := store.AllRateLimits(ctx)
	require.NoError(t, err)
	require.Len(t, rl, 1)
	assert.Equal(t, IdentityRateLimits{IdentityID: aliceID, RPM: 60, TPM: 100000}, rl[0])
}
