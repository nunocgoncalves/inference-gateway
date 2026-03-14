package ratelimit

import (
	"context"
	"time"
)

// Result holds the outcome of a rate limit check.
type Result struct {
	Allowed   bool
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// Limiter defines the interface for rate limiting operations.
type Limiter interface {
	// CheckRPM checks and increments the requests-per-minute counter for a key.
	// Returns whether the request is allowed plus metadata for response headers.
	CheckRPM(ctx context.Context, keyID string, limit int) (*Result, error)

	// CheckTPM checks whether the key is within its tokens-per-minute budget.
	// Does NOT increment — use IncrementTPM after knowing the token count.
	CheckTPM(ctx context.Context, keyID string, limit int) (*Result, error)

	// IncrementTPM adds tokens to the TPM counter. Called after a response
	// completes (non-streaming) or periodically during streaming (batched).
	IncrementTPM(ctx context.Context, keyID string, tokens int) error
}
