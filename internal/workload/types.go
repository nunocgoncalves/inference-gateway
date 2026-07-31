// Package workload resolves a supervisor caller's durable scope from
// control-plane state on the request path (HOR-398; ARCH-004/010).
//
// Unlike the snapshot cache (catalog/api-keys/capabilities/rate-limits, which
// tolerate LISTEN/NOTIFY lag), turn/assignment state is highly dynamic — a
// stale cache would wrongly accept a fenced/inactive turn. The workload Store
// therefore reads runtime.turns + runtime.run_pool_assignments +
// toolgateway.pools LIVE on every request, mirroring the control-plane tool
// gateway's ResolvePoolBySpiffePrefix / ResolveTurnScope (HOR-392). The gateway
// shares the control-plane's Postgres and reads these tables directly; there
// is no request-path RPC to the control-plane.
package workload

// Pool is an AgentPool registry row (toolgateway.pools). The supervisor's
// SPIFFE id prefix must match spiffe_id_prefix.
type Pool struct {
	ID             string
	Key            string
	Name           string
	SpiffeIDPrefix string
}

// TurnScope is the durable resolution of a supervisor/turn caller. The
// supervisor's pool (resolved from the verified SPIFFE id) is cross-checked:
// the supplied run_id + turn_id must match a running turn whose run is durably
// assigned to that same pool. Fail closed otherwise (ARCH-004). AssignedModel
// is runtime.turns.model — the gateway denies a request whose body model does
// not match it (ARCH-010: deny model-mismatched assignments). ScopeIdentityID
// is runtime.workflow_runs.scope_identity_id — the effective identity for
// catalogue authorization, usage, and rate limits.
type TurnScope struct {
	RunID           string
	TurnID          string
	TurnState       string
	AssignedModel   string
	ScopeIdentityID string
}
