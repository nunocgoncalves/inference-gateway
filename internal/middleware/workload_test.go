package middleware

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/inference-gateway/internal/spiffe"
	"github.com/nunocgoncalves/inference-gateway/internal/spiffe/testca"
	"github.com/nunocgoncalves/inference-gateway/internal/workload"
)

// fakeStore is an in-memory workload.Store for middleware tests.
type fakeStore struct {
	pool      workload.Pool
	poolErr   error
	turn      workload.TurnScope
	turnErr   error
	gotSpiffe string
	gotPoolID string
	gotRunID  string
	gotTurnID string
}

func (f *fakeStore) ResolvePoolBySpiffePrefix(_ context.Context, spiffeID string) (workload.Pool, error) {
	f.gotSpiffe = spiffeID
	return f.pool, f.poolErr
}

func (f *fakeStore) ResolveTurnScope(_ context.Context, poolID, runID, turnID string) (workload.TurnScope, error) {
	f.gotPoolID, f.gotRunID, f.gotTurnID = poolID, runID, turnID
	return f.turn, f.turnErr
}

// mintSupervisorLeaf issues a supervisor SPIFFE leaf cert and returns its
// parsed *x509.Certificate for stuffing into r.TLS.PeerCertificates.
func mintSupervisorLeaf(t *testing.T, spiffeID string) *x509.Certificate {
	t.Helper()
	ca, err := testca.New()
	require.NoError(t, err)
	leaf, err := ca.Leaf(testca.LeafOpts{SPIFFEID: spiffeID})
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	require.NoError(t, err)
	return parsed
}

func newWorkloadRequest(t *testing.T, spiffeID, runID, turnID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if spiffeID != "" {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{mintSupervisorLeaf(t, spiffeID)}}
	}
	if runID != "" {
		req.Header.Set(HeaderRunID, runID)
	}
	if turnID != "" {
		req.Header.Set(HeaderTurnID, turnID)
	}
	return req
}

func TestWorkloadAuth_OK(t *testing.T) {
	store := &fakeStore{
		pool: workload.Pool{ID: "pool-1", Key: "default/pool-1", SpiffeIDPrefix: "spiffe://iterabase.local/pools/pool-1/"},
		turn: workload.TurnScope{RunID: "run-1", TurnID: "turn-1", TurnState: "running", AssignedModel: "qwen3-27b", ScopeIdentityID: "id-7"},
	}
	called := false
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		wc, ok := WorkloadContextFromContext(r.Context())
		require.True(t, ok)
		assert.Equal(t, "pool-1", wc.PoolID)
		assert.Equal(t, "run-1", wc.RunID)
		assert.Equal(t, "turn-1", wc.TurnID)
		assert.Equal(t, "qwen3-27b", wc.AssignedModel)
		assert.Equal(t, "id-7", wc.EffectiveIdentity)
		// Effective identity also stamped via WithIdentityID (shared pipeline).
		assert.Equal(t, "id-7", IdentityIDFromContext(r.Context()))
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newWorkloadRequest(t, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc", "run-1", "turn-1"))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rr.Code)
	// The store received the validated ids.
	assert.Equal(t, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc", store.gotSpiffe)
	assert.Equal(t, "pool-1", store.gotPoolID)
	assert.Equal(t, "run-1", store.gotRunID)
	assert.Equal(t, "turn-1", store.gotTurnID)
}

func TestWorkloadAuth_NoMTLS(t *testing.T) {
	store := &fakeStore{}
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil) // no r.TLS
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestWorkloadAuth_NonSupervisorSAN(t *testing.T) {
	store := &fakeStore{}
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newWorkloadRequest(t, "spiffe://iterabase.local/tool-runners/default/r1", "run-1", "turn-1"))
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestWorkloadAuth_TrustDomainMismatch(t *testing.T) {
	store := &fakeStore{}
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newWorkloadRequest(t, "spiffe://evil.example/pools/pool-1/workers/pod-abc", "run-1", "turn-1"))
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestWorkloadAuth_PoolNotResolved(t *testing.T) {
	store := &fakeStore{poolErr: workload.ErrScopeDenied}
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newWorkloadRequest(t, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc", "run-1", "turn-1"))
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestWorkloadAuth_MissingTurnContext(t *testing.T) {
	store := &fakeStore{pool: workload.Pool{ID: "pool-1"}}
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	rr := httptest.NewRecorder()
	// No run/turn headers.
	h.ServeHTTP(rr, newWorkloadRequest(t, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc", "", ""))
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestWorkloadAuth_TurnScopeDenied(t *testing.T) {
	store := &fakeStore{
		pool:    workload.Pool{ID: "pool-1", SpiffeIDPrefix: "spiffe://iterabase.local/pools/pool-1/"},
		turnErr: workload.ErrScopeDenied,
	}
	h := WorkloadAuth(store, spiffe.DefaultTrustDomain, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newWorkloadRequest(t, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc", "run-1", "turn-1"))
	assert.Equal(t, http.StatusForbidden, rr.Code)
}
