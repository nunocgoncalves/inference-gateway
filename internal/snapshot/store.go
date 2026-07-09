package snapshot

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the read-only interface over the control-plane views. The Cache
// wraps a Store and does full refreshes; tests use a fake (B+C interface
// boundary).
type Store interface {
	// ListCatalog returns the effective_catalog rows (the gateway's routing table).
	ListCatalog(ctx context.Context) ([]CatalogEntry, error)
	// AllAPIKeys returns active (non-revoked, non-expired) API keys.
	AllAPIKeys(ctx context.Context) ([]APIKey, error)
	// AllCapabilities returns every effective_capabilities row.
	AllCapabilities(ctx context.Context) ([]Capability, error)
	// AllRateLimits returns every effective_rate_limits row.
	AllRateLimits(ctx context.Context) ([]IdentityRateLimits, error)
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
		var displayName *string
		var contextLength *int
		var backendModelID *string
		var caps, dp, rc, tf, rl []byte
		if err := rows.Scan(&e.ModelID, &displayName, &contextLength, &caps, &e.BackendRef,
			&e.BackendKind, &backendModelID, &e.BackendURL, &dp, &rc, &tf, &rl, &e.Available); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		if displayName != nil {
			e.DisplayName = *displayName
		}
		if contextLength != nil {
			e.ContextLength = *contextLength
		}
		if backendModelID != nil {
			e.BackendModelID = *backendModelID
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

func (s *PGStore) AllAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key_hash, identity_id FROM identity.api_keys
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.KeyHash, &k.IdentityID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PGStore) AllCapabilities(ctx context.Context) ([]Capability, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT identity_id, resource, action FROM permissions.effective_capabilities`)
	if err != nil {
		return nil, fmt.Errorf("query effective_capabilities: %w", err)
	}
	defer rows.Close()

	var out []Capability
	for rows.Next() {
		var c Capability
		if err := rows.Scan(&c.IdentityID, &c.Resource, &c.Action); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PGStore) AllRateLimits(ctx context.Context) ([]IdentityRateLimits, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT identity_id, rpm, tpm FROM permissions.effective_rate_limits`)
	if err != nil {
		return nil, fmt.Errorf("query effective_rate_limits: %w", err)
	}
	defer rows.Close()

	var out []IdentityRateLimits
	for rows.Next() {
		var r IdentityRateLimits
		if err := rows.Scan(&r.IdentityID, &r.RPM, &r.TPM); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
