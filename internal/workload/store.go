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
	// (dispatched to a worker) whose run is durably assigned to poolID AND
	// whose active assignment is bound to the verified workerID + the caller's
	// current fencing generation (HOR-249 / DEC-041: a fenced/old-generation
	// supervisor cert is denied). Returns the assigned model + effective
	// identity. Fail closed otherwise.
	ResolveTurnScope(ctx context.Context, poolID, runID, turnID, workerID string, fencingGeneration uint64) (TurnScope, error)
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
// to poolID AND whose active assignment is bound to the verified workerID +
// the caller's current fencing generation. Returns the assigned model +
// effective identity. Fail closed otherwise.
//
// HOR-249 / DEC-041 (closed): the active-assignment cross-check binds
// authorization to the specific verified worker (cert SAN pod name) AND the
// current fencing generation, so a still-valid supervisor cert from a different
// same-pool worker — or a fenced/old-generation cert — is denied for a running
// turn. The control-plane dispatch (HOR-249) writes runtime.turn_assignments
// with (worker_id, fencing_generation, state='active'); this read fails closed
// when no active row matches.
func (s *PGStore) ResolveTurnScope(ctx context.Context, poolID, runID, turnID, workerID string, fencingGeneration uint64) (TurnScope, error) {
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
	// One fail-closed join (HOR-249 / DEC-041): the turn, its run-pool
	// assignment, the workflow run, AND the active turn assignment must ALL be
	// coherent with the verified request context — turn id + run id + pool +
	// worker (cert SAN pod name) + current fencing generation — read from a
	// single snapshot. Selecting the active assignment by turn_id alone and
	// Go-comparing worker/generation would accept a wrong-pool row that happens
	// to share a pod name + generation across two unrelated snapshots (worker
	// ids are pod names scoped to a pool, and the production assignment schema
	// carries no cross-schema FK for pool_id). The active assignment's
	// denormalized run_id and pool_id are therefore asserted explicitly against
	// the validated run + run-pool assignment (ta.run_id = t.run_id,
	// ta.pool_id = a.pool_id = poolID) so an inconsistent row — same pod name +
	// generation but a mismatched run or pool — yields no result. Returning the
	// scope from this single coherent row guarantees the bound identity is the
	// validated one. A fenced/terminal assignment (no active row), a different
	// same-pool worker, an old-generation caller, or a run/pool mismatch all
	// yield no row -> ErrScopeDenied.
	var ts TurnScope
	err := s.pool.QueryRow(ctx, `
		SELECT t.run_id::text, t.id::text, t.state, COALESCE(t.model, ''), wr.scope_identity_id::text
		FROM runtime.turns t
		JOIN runtime.run_pool_assignments a ON a.run_id = t.run_id
		JOIN runtime.workflow_runs wr ON wr.id = t.run_id
		JOIN runtime.turn_assignments ta ON ta.turn_id = t.id
		WHERE t.id = $1::uuid AND t.state = 'running'
		  AND t.run_id::text = $2
		  AND a.pool_id = $3::uuid
		  AND ta.state = 'active'
		  AND ta.run_id = t.run_id
		  AND ta.pool_id = a.pool_id
		  AND ta.worker_id = $4
		  AND ta.fencing_generation = $5`,
		turnID, runID, poolID, workerID, fencingGeneration).
		Scan(&ts.RunID, &ts.TurnID, &ts.TurnState, &ts.AssignedModel, &ts.ScopeIdentityID)
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
