package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no row matches (e.g. an invalid API key, or an
// identity with no rate-limit policy — caller treats rate-limit not-found as
// unlimited).
var ErrNotFound = errors.New("snapshot: not found")

// Store is the read-only interface over the control-plane views. The Cache
// wraps a Store; tests use a fake (B+C interface boundary).
type Store interface {
	// ListCatalog returns the effective_catalog rows (the gateway's routing table).
	ListCatalog(ctx context.Context) ([]CatalogEntry, error)
	// APIKeyByHash resolves an API key hash to its identity_id. ErrNotFound = invalid/revoked/expired.
	APIKeyByHash(ctx context.Context, keyHash string) (string, error)
	// Capabilities returns the capability rows granted to an identity.
	Capabilities(ctx context.Context, identityID string) ([]Capability, error)
	// IdentityRateLimits returns the per-identity throughput limit. ErrNotFound = unlimited.
	IdentityRateLimits(ctx context.Context, identityID string) (IdentityRateLimits, error)
}

// PGStore implements Store reading the control-plane views directly from the
// shared Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps a pool for snapshot reads.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func (s *PGStore) ListCatalog(ctx context.Context) ([]CatalogEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model_id, display_name, context_length, capabilities, backend_ref,
		       backend_kind, backend_model_id, backend_url,
		       default_params, reasoning_config, transforms, rate_limits, available
		FROM catalog.effective_catalog`)
	if err != nil {
		return nil, fmt.Errorf("query effective_catalog: %w", err)
	}
	defer rows.Close()

	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		var caps, dp, rc, tf, rl []byte
		if err := rows.Scan(&e.ModelID, &e.DisplayName, &e.ContextLength, &caps, &e.BackendRef,
			&e.BackendKind, &e.BackendModelID, &e.BackendURL, &dp, &rc, &tf, &rl, &e.Available); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		_ = json.Unmarshal(caps, &e.Capabilities)
		_ = json.Unmarshal(dp, &e.DefaultParams)
		_ = json.Unmarshal(rc, &e.ReasoningConfig)
		_ = json.Unmarshal(tf, &e.Transforms)
		_ = json.Unmarshal(rl, &e.RateLimits)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PGStore) APIKeyByHash(ctx context.Context, keyHash string) (string, error) {
	var identityID string
	err := s.pool.QueryRow(ctx, `
		SELECT identity_id FROM identity.api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())`, keyHash).Scan(&identityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return identityID, err
}

func (s *PGStore) Capabilities(ctx context.Context, identityID string) ([]Capability, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT resource, action FROM permissions.effective_capabilities WHERE identity_id = $1`,
		identityID)
	if err != nil {
		return nil, fmt.Errorf("query effective capabilities: %w", err)
	}
	defer rows.Close()

	var out []Capability
	for rows.Next() {
		var c Capability
		if err := rows.Scan(&c.Resource, &c.Action); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PGStore) IdentityRateLimits(ctx context.Context, identityID string) (IdentityRateLimits, error) {
	var rl IdentityRateLimits
	err := s.pool.QueryRow(ctx, `
		SELECT rpm, tpm FROM permissions.effective_rate_limits WHERE identity_id = $1`,
		identityID).Scan(&rl.RPM, &rl.TPM)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityRateLimits{}, ErrNotFound
	}
	return rl, err
}
