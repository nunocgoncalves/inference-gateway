package loadbalancer

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// HealthCheckConfig holds configuration for the health checker.
type HealthCheckConfig struct {
	Interval           time.Duration
	Timeout            time.Duration
	HealthyThreshold   int // Consecutive successes to mark healthy
	UnhealthyThreshold int // Consecutive failures to mark unhealthy
}

// OnHealthChange is called when a backend's health state changes.
type OnHealthChange func(backendID string, healthy bool)

// HealthChecker performs periodic active health checks against backends
// and supports passive failure reporting from the proxy layer.
type HealthChecker struct {
	cfg      HealthCheckConfig
	client   *http.Client
	logger   *slog.Logger
	onChange OnHealthChange

	mu       sync.Mutex
	probes   map[string]*probeState // keyed by backend ID
	stopCh   chan struct{}
	done     chan struct{}
	backends []registry.Backend
}

type probeState struct {
	consecutiveSuccess int
	consecutiveFailure int
	healthy            bool
}

// NewHealthChecker creates a new health checker.
func NewHealthChecker(cfg HealthCheckConfig, logger *slog.Logger, onChange OnHealthChange) *HealthChecker {
	return &HealthChecker{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		logger:   logger,
		onChange: onChange,
		probes:   make(map[string]*probeState),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the periodic health check loop for the given backends.
func (hc *HealthChecker) Start(backends []registry.Backend) {
	hc.mu.Lock()
	hc.backends = backends
	for _, b := range backends {
		if _, ok := hc.probes[b.ID]; !ok {
			hc.probes[b.ID] = &probeState{healthy: b.Healthy}
		}
	}
	hc.mu.Unlock()

	go hc.loop()
}

// Stop terminates the health check loop.
func (hc *HealthChecker) Stop() {
	close(hc.stopCh)
	<-hc.done
}

// UpdateBackends updates the set of backends to check. Called when the
// registry refreshes.
func (hc *HealthChecker) UpdateBackends(backends []registry.Backend) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	hc.backends = backends

	// Add new backends.
	activeIDs := make(map[string]bool, len(backends))
	for _, b := range backends {
		activeIDs[b.ID] = true
		if _, ok := hc.probes[b.ID]; !ok {
			hc.probes[b.ID] = &probeState{healthy: b.Healthy}
		}
	}

	// Remove stale probes.
	for id := range hc.probes {
		if !activeIDs[id] {
			delete(hc.probes, id)
		}
	}
}

// ReportFailure is called by the proxy when a request to a backend fails
// (passive health check). It immediately marks the backend unhealthy.
func (hc *HealthChecker) ReportFailure(backendID string) {
	hc.mu.Lock()
	probe, ok := hc.probes[backendID]
	if !ok {
		hc.mu.Unlock()
		return
	}
	probe.consecutiveSuccess = 0
	probe.consecutiveFailure = hc.cfg.UnhealthyThreshold // Immediate failure
	wasHealthy := probe.healthy
	probe.healthy = false
	hc.mu.Unlock()

	if wasHealthy {
		hc.logger.Warn("backend marked unhealthy (passive)", "backend_id", backendID)
		if hc.onChange != nil {
			hc.onChange(backendID, false)
		}
	}
}

func (hc *HealthChecker) loop() {
	defer close(hc.done)
	ticker := time.NewTicker(hc.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.stopCh:
			return
		case <-ticker.C:
			hc.checkAll()
		}
	}
}

func (hc *HealthChecker) checkAll() {
	hc.mu.Lock()
	backends := make([]registry.Backend, len(hc.backends))
	copy(backends, hc.backends)
	hc.mu.Unlock()

	var wg sync.WaitGroup
	for _, b := range backends {
		if !b.Active {
			continue
		}
		wg.Add(1)
		go func(backend registry.Backend) {
			defer wg.Done()
			hc.checkOne(backend)
		}(b)
	}
	wg.Wait()
}

func (hc *HealthChecker) checkOne(backend registry.Backend) {
	ctx, cancel := context.WithTimeout(context.Background(), hc.cfg.Timeout)
	defer cancel()

	healthy := hc.probe(ctx, backend.URL)

	hc.mu.Lock()
	state, ok := hc.probes[backend.ID]
	if !ok {
		hc.mu.Unlock()
		return
	}

	var changed bool
	if healthy {
		state.consecutiveFailure = 0
		state.consecutiveSuccess++
		if !state.healthy && state.consecutiveSuccess >= hc.cfg.HealthyThreshold {
			state.healthy = true
			changed = true
		}
	} else {
		state.consecutiveSuccess = 0
		state.consecutiveFailure++
		if state.healthy && state.consecutiveFailure >= hc.cfg.UnhealthyThreshold {
			state.healthy = false
			changed = true
		}
	}
	newHealthy := state.healthy
	hc.mu.Unlock()

	if changed {
		if newHealthy {
			hc.logger.Info("backend recovered", "backend_id", backend.ID, "url", backend.URL)
		} else {
			hc.logger.Warn("backend marked unhealthy (active)", "backend_id", backend.ID, "url", backend.URL)
		}
		if hc.onChange != nil {
			hc.onChange(backend.ID, newHealthy)
		}
	}
}

func (hc *HealthChecker) probe(ctx context.Context, baseURL string) bool {
	url := fmt.Sprintf("%s/health", baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}

	resp, err := hc.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}
