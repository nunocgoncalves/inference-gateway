package loadbalancer

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeBackend(id string, weight int, healthy bool) registry.Backend {
	return registry.Backend{
		ID:      id,
		URL:     fmt.Sprintf("http://%s:8000", id),
		Weight:  weight,
		Active:  true,
		Healthy: healthy,
	}
}

// ---------------------------------------------------------------------------
// WeightedRoundRobin tests
// ---------------------------------------------------------------------------

func TestWRR_BasicDistribution(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 3, true),
		makeBackend("b", 1, true),
	}
	wrr := New(backends)

	// Over 40 requests, expect roughly 3:1 distribution.
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		counts[b.ID]++
	}

	assert.Equal(t, 30, counts["a"], "backend a (weight=3) should get 30/40")
	assert.Equal(t, 10, counts["b"], "backend b (weight=1) should get 10/40")
}

func TestWRR_EqualWeights(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 1, true),
		makeBackend("b", 1, true),
		makeBackend("c", 1, true),
	}
	wrr := New(backends)

	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		counts[b.ID]++
	}

	assert.Equal(t, 10, counts["a"])
	assert.Equal(t, 10, counts["b"])
	assert.Equal(t, 10, counts["c"])
}

func TestWRR_SmoothDistribution(t *testing.T) {
	// With weights 5:1, the smooth WRR should NOT send 5 consecutive to "a"
	// then 1 to "b". It should interleave.
	backends := []registry.Backend{
		makeBackend("a", 5, true),
		makeBackend("b", 1, true),
	}
	wrr := New(backends)

	sequence := make([]string, 12)
	for i := 0; i < 12; i++ {
		b, _ := wrr.Next()
		sequence[i] = b.ID
	}

	// In smooth WRR with 5:1, the pattern for 6 requests should be: a a a a b a
	// (or similar — "b" appears every ~6th request, not in a burst at the end).
	// Verify "b" appears within the first 6 requests.
	foundB := false
	for i := 0; i < 6; i++ {
		if sequence[i] == "b" {
			foundB = true
			break
		}
	}
	assert.True(t, foundB, "smooth WRR should interleave b within first 6 requests, got: %v", sequence[:6])
}

func TestWRR_SingleBackend(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("solo", 1, true),
	}
	wrr := New(backends)

	for i := 0; i < 10; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		assert.Equal(t, "solo", b.ID)
	}
}

func TestWRR_NoBackends(t *testing.T) {
	wrr := New([]registry.Backend{})
	_, err := wrr.Next()
	assert.ErrorIs(t, err, ErrNoBackends)
}

func TestWRR_AllUnhealthy(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 1, false),
		makeBackend("b", 1, false),
	}
	wrr := New(backends)

	_, err := wrr.Next()
	assert.ErrorIs(t, err, ErrAllUnhealthy)
}

func TestWRR_SkipsUnhealthy(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("healthy", 1, true),
		makeBackend("unhealthy", 1, false),
	}
	wrr := New(backends)

	for i := 0; i < 10; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		assert.Equal(t, "healthy", b.ID)
	}
}

func TestWRR_MarkHealthy(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 1, true),
		makeBackend("b", 1, true),
	}
	wrr := New(backends)

	// Mark "a" unhealthy.
	wrr.MarkHealthy("a", false)

	for i := 0; i < 5; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		assert.Equal(t, "b", b.ID)
	}

	// Mark "a" healthy again.
	wrr.MarkHealthy("a", true)

	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		b, _ := wrr.Next()
		counts[b.ID]++
	}
	assert.Equal(t, 5, counts["a"])
	assert.Equal(t, 5, counts["b"])
}

func TestWRR_SkipsInactive(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("active", 1, true),
		{ID: "inactive", URL: "http://inactive:8000", Weight: 1, Active: false, Healthy: true},
	}
	wrr := New(backends)

	for i := 0; i < 5; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		assert.Equal(t, "active", b.ID)
	}
}

func TestWRR_UpdateBackends(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 1, true),
	}
	wrr := New(backends)

	// Verify initial state.
	b, _ := wrr.Next()
	assert.Equal(t, "a", b.ID)

	// Add a new backend.
	wrr.UpdateBackends([]registry.Backend{
		makeBackend("a", 1, true),
		makeBackend("b", 1, true),
	})

	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		b, _ := wrr.Next()
		counts[b.ID]++
	}
	assert.Equal(t, 5, counts["a"])
	assert.Equal(t, 5, counts["b"])
}

func TestWRR_UpdateBackendsPreservesHealth(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 1, true),
		makeBackend("b", 1, true),
	}
	wrr := New(backends)

	// Mark "b" unhealthy.
	wrr.MarkHealthy("b", false)

	// Update backends — should preserve "b" as unhealthy.
	wrr.UpdateBackends([]registry.Backend{
		makeBackend("a", 1, true),
		makeBackend("b", 1, true),
	})

	for i := 0; i < 5; i++ {
		b, err := wrr.Next()
		require.NoError(t, err)
		assert.Equal(t, "a", b.ID, "b should still be unhealthy after update")
	}
}

func TestWRR_ConcurrentAccess(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 1, true),
		makeBackend("b", 1, true),
	}
	wrr := New(backends)

	var wg sync.WaitGroup
	counts := make(map[string]int)
	var mu sync.Mutex

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := wrr.Next()
			if err != nil {
				return
			}
			mu.Lock()
			counts[b.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, 50, counts["a"])
	assert.Equal(t, 50, counts["b"])
}

func TestWRR_Backends_Snapshot(t *testing.T) {
	backends := []registry.Backend{
		makeBackend("a", 3, true),
		makeBackend("b", 1, true),
	}
	wrr := New(backends)

	snap := wrr.Backends()
	assert.Len(t, snap, 2)
	assert.Equal(t, "a", snap[0].ID)
	assert.Equal(t, 3, snap[0].Weight)
}

// ---------------------------------------------------------------------------
// HealthChecker tests
// ---------------------------------------------------------------------------

func TestHealthChecker_DetectsUnhealthy(t *testing.T) {
	// Start a server that returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var changed []bool
	var mu sync.Mutex
	onChange := func(id string, healthy bool) {
		mu.Lock()
		changed = append(changed, healthy)
		mu.Unlock()
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hc := NewHealthChecker(HealthCheckConfig{
		Interval:           100 * time.Millisecond,
		Timeout:            1 * time.Second,
		HealthyThreshold:   2,
		UnhealthyThreshold: 1,
	}, logger, onChange, nil)

	backends := []registry.Backend{
		{ID: "bad", URL: srv.URL, Weight: 1, Active: true, Healthy: true},
	}
	hc.Start(backends)
	defer hc.Stop()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changed) > 0 && !changed[0]
	}, 2*time.Second, 50*time.Millisecond, "should detect unhealthy backend")
}

func TestHealthChecker_DetectsRecovery(t *testing.T) {
	var healthy bool
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := healthy
		mu.Unlock()
		if h {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var changes []bool
	var changesMu sync.Mutex
	onChange := func(id string, h bool) {
		changesMu.Lock()
		changes = append(changes, h)
		changesMu.Unlock()
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hc := NewHealthChecker(HealthCheckConfig{
		Interval:           100 * time.Millisecond,
		Timeout:            1 * time.Second,
		HealthyThreshold:   2,
		UnhealthyThreshold: 1,
	}, logger, onChange, nil)

	backends := []registry.Backend{
		{ID: "recover", URL: srv.URL, Weight: 1, Active: true, Healthy: true},
	}
	hc.Start(backends)
	defer hc.Stop()

	// Wait for it to be marked unhealthy.
	require.Eventually(t, func() bool {
		changesMu.Lock()
		defer changesMu.Unlock()
		return len(changes) > 0 && !changes[0]
	}, 2*time.Second, 50*time.Millisecond)

	// Now make it healthy.
	mu.Lock()
	healthy = true
	mu.Unlock()

	// Wait for it to recover (needs 2 consecutive successes).
	require.Eventually(t, func() bool {
		changesMu.Lock()
		defer changesMu.Unlock()
		for i := len(changes) - 1; i >= 0; i-- {
			if changes[i] {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond, "should detect backend recovery")
}

func TestHealthChecker_PassiveFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var changed []bool
	var mu sync.Mutex
	onChange := func(id string, healthy bool) {
		mu.Lock()
		changed = append(changed, healthy)
		mu.Unlock()
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hc := NewHealthChecker(HealthCheckConfig{
		Interval:           10 * time.Minute, // Long interval — won't fire during test
		Timeout:            1 * time.Second,
		HealthyThreshold:   3,
		UnhealthyThreshold: 1,
	}, logger, onChange, nil)

	backends := []registry.Backend{
		{ID: "passive", URL: srv.URL, Weight: 1, Active: true, Healthy: true},
	}
	hc.Start(backends)
	defer hc.Stop()

	// Report a failure (passive health check).
	hc.ReportFailure("passive")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, changed, 1)
	assert.False(t, changed[0], "passive failure should mark backend unhealthy immediately")
}

func TestHealthChecker_UpdateBackends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hc := NewHealthChecker(HealthCheckConfig{
		Interval:           10 * time.Minute,
		Timeout:            1 * time.Second,
		HealthyThreshold:   1,
		UnhealthyThreshold: 1,
	}, logger, nil, nil)

	hc.Start([]registry.Backend{
		{ID: "old", URL: srv.URL, Weight: 1, Active: true, Healthy: true},
	})
	defer hc.Stop()

	// Update to new backends.
	hc.UpdateBackends([]registry.Backend{
		{ID: "new", URL: srv.URL, Weight: 1, Active: true, Healthy: true},
	})

	hc.mu.Lock()
	defer hc.mu.Unlock()
	_, hasOld := hc.probes["old"]
	_, hasNew := hc.probes["new"]
	assert.False(t, hasOld, "old probe should be removed")
	assert.True(t, hasNew, "new probe should be added")
}

func TestHealthChecker_UnreachableBackend(t *testing.T) {
	var changed []bool
	var mu sync.Mutex
	onChange := func(id string, healthy bool) {
		mu.Lock()
		changed = append(changed, healthy)
		mu.Unlock()
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hc := NewHealthChecker(HealthCheckConfig{
		Interval:           100 * time.Millisecond,
		Timeout:            200 * time.Millisecond,
		HealthyThreshold:   3,
		UnhealthyThreshold: 1,
	}, logger, onChange, nil)

	// Point to a port that nothing listens on.
	backends := []registry.Backend{
		{ID: "unreachable", URL: "http://127.0.0.1:1", Weight: 1, Active: true, Healthy: true},
	}
	hc.Start(backends)
	defer hc.Stop()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changed) > 0 && !changed[0]
	}, 3*time.Second, 50*time.Millisecond, "unreachable backend should be marked unhealthy")
}
