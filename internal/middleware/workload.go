package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/nunocgoncalves/inference-gateway/internal/spiffe"
	"github.com/nunocgoncalves/inference-gateway/internal/workload"
)

// Turn-context headers supplied by the supervisor. These are VALIDATED against
// durable state, never trusted as scope: the SPIFFE-derived pool + the turn row
// are the authoritative scope (ARCH-004). Absent/invalid -> 403.
const (
	HeaderRunID  = "X-Iterabase-Run-Id"
	HeaderTurnID = "X-Iterabase-Turn-Id"
)

// workloadCtxKey carries the resolved durable scope for a workload caller.
type workloadCtxKey struct{}

// WorkloadContext is the durable scope resolved for a supervisor caller. The
// proxy handler uses AssignedModel to deny model-mismatched requests and
// EffectiveIdentityID (also stamped via WithIdentityID) for catalogue authz,
// usage, and rate limits.
type WorkloadContext struct {
	PoolID            string
	PoolKey           string
	RunID             string
	TurnID            string
	AssignedModel     string
	EffectiveIdentity string
}

// WithWorkloadContext stores the resolved workload scope in the context.
func WithWorkloadContext(ctx context.Context, wc WorkloadContext) context.Context {
	return context.WithValue(ctx, workloadCtxKey{}, wc)
}

// WorkloadContextFromContext returns the workload scope, or false if the
// request did not arrive on the workload (mTLS) path.
func WorkloadContextFromContext(ctx context.Context) (WorkloadContext, bool) {
	wc, ok := ctx.Value(workloadCtxKey{}).(WorkloadContext)
	return wc, ok
}

// WorkloadAuth validates a supervisor's mTLS SPIFFE identity and active durable
// turn context (HOR-398; ARCH-004/010). It runs AFTER tls verifies the chain
// (RequireAndVerifyClientCert). The pool is resolved from the verified SPIFFE
// id; the run/turn are validated against runtime state. On success it stamps
// both the effective identity (via WithIdentityID, so the existing proxy
// capability/usage/rate-limit pipeline works unchanged) and the WorkloadContext
// (for the model-mismatch check). Any failure is 403 with an OpenAI-compatible
// error; no fallback (REQ-010/SCN-009).
func WorkloadAuth(store workload.Store, trustDomain string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := spiffe.IdentityFromConnState(r.TLS, trustDomain)
			if err != nil || id.Kind != spiffe.KindSupervisor {
				writeWorkloadError(w, http.StatusForbidden, "unauthenticated workload identity", "invalid_workload_identity")
				return
			}
			pool, err := store.ResolvePoolBySpiffePrefix(r.Context(), id.SPIFFEID)
			if err != nil {
				logger.Warn("workload auth: pool not resolved", "spiffe_id", id.SPIFFEID, "error", err)
				writeWorkloadError(w, http.StatusForbidden, "workload identity not bound to an active pool", "pool_not_authorized")
				return
			}
			runID := r.Header.Get(HeaderRunID)
			turnID := r.Header.Get(HeaderTurnID)
			if runID == "" || turnID == "" {
				writeWorkloadError(w, http.StatusForbidden, "missing turn context", "missing_turn_context")
				return
			}
			ts, err := store.ResolveTurnScope(r.Context(), pool.ID, runID, turnID)
			if err != nil {
				logger.Warn("workload auth: turn scope denied",
					"spiffe_id", id.SPIFFEID, "pool", pool.Key, "run_id", runID, "turn_id", turnID, "error", err)
				writeWorkloadError(w, http.StatusForbidden, "turn not active or not assigned to this pool", "turn_not_authorized")
				return
			}
			wc := WorkloadContext{
				PoolID:            pool.ID,
				PoolKey:           pool.Key,
				RunID:             ts.RunID,
				TurnID:            ts.TurnID,
				AssignedModel:     ts.AssignedModel,
				EffectiveIdentity: ts.ScopeIdentityID,
			}
			ctx := WithWorkloadContext(r.Context(), wc)
			// Stamp the effective identity so the existing capability/usage/
			// rate-limit pipeline (keyed on IdentityIDFromContext) serves the
			// workload path unchanged.
			ctx = WithIdentityID(ctx, ts.ScopeIdentityID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeWorkloadError(w http.ResponseWriter, status int, message string, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "authentication_error",
			"code":    code,
		},
	}); err != nil {
		slog.Error("failed to write workload auth error response", "error", err)
	}
}
