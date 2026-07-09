package middleware

import (
	"context"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

// fakeReader is a snapshot.Reader fake for middleware unit tests.
type fakeReader struct {
	apiKeys    map[string]string
	caps       map[string][]snapshot.Capability
	rateLimits map[string]snapshot.IdentityRateLimits
}

func (f *fakeReader) CatalogEntry(string) (snapshot.CatalogEntry, bool) {
	return snapshot.CatalogEntry{}, false
}
func (f *fakeReader) ListCatalog() []snapshot.CatalogEntry         { return nil }
func (f *fakeReader) IdentityByAPIKey(h string) (string, bool)     { id, ok := f.apiKeys[h]; return id, ok }
func (f *fakeReader) Capabilities(id string) []snapshot.Capability { return f.caps[id] }
func (f *fakeReader) RateLimits(id string) (snapshot.IdentityRateLimits, bool) {
	rl, ok := f.rateLimits[id]
	return rl, ok
}
func (f *fakeReader) Fresh(time.Duration) bool { return true }
func (f *fakeReader) LastRefresh() time.Time   { return time.Now() }

// fakeLimiter is a ratelimit.Limiter fake that records the last RPM limit.
type fakeLimiter struct {
	rpmAllowed   bool
	lastRPMLimit int
}

func (f *fakeLimiter) CheckRPM(_ context.Context, _ string, limit int) (*ratelimit.Result, error) {
	f.lastRPMLimit = limit
	return &ratelimit.Result{Allowed: f.rpmAllowed, Limit: limit, Remaining: 0, ResetAt: time.Now().Add(time.Minute)}, nil
}
func (f *fakeLimiter) CheckTPM(_ context.Context, _ string, limit int) (*ratelimit.Result, error) {
	return &ratelimit.Result{Allowed: true, Limit: limit, Remaining: 999, ResetAt: time.Now().Add(time.Minute)}, nil
}
func (f *fakeLimiter) IncrementTPM(_ context.Context, _ string, _ int) error { return nil }
