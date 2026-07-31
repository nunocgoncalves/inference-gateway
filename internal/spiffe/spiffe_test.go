package spiffe

import (
	"crypto/tls"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_Supervisor(t *testing.T) {
	id, err := Parse("spiffe://iterabase.local/pools/pool-1/workers/pod-abc", DefaultTrustDomain)
	require.NoError(t, err)
	assert.Equal(t, KindSupervisor, id.Kind)
	assert.Equal(t, "iterabase.local", id.TrustDomain)
	assert.Equal(t, "pool-1", id.PoolUID)
	assert.Equal(t, "pod-abc", id.WorkerID)
	assert.Equal(t, "spiffe://iterabase.local/pools/pool-1/workers/pod-abc", id.SPIFFEID)
}

func TestParse_TrustDomainMismatch(t *testing.T) {
	_, err := Parse("spiffe://evil.example/pools/pool-1/workers/pod-abc", DefaultTrustDomain)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trust domain mismatch")
}

func TestParse_UnrecognizedPath(t *testing.T) {
	// A tool-runner or workflow-runtime id is not a valid inference caller.
	for _, s := range []string{
		"spiffe://iterabase.local/tool-runners/default/runner-1",
		"spiffe://iterabase.local/control-plane/workflow-runtime",
		"spiffe://iterabase.local/pools/pool-1", // missing worker segment
	} {
		_, err := Parse(s, DefaultTrustDomain)
		require.Error(t, err, "path %q must be rejected", s)
		assert.Contains(t, err.Error(), "unrecognized id path")
	}
}

func TestParse_NotSPIFFE(t *testing.T) {
	_, err := Parse("https://iterabase.local/pools/pool-1/workers/pod-abc", DefaultTrustDomain)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a spiffe:// URI")
}

func TestIdentityFromConnState_NoTLS(t *testing.T) {
	_, err := IdentityFromConnState(nil, DefaultTrustDomain)
	require.Error(t, err)
}

func TestIdentityFromCerts_Empty(t *testing.T) {
	_, err := IdentityFromCerts(nil, DefaultTrustDomain)
	require.Error(t, err)
}

func TestServerTLSConfig_H2Only(t *testing.T) {
	cfg := ServerTLSConfig(tls.Certificate{}, nil)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	require.Len(t, cfg.NextProtos, 1)
	assert.Equal(t, "h2", cfg.NextProtos[0])
	assert.GreaterOrEqual(t, cfg.MinVersion, uint16(tls.VersionTLS12))
}

func TestKindString(t *testing.T) {
	assert.Equal(t, "supervisor", KindSupervisor.String())
	assert.Equal(t, "unknown", KindUnknown.String())
}

// Ensure errors are usable as values (no panic on Is).
func TestParse_ErrorIs(t *testing.T) {
	_, err := Parse("garbage", DefaultTrustDomain)
	require.Error(t, err)
	assert.False(t, errors.Is(err, assert.AnError))
}
