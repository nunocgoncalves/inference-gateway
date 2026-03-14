package auth

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

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

	require.NoError(t, database.MigrateUp(connStr))

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)

	cleanup := func() {
		pool.Close()
		require.NoError(t, pgContainer.Terminate(ctx))
	}
	return pool, cleanup
}

func ptrInt(v int) *int              { return &v }
func ptrTime(v time.Time) *time.Time { return &v }

func TestGenerateKey(t *testing.T) {
	plaintext, hash, prefix, err := GenerateKey()
	require.NoError(t, err)

	// Key starts with the display prefix.
	assert.True(t, len(plaintext) > 0)
	assert.Contains(t, plaintext, "ml-")

	// Hash is a valid hex SHA-256 (64 chars).
	assert.Len(t, hash, 64)

	// Prefix is the first portion of the plaintext.
	assert.Equal(t, plaintext[:len(keyDisplayPrefix)+keyPrefixLen], prefix)

	// Hashing the plaintext again yields the same hash.
	assert.Equal(t, hash, HashKey(plaintext))
}

func TestGenerateKey_Uniqueness(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 100; i++ {
		plaintext, _, _, err := GenerateKey()
		require.NoError(t, err)
		assert.False(t, keys[plaintext], "duplicate key generated")
		keys[plaintext] = true
	}
}

func TestAPIKey_IsExpired(t *testing.T) {
	// Not expired (no expiration).
	k := &APIKey{}
	assert.False(t, k.IsExpired())

	// Not expired (future).
	future := time.Now().Add(1 * time.Hour)
	k.ExpiresAt = &future
	assert.False(t, k.IsExpired())

	// Expired.
	past := time.Now().Add(-1 * time.Hour)
	k.ExpiresAt = &past
	assert.True(t, k.IsExpired())
}

func TestAPIKey_IsModelAllowed(t *testing.T) {
	// No restrictions.
	k := &APIKey{}
	assert.True(t, k.IsModelAllowed("any-model"))

	// Empty list = no restrictions.
	k.AllowedModels = []string{}
	assert.True(t, k.IsModelAllowed("any-model"))

	// Restricted.
	k.AllowedModels = []string{"llama-70b", "gpt-4o"}
	assert.True(t, k.IsModelAllowed("llama-70b"))
	assert.True(t, k.IsModelAllowed("gpt-4o"))
	assert.False(t, k.IsModelAllowed("other-model"))
}

// ---------------------------------------------------------------------------
// PGStore tests
// ---------------------------------------------------------------------------

func TestPGStore_CreateAndValidateKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	plaintext, hash, prefix, err := GenerateKey()
	require.NoError(t, err)

	key := &APIKey{
		Name:      "test-key",
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    true,
		RPMLimit:  ptrInt(100),
		TPMLimit:  ptrInt(50000),
	}
	require.NoError(t, store.CreateKey(ctx, key))
	assert.NotEmpty(t, key.ID)
	assert.False(t, key.CreatedAt.IsZero())

	// Validate with correct key.
	got, err := store.ValidateKey(ctx, HashKey(plaintext))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, key.ID, got.ID)
	assert.Equal(t, "test-key", got.Name)
	assert.Equal(t, 100, *got.RPMLimit)
	assert.Equal(t, 50000, *got.TPMLimit)

	// Validate with wrong key.
	got, err = store.ValidateKey(ctx, HashKey("wrong-key"))
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPGStore_ValidateKey_Inactive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	_, hash, prefix, _ := GenerateKey()
	key := &APIKey{
		Name:      "inactive-key",
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    false,
	}
	require.NoError(t, store.CreateKey(ctx, key))

	// Should not validate inactive keys.
	got, err := store.ValidateKey(ctx, hash)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPGStore_ValidateKey_Expired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	_, hash, prefix, _ := GenerateKey()
	past := time.Now().Add(-1 * time.Hour)
	key := &APIKey{
		Name:      "expired-key",
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    true,
		ExpiresAt: &past,
	}
	require.NoError(t, store.CreateKey(ctx, key))

	// Should not validate expired keys.
	got, err := store.ValidateKey(ctx, hash)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPGStore_ValidateKey_WithAllowedModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	_, hash, prefix, _ := GenerateKey()
	key := &APIKey{
		Name:          "restricted-key",
		KeyHash:       hash,
		KeyPrefix:     prefix,
		Active:        true,
		AllowedModels: []string{"llama-70b", "gpt-4o"},
	}
	require.NoError(t, store.CreateKey(ctx, key))

	got, err := store.ValidateKey(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"llama-70b", "gpt-4o"}, got.AllowedModels)
	assert.True(t, got.IsModelAllowed("llama-70b"))
	assert.False(t, got.IsModelAllowed("other"))
}

func TestPGStore_GetKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	_, hash, prefix, _ := GenerateKey()
	key := &APIKey{
		Name:      "get-test",
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    true,
	}
	require.NoError(t, store.CreateKey(ctx, key))

	got, err := store.GetKey(ctx, key.ID)
	require.NoError(t, err)
	assert.Equal(t, "get-test", got.Name)

	// Not found.
	_, err = store.GetKey(ctx, "00000000-0000-0000-0000-000000000000")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestPGStore_ListKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, hash, prefix, _ := GenerateKey()
		require.NoError(t, store.CreateKey(ctx, &APIKey{
			Name:      "key-" + prefix,
			KeyHash:   hash,
			KeyPrefix: prefix,
			Active:    true,
		}))
	}

	keys, err := store.ListKeys(ctx)
	require.NoError(t, err)
	assert.Len(t, keys, 3)
}

func TestPGStore_UpdateKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	_, hash, prefix, _ := GenerateKey()
	key := &APIKey{
		Name:      "before-update",
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    true,
	}
	require.NoError(t, store.CreateKey(ctx, key))

	key.Name = "after-update"
	key.RPMLimit = ptrInt(200)
	key.AllowedModels = []string{"model-a"}
	require.NoError(t, store.UpdateKey(ctx, key))

	got, err := store.GetKey(ctx, key.ID)
	require.NoError(t, err)
	assert.Equal(t, "after-update", got.Name)
	assert.Equal(t, 200, *got.RPMLimit)
	assert.Equal(t, []string{"model-a"}, got.AllowedModels)
}

func TestPGStore_DeleteKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	plaintext, hash, prefix, _ := GenerateKey()
	key := &APIKey{
		Name:      "to-delete",
		KeyHash:   hash,
		KeyPrefix: prefix,
		Active:    true,
	}
	require.NoError(t, store.CreateKey(ctx, key))

	// Delete (soft).
	require.NoError(t, store.DeleteKey(ctx, key.ID))

	// Should no longer validate.
	got, err := store.ValidateKey(ctx, HashKey(plaintext))
	require.NoError(t, err)
	assert.Nil(t, got, "deleted key should not validate")

	// But GetKey still returns it (with active=false).
	got, err = store.GetKey(ctx, key.ID)
	require.NoError(t, err)
	assert.False(t, got.Active)
}

func TestPGStore_DeleteKey_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewPGStore(pool)
	ctx := context.Background()

	err := store.DeleteKey(ctx, "00000000-0000-0000-0000-000000000000")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
