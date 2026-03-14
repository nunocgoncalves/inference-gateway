package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store defines the persistence interface for API key management.
type Store interface {
	// ValidateKey looks up a key by its hash and returns the key metadata.
	// Returns nil if the key is not found, inactive, or expired.
	ValidateKey(ctx context.Context, keyHash string) (*APIKey, error)

	// CreateKey stores a new API key. The key's ID, CreatedAt, and UpdatedAt
	// are populated on return.
	CreateKey(ctx context.Context, key *APIKey) error

	// GetKey returns a key by ID.
	GetKey(ctx context.Context, id string) (*APIKey, error)

	// ListKeys returns all keys (active and inactive).
	ListKeys(ctx context.Context) ([]APIKey, error)

	// UpdateKey updates a key's mutable fields (name, limits, allowed models, active).
	UpdateKey(ctx context.Context, key *APIKey) error

	// DeleteKey soft-deletes a key by setting active=false.
	DeleteKey(ctx context.Context, id string) error
}

// PGStore implements Store using PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore creates a new PostgreSQL-backed auth store.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

func (s *PGStore) ValidateKey(ctx context.Context, keyHash string) (*APIKey, error) {
	query := `
		SELECT id, name, key_hash, key_prefix, active,
		       rpm_limit, tpm_limit, allowed_models,
		       expires_at, created_at, updated_at
		FROM api_keys
		WHERE key_hash = $1 AND active = true`

	row := s.pool.QueryRow(ctx, query, keyHash)
	key, err := scanAPIKey(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil // Not found — not an error, just invalid key
		}
		return nil, fmt.Errorf("validating key: %w", err)
	}

	// Check expiration.
	if key.IsExpired() {
		return nil, nil
	}

	return key, nil
}

func (s *PGStore) CreateKey(ctx context.Context, key *APIKey) error {
	query := `
		INSERT INTO api_keys (name, key_hash, key_prefix, active, rpm_limit, tpm_limit, allowed_models, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`

	return s.pool.QueryRow(ctx, query,
		key.Name, key.KeyHash, key.KeyPrefix, key.Active,
		key.RPMLimit, key.TPMLimit, key.AllowedModels, key.ExpiresAt,
	).Scan(&key.ID, &key.CreatedAt, &key.UpdatedAt)
}

func (s *PGStore) GetKey(ctx context.Context, id string) (*APIKey, error) {
	query := `
		SELECT id, name, key_hash, key_prefix, active,
		       rpm_limit, tpm_limit, allowed_models,
		       expires_at, created_at, updated_at
		FROM api_keys
		WHERE id = $1`

	row := s.pool.QueryRow(ctx, query, id)
	key, err := scanAPIKey(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("key not found: %s", id)
		}
		return nil, fmt.Errorf("getting key: %w", err)
	}
	return key, nil
}

func (s *PGStore) ListKeys(ctx context.Context) ([]APIKey, error) {
	query := `
		SELECT id, name, key_hash, key_prefix, active,
		       rpm_limit, tpm_limit, allowed_models,
		       expires_at, created_at, updated_at
		FROM api_keys
		ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		key, err := scanAPIKeyRows(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, *key)
	}
	return keys, rows.Err()
}

func (s *PGStore) UpdateKey(ctx context.Context, key *APIKey) error {
	query := `
		UPDATE api_keys
		SET name = $1, active = $2, rpm_limit = $3, tpm_limit = $4,
		    allowed_models = $5, expires_at = $6, updated_at = now()
		WHERE id = $7
		RETURNING updated_at`

	result := s.pool.QueryRow(ctx, query,
		key.Name, key.Active, key.RPMLimit, key.TPMLimit,
		key.AllowedModels, key.ExpiresAt, key.ID,
	)
	if err := result.Scan(&key.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("key not found: %s", key.ID)
		}
		return fmt.Errorf("updating key: %w", err)
	}
	return nil
}

func (s *PGStore) DeleteKey(ctx context.Context, id string) error {
	query := `UPDATE api_keys SET active = false, updated_at = now() WHERE id = $1`
	tag, err := s.pool.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("deleting key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("key not found: %s", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

func scanAPIKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Active,
		&k.RPMLimit, &k.TPMLimit, &k.AllowedModels,
		&k.ExpiresAt, &k.CreatedAt, &k.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func scanAPIKeyRows(rows pgx.Rows) (*APIKey, error) {
	var k APIKey
	err := rows.Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Active,
		&k.RPMLimit, &k.TPMLimit, &k.AllowedModels,
		&k.ExpiresAt, &k.CreatedAt, &k.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scanning api key: %w", err)
	}
	return &k, nil
}
