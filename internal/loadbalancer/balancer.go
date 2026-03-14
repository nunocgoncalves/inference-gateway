package loadbalancer

import (
	"errors"
	"sync"

	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

var (
	// ErrNoBackends is returned when no backends are configured for a model.
	ErrNoBackends = errors.New("no backends configured")
	// ErrAllUnhealthy is returned when all backends for a model are unhealthy.
	ErrAllUnhealthy = errors.New("all backends are unhealthy")
)

// backendState tracks the smooth weighted round-robin state for a backend.
type backendState struct {
	backend         registry.Backend
	effectiveWeight int
	currentWeight   int
}

// WeightedRoundRobin implements smooth weighted round-robin load balancing
// (the algorithm used by Nginx). It distributes requests proportionally
// across backends according to their weights while keeping the distribution
// smooth (no bursts to a single backend).
type WeightedRoundRobin struct {
	mu       sync.Mutex
	backends []backendState
}

// New creates a WeightedRoundRobin balancer from a list of backends.
// Only active backends are included.
func New(backends []registry.Backend) *WeightedRoundRobin {
	states := make([]backendState, 0, len(backends))
	for _, b := range backends {
		if b.Active {
			states = append(states, backendState{
				backend:         b,
				effectiveWeight: b.Weight,
				currentWeight:   0,
			})
		}
	}
	return &WeightedRoundRobin{backends: states}
}

// Next selects the next backend using the smooth weighted round-robin
// algorithm. It skips unhealthy backends. Returns ErrNoBackends if no
// backends are configured, or ErrAllUnhealthy if all are down.
func (wrr *WeightedRoundRobin) Next() (*registry.Backend, error) {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	if len(wrr.backends) == 0 {
		return nil, ErrNoBackends
	}

	totalWeight := 0
	var best *backendState

	for i := range wrr.backends {
		s := &wrr.backends[i]

		if !s.backend.Healthy {
			continue
		}

		s.currentWeight += s.effectiveWeight
		totalWeight += s.effectiveWeight

		if best == nil || s.currentWeight > best.currentWeight {
			best = s
		}
	}

	if best == nil {
		return nil, ErrAllUnhealthy
	}

	best.currentWeight -= totalWeight
	return &best.backend, nil
}

// MarkHealthy sets the health status of a backend by ID.
func (wrr *WeightedRoundRobin) MarkHealthy(backendID string, healthy bool) {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	for i := range wrr.backends {
		if wrr.backends[i].backend.ID == backendID {
			wrr.backends[i].backend.Healthy = healthy
			if !healthy {
				// Reset current weight to avoid a burst when it recovers.
				wrr.backends[i].currentWeight = 0
			}
			return
		}
	}
}

// UpdateBackends replaces the backend list. This is called when the registry
// cache refreshes. It preserves health state for backends that still exist.
func (wrr *WeightedRoundRobin) UpdateBackends(backends []registry.Backend) {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	// Build a map of existing health states.
	healthMap := make(map[string]bool, len(wrr.backends))
	for _, s := range wrr.backends {
		healthMap[s.backend.ID] = s.backend.Healthy
	}

	states := make([]backendState, 0, len(backends))
	for _, b := range backends {
		if !b.Active {
			continue
		}
		// Preserve existing health state if known.
		if healthy, ok := healthMap[b.ID]; ok {
			b.Healthy = healthy
		}
		states = append(states, backendState{
			backend:         b,
			effectiveWeight: b.Weight,
			currentWeight:   0,
		})
	}

	wrr.backends = states
}

// Backends returns a snapshot of the current backend states (for health
// endpoint / observability).
func (wrr *WeightedRoundRobin) Backends() []registry.Backend {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	result := make([]registry.Backend, len(wrr.backends))
	for i, s := range wrr.backends {
		result[i] = s.backend
	}
	return result
}
