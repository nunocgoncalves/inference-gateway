package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	// keyLength is the number of random bytes in a generated API key.
	keyLength = 32
	// keyPrefixLen is how many characters of the key are stored for display.
	keyPrefixLen = 8
	// keyDisplayPrefix is prepended to all generated keys for easy identification.
	keyDisplayPrefix = "ml-"
)

// APIKey represents a stored API key with its metadata.
type APIKey struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	KeyHash       string     `json:"-"`          // Never expose
	KeyPrefix     string     `json:"key_prefix"` // e.g. "ml-a1b2c3d4"
	Active        bool       `json:"active"`
	RPMLimit      *int       `json:"rpm_limit,omitempty"`
	TPMLimit      *int       `json:"tpm_limit,omitempty"`
	AllowedModels []string   `json:"allowed_models,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// GenerateKey creates a new cryptographically random API key and returns
// the plaintext key, the SHA-256 hash, and the display prefix. The plaintext
// should be shown to the user once and never stored.
func GenerateKey() (plaintext string, hash string, prefix string, err error) {
	b := make([]byte, keyLength)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generating random key: %w", err)
	}

	plaintext = keyDisplayPrefix + hex.EncodeToString(b)
	hash = HashKey(plaintext)
	prefix = plaintext[:len(keyDisplayPrefix)+keyPrefixLen]

	return plaintext, hash, prefix, nil
}

// HashKey returns the SHA-256 hex digest of a plaintext API key.
func HashKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// IsExpired returns true if the key has an expiration date that has passed.
func (k *APIKey) IsExpired() bool {
	if k.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*k.ExpiresAt)
}

// IsModelAllowed checks whether the key is permitted to access the given model.
// If AllowedModels is nil or empty, all models are allowed.
func (k *APIKey) IsModelAllowed(modelName string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, m := range k.AllowedModels {
		if m == modelName {
			return true
		}
	}
	return false
}

// contextKey is an unexported type used for storing auth data in context.
type contextKey struct{}

// WithAPIKey stores the authenticated API key in the request context.
func WithAPIKey(ctx context.Context, key *APIKey) context.Context {
	return context.WithValue(ctx, contextKey{}, key)
}

// APIKeyFromContext retrieves the authenticated API key from the context.
// Returns nil if no key is present (unauthenticated request).
func APIKeyFromContext(ctx context.Context) *APIKey {
	key, _ := ctx.Value(contextKey{}).(*APIKey)
	return key
}
