package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const (
	// RequestIDHeader is the canonical header name for request IDs.
	RequestIDHeader = "X-Request-ID"
	// requestIDLength is the number of random bytes used to generate an ID (16 bytes = 32 hex chars).
	requestIDLength = 16
)

type requestIDKey struct{}

// RequestID is middleware that ensures every request has a unique ID.
// If the incoming request carries an X-Request-ID header, that value is reused.
// Otherwise a new random hex ID is generated.
//
// The ID is:
//   - Stored in the request context (retrieve with RequestIDFromContext)
//   - Set on the response as X-Request-ID
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = generateID()
		}

		// Set on the response header immediately so it's available even on errors.
		w.Header().Set(RequestIDHeader, id)

		// Store in context for downstream handlers and middleware.
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext retrieves the request ID from the context.
// Returns an empty string if no ID is present.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// generateID produces a cryptographically random hex string.
func generateID() string {
	b := make([]byte, requestIDLength)
	// crypto/rand.Read never returns an error on supported platforms.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
