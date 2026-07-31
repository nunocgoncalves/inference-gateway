package workload

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrScopeDenied is returned when the caller's durable scope is genuinely not
// resolvable — unknown pool, unknown/inactive/terminal turn, run not assigned
// to the caller's pool, or caller-supplied malformed IDs. Callers map this to
// 403 (a real authorization denial, not an infrastructure failure).
var ErrScopeDenied = errors.New("workload: scope denied")

// ErrInfrastructure is returned when the scope cannot be resolved due to an
// infrastructure failure (DB connection loss, timeout, canceled query, schema
// mismatch). Callers fail closed (no access granted) but report it as 503, not
// 403 — a DB outage is not an authorization denial (AGENTS.md: infra failures
// surface as 502/503, not misleading 403s that change supervisor retry
// semantics).
var ErrInfrastructure = errors.New("workload: infrastructure error")

// Store is the read-only interface over the durable control-plane tables the
// workload path validates against. The middleware wraps a Store; tests use a
// fake (B+C interface boundary).
type Store interface {
	// ResolvePoolBySpiffePrefix finds the pool whose spiffe_id_prefix is a
	// prefix of the supervisor's verified SPIFFE id (the pool is derived from
	// the workload identity, not caller-supplied — ARCH-004/010).
	ResolvePoolBySpiffePrefix(ctx context.Context, spiffeID string) (Pool, error)
	// ResolveTurnScope resolves a supervisor/turn caller against durable
	// runtime state. The supplied runID + turnID must match a running turn
	// (dispatched to a worker) whose run is durably assigned to poolID. Returns
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
		// Connection loss / timeout / canceled query / schema drift — fail
		// closed but as an infrastructure error (503), not a 403 denial.
		return Pool{}, fmt.Errorf("%w: resolve pool by spiffe prefix: %v", ErrInfrastructure, err)
	}
	return p, nil
}

// ResolveTurnScope resolves a supervisor/turn caller against durable runtime
// state. The supplied runID + turnID must match a RUNNING turn (a turn that has
// been dispatched to a worker — pending turns are NOT accepted, since no worker
// is yet authorized to open inference for them) whose run is durably assigned
// to poolID. Returns the assigned model + effective identity. Fail closed
// otherwise.
//
// NOTE (HOR-398): this binds the turn to the pool resolved from the verified
// SPIFFE id, but does NOT yet bind to the specific verified worker / fencing
// generation — run_pool_assignments currently carries only pool_id. The
// authoritative worker/fencing-generation check is provided by HOR-249 (still
// Todo); until that durable contract exists, a still-valid supervisor cert
// from the same pool can open inference for a running turn. Requiring the
// 'running' state closes the pre-dispatch window but not the same-pool
// cross-worker window. See <finding> on store.go:65.
func (s *PGStore) ResolveTurnScope(ctx context.Context, poolID, runID, turnID string) (TurnScope, error) {
	// runID + turnID are caller-supplied (headers); validate they are UUIDs
	// before hitting the DB so a malformed-scope input is a 403 denial, not a
	// Postgres syntax error that would otherwise be indistinguishable from an
	// infrastructure failure. poolID is resolved from the verified SPIFFE id and
	// is already a valid UUID.
	if _, err := uuid.Parse(runID); err != nil {
		return TurnScope{}, ErrScopeDenied
	}
	if _, err := uuid.Parse(turnID); err != nil {
		return TurnScope{}, ErrScopeDenied
	}
	var ts TurnScope
	err := s.pool.QueryRow(ctx, `
		SELECT t.run_id::text, t.id::text, t.state, COALESCE(t.model, ''), wr.scope_identity_id::text
		FROM runtime.turns t
		JOIN runtime.run_pool_assignments a ON a.run_id = t.run_id
		JOIN runtime.workflow_runs wr ON wr.id = t.run_id
		WHERE t.id = $1::uuid AND t.state = 'running'
		  AND t.run_id::text = $2 AND a.pool_id = $3::uuid`,
		turnID, runID, poolID).Scan(&ts.RunID, &ts.TurnID, &ts.TurnState, &ts.AssignedModel, &ts.ScopeIdentityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TurnScope{}, ErrScopeDenied
		}
		// Genuine infrastructure failure (connection loss / timeout / canceled
		// query) — fail closed as 503, not a 403 denial.
		return TurnScope{}, fmt.Errorf("%w: resolve turn scope: %v", ErrInfrastructure, err)
	}
	return ts, nil
}
