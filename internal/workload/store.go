package workload

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrScopeDenied is returned when the caller's durable scope cannot be
// resolved (unknown pool, unknown/inactive/terminal turn, run not assigned to
// the caller's pool, or a DB error). Callers fail closed (403).
var ErrScopeDenied = errors.New("workload: scope denied")

// Store is the read-only interface over the durable control-plane tables the
// workload path validates against. The middleware wraps a Store; tests use a
// fake (B+C interface boundary).
type Store interface {
	// ResolvePoolBySpiffePrefix finds the pool whose spiffe_id_prefix is a
	// prefix of the supervisor's verified SPIFFE id (the pool is derived from
	// the workload identity, not caller-supplied — ARCH-004/010).
	ResolvePoolBySpiffePrefix(ctx context.Context, spiffeID string) (Pool, error)
	// ResolveTurnScope resolves a supervisor/turn caller against durable
	// runtime state. The supplied runID + turnID must match an active
	// (pending/running) turn whose run is durably assigned to poolID. Returns
	// the assigned model + effective identity. Fail closed otherwise.
	ResolveTurnScope(ctx context.Context, poolID, runID, turnID string) (TurnScope, error)
}

// PGStore implements Store reading the control-plane tables directly from the
// shared Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps a pool for workload reads. It MUST share the same Postgres
// as the snapshot cache (the control-plane DB).
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func (s *PGStore) ResolvePoolBySpiffePrefix(ctx context.Context, spiffeID string) (Pool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id::text, key, name, spiffe_id_prefix
		FROM toolgateway.pools
		WHERE $1 LIKE spiffe_id_prefix || '%' AND deleted_at IS NULL
		ORDER BY length(spiffe_id_prefix) DESC LIMIT 1`, spiffeID)
	var p Pool
	if err := row.Scan(&p.ID, &p.Key, &p.Name, &p.SpiffeIDPrefix); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Pool{}, ErrScopeDenied
		}
		return Pool{}, fmt.Errorf("resolve pool by spiffe prefix: %w", err)
	}
	return p, nil
}

func (s *PGStore) ResolveTurnScope(ctx context.Context, poolID, runID, turnID string) (TurnScope, error) {
	var ts TurnScope
	err := s.pool.QueryRow(ctx, `
		SELECT t.run_id::text, t.id::text, t.state, COALESCE(t.model, ''), wr.scope_identity_id::text
		FROM runtime.turns t
		JOIN runtime.run_pool_assignments a ON a.run_id = t.run_id
		JOIN runtime.workflow_runs wr ON wr.id = t.run_id
		WHERE t.id = $1::uuid AND t.state IN ('pending', 'running')
		  AND t.run_id::text = $2 AND a.pool_id = $3::uuid`,
		turnID, runID, poolID).Scan(&ts.RunID, &ts.TurnID, &ts.TurnState, &ts.AssignedModel, &ts.ScopeIdentityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TurnScope{}, ErrScopeDenied
		}
		// A malformed uuid / DB error is a denial, not a 500 leak — fail closed.
		return TurnScope{}, ErrScopeDenied
	}
	return ts, nil
}
