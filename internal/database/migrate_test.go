package database

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

func startPostgres(t *testing.T) (string, func()) {
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

	cleanup := func() {
		require.NoError(t, pgContainer.Terminate(ctx))
	}

	return connStr, cleanup
}

func TestMigrateUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	connStr, cleanup := startPostgres(t)
	defer cleanup()

	// Run migrations up.
	err := MigrateUp(connStr)
	require.NoError(t, err)

	// Verify the tables exist by connecting and querying.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	defer pool.Close()

	// Check models table exists.
	var exists bool
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'models')").
		Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "models table should exist")

	// Check model_backends table exists.
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'model_backends')").
		Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "model_backends table should exist")

	// Check api_keys table exists.
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'api_keys')").
		Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "api_keys table should exist")

	// Verify current version.
	version, dirty, err := MigrateVersion(connStr)
	require.NoError(t, err)
	assert.Equal(t, uint(2), version)
	assert.False(t, dirty)
}

func TestMigrateUpIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	connStr, cleanup := startPostgres(t)
	defer cleanup()

	// Run up twice — second call should be a no-op.
	require.NoError(t, MigrateUp(connStr))
	require.NoError(t, MigrateUp(connStr))

	version, dirty, err := MigrateVersion(connStr)
	require.NoError(t, err)
	assert.Equal(t, uint(2), version)
	assert.False(t, dirty)
}

func TestMigrateDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	connStr, cleanup := startPostgres(t)
	defer cleanup()

	// Migrate up first.
	require.NoError(t, MigrateUp(connStr))

	// Roll back 1 step (api_keys).
	require.NoError(t, MigrateDown(connStr, 1))

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	defer pool.Close()

	// api_keys should be gone.
	var exists bool
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'api_keys')").
		Scan(&exists)
	require.NoError(t, err)
	assert.False(t, exists, "api_keys table should not exist after rollback")

	// models should still be there.
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'models')").
		Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "models table should still exist")

	version, _, err := MigrateVersion(connStr)
	require.NoError(t, err)
	assert.Equal(t, uint(1), version)
}

func TestMigrateDownAll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	connStr, cleanup := startPostgres(t)
	defer cleanup()

	require.NoError(t, MigrateUp(connStr))

	// Roll back all (steps=0).
	require.NoError(t, MigrateDown(connStr, 0))

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	defer pool.Close()

	for _, table := range []string{"models", "model_backends", "api_keys"} {
		var exists bool
		err = pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)", table).
			Scan(&exists)
		require.NoError(t, err)
		assert.False(t, exists, "%s table should not exist after full rollback", table)
	}
}

func TestMigrateSchemaDetails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	connStr, cleanup := startPostgres(t)
	defer cleanup()

	require.NoError(t, MigrateUp(connStr))

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	defer pool.Close()

	// Verify models table has the expected columns.
	rows, err := pool.Query(ctx,
		`SELECT column_name, data_type, is_nullable
		 FROM information_schema.columns
		 WHERE table_name = 'models'
		 ORDER BY ordinal_position`)
	require.NoError(t, err)
	defer rows.Close()

	columns := make(map[string]string)
	for rows.Next() {
		var name, dataType, nullable string
		require.NoError(t, rows.Scan(&name, &dataType, &nullable))
		columns[name] = dataType
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, "uuid", columns["id"])
	assert.Equal(t, "text", columns["name"])
	assert.Equal(t, "text", columns["model_id"])
	assert.Equal(t, "boolean", columns["active"])
	assert.Equal(t, "jsonb", columns["default_params"])
	assert.Equal(t, "jsonb", columns["reasoning_config"])
	assert.Equal(t, "jsonb", columns["transforms"])
	assert.Equal(t, "jsonb", columns["rate_limits"])

	// Verify model_backends has the weight CHECK constraint by inserting invalid data.
	_, err = pool.Exec(ctx,
		`INSERT INTO models (name, model_id) VALUES ('test-model', 'test/model-id')`)
	require.NoError(t, err)

	var modelID string
	err = pool.QueryRow(ctx, `SELECT id FROM models WHERE name = 'test-model'`).Scan(&modelID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO model_backends (model_id, url, weight) VALUES ($1, 'http://localhost:8000', 0)`,
		modelID)
	assert.Error(t, err, "weight=0 should violate CHECK constraint")

	// Verify the unique constraint on (model_id, url).
	_, err = pool.Exec(ctx,
		`INSERT INTO model_backends (model_id, url, weight) VALUES ($1, 'http://localhost:8000', 1)`,
		modelID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO model_backends (model_id, url, weight) VALUES ($1, 'http://localhost:8000', 2)`,
		modelID)
	assert.Error(t, err, "duplicate (model_id, url) should violate unique constraint")

	// Verify api_keys unique constraint on key_hash.
	_, err = pool.Exec(ctx,
		`INSERT INTO api_keys (name, key_hash, key_prefix) VALUES ('key1', 'hash123', 'ml-abcd')`)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO api_keys (name, key_hash, key_prefix) VALUES ('key2', 'hash123', 'ml-efgh')`)
	assert.Error(t, err, "duplicate key_hash should violate unique constraint")
}
