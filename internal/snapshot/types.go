// Package snapshot is the gateway's read-only view of control-plane state
// (catalog, API keys, capabilities, rate limits), consumed directly from the
// shared Postgres. The gateway owns its cache + freshness via LISTEN/NOTIFY
// (HOR-247). No request-path calls to control-plane.
package snapshot

// CatalogEntry is a row of catalog.effective_catalog — the contract the gateway
// routes on: alias -> backend_url (+ backend_model_id for the model-field
// rewrite) + per-alias config, filtered to available rows.
type CatalogEntry struct {
	ModelID         string          `json:"model_id"` // client-facing alias
	DisplayName     string          `json:"display_name"`
	ContextLength   int             `json:"context_length"`
	Capabilities    []string        `json:"capabilities"`
	BackendRef      string          `json:"backend_ref"`
	BackendKind     string          `json:"backend_kind"`
	BackendModelID  string          `json:"backend_model_id"` // HuggingFace id; the gateway rewrites the alias -> this
	BackendURL      string          `json:"backend_url"`
	DefaultParams   DefaultParams   `json:"default_params"`
	ReasoningConfig ReasoningConfig `json:"reasoning_config"`
	Transforms      Transforms      `json:"transforms"`
	RateLimits      ModelRateLimits `json:"rate_limits"`
	Available       bool            `json:"available"`
}

// DefaultParams are default sampling parameters injected when the client omits
// them. Pointer fields keep "not set" distinct from zero.
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

// ReasoningConfig controls reasoning/thinking behaviour (forwarded to vLLM as
// chat_template_kwargs.enable_thinking).
type ReasoningConfig struct {
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}

// Transforms define request/response transformation rules per alias.
type Transforms struct {
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty"`
	RewriteModelName   bool   `json:"rewrite_model_name,omitempty"`
}

// ModelRateLimits are per-alias rate-limit overrides (from the catalog).
type ModelRateLimits struct {
	RPM *int `json:"rpm,omitempty"`
	TPM *int `json:"tpm,omitempty"`
}

// APIKey is an active API key (hash -> identity) from identity.api_keys.
type APIKey struct {
	KeyHash    string
	IdentityID string
}

// Capability is a row of permissions.effective_capabilities: an identity is
// granted (resource, action). Wildcard '*' matches any.
type Capability struct {
	IdentityID string
	Resource   string
	Action     string
}

// IdentityRateLimits is a per-identity throughput limit (from
// permissions.effective_rate_limits). The gateway enforces via Redis.
type IdentityRateLimits struct {
	IdentityID string
	RPM        int
	TPM        int
}
