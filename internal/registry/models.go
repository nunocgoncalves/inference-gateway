package registry

import (
	"encoding/json"
	"time"
)

// Model represents a registered model in the gateway.
type Model struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`     // Alias exposed to clients (e.g. "gpt-4o")
	ModelID         string          `json:"model_id"` // Actual vLLM model identifier
	Active          bool            `json:"active"`
	DefaultParams   DefaultParams   `json:"default_params"`
	ReasoningConfig ReasoningConfig `json:"reasoning_config"`
	Transforms      Transforms      `json:"transforms"`
	RateLimits      ModelRateLimits `json:"rate_limits"`
	Backends        []Backend       `json:"backends,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// DefaultParams are the default sampling parameters injected into requests
// when the client does not specify them. All fields are pointers so that
// "not set" (nil) is distinguishable from a zero value.
type DefaultParams struct {
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	MaxTokens         *int     `json:"max_tokens,omitempty"`
	FrequencyPenalty  *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64 `json:"presence_penalty,omitempty"`
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
	TopK              *int     `json:"top_k,omitempty"`
	MinP              *float64 `json:"min_p,omitempty"`
	Stop              []string `json:"stop,omitempty"`
}

// ReasoningConfig controls reasoning/thinking behaviour for a model.
// The gateway forwards this to vLLM as chat_template_kwargs.enable_thinking.
//
//   - EnableThinking == nil  → passthrough (no override, honour client request)
//   - EnableThinking == true → force thinking on
//   - EnableThinking == false → force thinking off
type ReasoningConfig struct {
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}

// Transforms define request/response transformation rules applied per model.
type Transforms struct {
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty"`
	RewriteModelName   bool   `json:"rewrite_model_name,omitempty"`
}

// ModelRateLimits are per-model rate limit overrides. When set, these take
// precedence over the API key's rate limits.
type ModelRateLimits struct {
	RPM *int `json:"rpm,omitempty"`
	TPM *int `json:"tpm,omitempty"`
}

// Backend represents a single vLLM backend instance serving a model.
type Backend struct {
	ID              string     `json:"id"`
	ModelID         string     `json:"model_id"`
	URL             string     `json:"url"`
	Weight          int        `json:"weight"`
	Active          bool       `json:"active"`
	Healthy         bool       `json:"healthy"`
	LastHealthCheck *time.Time `json:"last_health_check,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Scan helpers for JSONB columns ----------------------------------------

func (d *DefaultParams) Scan(src any) error {
	return scanJSON(src, d)
}

func (r *ReasoningConfig) Scan(src any) error {
	return scanJSON(src, r)
}

func (t *Transforms) Scan(src any) error {
	return scanJSON(src, t)
}

func (m *ModelRateLimits) Scan(src any) error {
	return scanJSON(src, m)
}

func scanJSON(src any, dest any) error {
	if src == nil {
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return nil
	}
	return json.Unmarshal(data, dest)
}
