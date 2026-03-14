package admin

import (
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// ---------------------------------------------------------------------------
// Request / Response types for the Admin API
// ---------------------------------------------------------------------------

// CreateModelRequest is the request body for POST /admin/v1/models.
type CreateModelRequest struct {
	Name            string                    `json:"name"`
	ModelID         string                    `json:"model_id"`
	Active          *bool                     `json:"active,omitempty"`
	DefaultParams   *registry.DefaultParams   `json:"default_params,omitempty"`
	ReasoningConfig *registry.ReasoningConfig `json:"reasoning_config,omitempty"`
	Transforms      *registry.Transforms      `json:"transforms,omitempty"`
	RateLimits      *registry.ModelRateLimits `json:"rate_limits,omitempty"`
}

// UpdateModelRequest is the request body for PUT /admin/v1/models/{id}.
type UpdateModelRequest struct {
	Name            *string                   `json:"name,omitempty"`
	ModelID         *string                   `json:"model_id,omitempty"`
	Active          *bool                     `json:"active,omitempty"`
	DefaultParams   *registry.DefaultParams   `json:"default_params,omitempty"`
	ReasoningConfig *registry.ReasoningConfig `json:"reasoning_config,omitempty"`
	Transforms      *registry.Transforms      `json:"transforms,omitempty"`
	RateLimits      *registry.ModelRateLimits `json:"rate_limits,omitempty"`
}

// CreateBackendRequest is the request body for POST /admin/v1/models/{id}/backends.
type CreateBackendRequest struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
	Active *bool  `json:"active,omitempty"`
}

// UpdateBackendRequest is the request body for PUT /admin/v1/models/{id}/backends/{bid}.
type UpdateBackendRequest struct {
	URL    *string `json:"url,omitempty"`
	Weight *int    `json:"weight,omitempty"`
	Active *bool   `json:"active,omitempty"`
}
