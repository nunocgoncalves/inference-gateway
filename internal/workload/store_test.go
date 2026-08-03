package workload

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

func setupStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
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
	_, err = pool.Exec(ctx, FixtureSchema)
	require.NoError(t, err)
	return NewPGStore(pool), pool
}

// seed inserts a pool + run + assignment + turn + active turn-assignment and
// returns the ids. turnState + model are configurable. The active assignment
// is bound to worker 'pod-abc' at fencing generation 1 (HOR-249/DEC-041).
func seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, spiffePrefix, model, turnState string) (poolID, runID, turnID, scopeIdentityID string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO toolgateway.pools (key, name, spiffe_id_prefix)
		VALUES ('default/pool-1', 'pool-1', $1) RETURNING id::text`, spiffePrefix).Scan(&poolID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO runtime.workflow_runs (kind, scope_identity_id, session_id, session_dir)
		VALUES ('chat', gen_random_uuid(), 'sess-1', '/tmp/sess') RETURNING id::text`).Scan(&runID))
	_, err := pool.Exec(ctx, `INSERT INTO runtime.run_pool_assignments (run_id, pool_id) VALUES ($1, $2::uuid)`, runID, poolID)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO runtime.turns (run_id, session_id, model, state) VALUES ($1, 'sess-1', $2, $3) RETURNING id::text`,
		runID, model, turnState).Scan(&turnID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT scope_identity_id::text FROM runtime.workflow_runs WHERE id = $1::uuid`, runID).Scan(&scopeIdentityID))
	// Active assignment bound to worker pod-abc at generation 1 (HOR-249).
	if turnState == "running" {
		var ident string
		require.NoError(t, pool.QueryRow(ctx, `SELECT scope_identity_id::text FROM runtime.workflow_runs WHERE id = $1::uuid`, runID).Scan(&ident))
		_, err := pool.Exec(ctx, `
			INSERT INTO runtime.turn_assignments
			    (turn_id, run_id, pool_id, worker_id, fencing_generation, attempt_id,
			     scope_identity_id, agent_pool_key, state)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'pod-abc', 1, $2, $4::uuid, 'pool-1', 'active')`,
			turnID, runID, poolID, ident)
		require.NoError(t, err)
	}
	return poolID, runID, turnID, scopeIdentityID
}

func TestResolvePoolBySpiffePrefix_OK(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, _, _, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")

	got, err := store.ResolvePoolBySpiffePrefix(ctx, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc")
	require.NoError(t, err)
	assert.Equal(t, poolID, got.ID)
	assert.Equal(t, "spiffe://iterabase.local/pools/pool-1/", got.SpiffeIDPrefix)
}

func TestResolvePoolBySpiffePrefix_Unknown(t *testing.T) {
	ctx := context.Background()
	store, _ := setupStore(t)
	_, err := store.ResolvePoolBySpiffePrefix(ctx, "spiffe://iterabase.local/pools/unknown/workers/x")
	assert.ErrorIs(t, err, ErrScopeDenied)
}

func TestResolveTurnScope_OK(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, runID, turnID, scopeID := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")

	ts, err := store.ResolveTurnScope(ctx, poolID, runID, turnID, "pod-abc", 1)
	require.NoError(t, err)
	assert.Equal(t, runID, ts.RunID)
	assert.Equal(t, turnID, ts.TurnID)
	assert.Equal(t, "running", ts.TurnState)
	assert.Equal(t, "qwen3-27b", ts.AssignedModel)
	assert.Equal(t, scopeID, ts.ScopeIdentityID)
}

func TestResolveTurnScope_PendingDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, runID, turnID, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "pending")
	// A pending turn has not been dispatched to a worker — no worker is yet
	// authorized to open inference for it, so it must be denied (HOR-398).
	_, err := store.ResolveTurnScope(ctx, poolID, runID, turnID, "pod-abc", 1)
	assert.ErrorIs(t, err, ErrScopeDenied)
}

func TestResolveTurnScope_TerminalDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, runID, turnID, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "succeeded")
	// The partial unique index on active turns means a 'succeeded' turn coexists
	// with a new active turn only if the session differs; here we simply expect
	// the terminal turn to be denied.
	_, err := store.ResolveTurnScope(ctx, poolID, runID, turnID, "pod-abc", 1)
	assert.ErrorIs(t, err, ErrScopeDenied)
}

func TestResolveTurnScope_WrongPoolDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	_, runID, turnID, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")
	// A different pool id never matches the assignment.
	_, err := store.ResolveTurnScope(ctx, "00000000-0000-0000-0000-000000000000", runID, turnID, "pod-abc", 1)
	assert.ErrorIs(t, err, ErrScopeDenied)
}

func TestResolveTurnScope_WrongRunDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, _, turnID, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")
	_, err := store.ResolveTurnScope(ctx, poolID, "00000000-0000-0000-0000-000000000000", turnID, "pod-abc", 1)
	assert.ErrorIs(t, err, ErrScopeDenied)
}

func TestResolveTurnScope_MalformedIDsDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, runID, _, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")
	_, err := store.ResolveTurnScope(ctx, poolID, runID, "not-a-uuid", "pod-abc", 1)
	assert.ErrorIs(t, err, ErrScopeDenied)
}

// TestResolveTurnScope_WrongWorkerDenied: a still-valid supervisor cert from
// a different same-pool worker is denied (DEC-041 same-pool residual closed).
func TestResolveTurnScope_WrongWorkerDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, runID, turnID, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")
	_, err := store.ResolveTurnScope(ctx, poolID, runID, turnID, "pod-other", 1)
	assert.ErrorIs(t, err, ErrScopeDenied)
}

// TestResolveTurnScope_StaleGenerationDenied: a fenced/old-generation caller
// is denied even with the correct worker (DEC-041).
func TestResolveTurnScope_StaleGenerationDenied(t *testing.T) {
	ctx := context.Background()
	store, pool := setupStore(t)
	poolID, runID, turnID, _ := seed(t, ctx, pool, "spiffe://iterabase.local/pools/pool-1/", "qwen3-27b", "running")
	_, err := store.ResolveTurnScope(ctx, poolID, runID, turnID, "pod-abc", 2)
	assert.ErrorIs(t, err, ErrScopeDenied)
}
