package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/loadbalancer"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// Handler is the main proxy handler for OpenAI-compatible endpoints.
type Handler struct {
	cache     *registry.Cache
	balancers map[string]*loadbalancer.WeightedRoundRobin // keyed by model name
	limiter   ratelimit.Limiter
	hc        *loadbalancer.HealthChecker
	client    *http.Client
	logger    *slog.Logger
}

// NewHandler creates a new proxy handler.
func NewHandler(
	cache *registry.Cache,
	limiter ratelimit.Limiter,
	hc *loadbalancer.HealthChecker,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		cache:     cache,
		balancers: make(map[string]*loadbalancer.WeightedRoundRobin),
		limiter:   limiter,
		hc:        hc,
		client: &http.Client{
			Timeout: 300 * time.Second, // Long timeout for inference
			// Don't follow redirects.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// GetOrCreateBalancer returns the LB for a model, creating one if needed.
func (h *Handler) GetOrCreateBalancer(model *registry.Model) *loadbalancer.WeightedRoundRobin {
	if lb, ok := h.balancers[model.Name]; ok {
		lb.UpdateBackends(model.Backends)
		return lb
	}
	lb := loadbalancer.New(model.Backends)
	h.balancers[model.Name] = lb
	return lb
}

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Read the request body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	// Parse to extract model name and stream flag.
	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}

	// Check model access for the authenticated key.
	apiKey := auth.APIKeyFromContext(r.Context())
	if apiKey != nil && !apiKey.IsModelAllowed(req.Model) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("API key does not have access to model '%s'", req.Model),
			"model_not_allowed")
		return
	}

	// Look up model in registry.
	model := h.cache.GetModelByName(r.Context(), req.Model)
	if model == nil {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("model '%s' not found", req.Model),
			"model_not_found")
		return
	}

	// Select backend.
	lb := h.GetOrCreateBalancer(model)
	backend, err := lb.Next()
	if err != nil {
		if err == loadbalancer.ErrAllUnhealthy {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("all backends for model '%s' are unhealthy", req.Model),
				"service_unavailable")
		} else {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("no backends configured for model '%s'", req.Model),
				"service_unavailable")
		}
		return
	}

	// Apply request transforms: rewrite model name to actual vLLM model ID.
	body = rewriteModelInRequest(body, model.ModelID)

	// Forward to backend.
	if req.Stream {
		h.handleStreaming(w, r, body, backend, model)
	} else {
		h.handleNonStreaming(w, r, body, backend, model)
	}
}

func (h *Handler) handleNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	backend *registry.Backend,
	model *registry.Model,
) {
	backendURL := fmt.Sprintf("%s/v1/chat/completions", backend.URL)

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, backendURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create proxy request", "server_error")
		return
	}

	// Copy relevant headers.
	proxyReq.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("X-Request-ID"); v != "" {
		proxyReq.Header.Set("X-Request-ID", v)
	}

	resp, err := h.client.Do(proxyReq)
	if err != nil {
		h.logger.Error("backend request failed",
			"backend", backend.URL, "model", model.Name, "error", err)
		// Report passive health failure.
		if h.hc != nil {
			h.hc.ReportFailure(backend.ID)
		}
		writeError(w, http.StatusBadGateway, "backend request failed", "backend_error")
		return
	}
	defer resp.Body.Close()

	// Read the backend response.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read backend response", "backend_error")
		return
	}

	// Rewrite model name in response back to the alias.
	respBody = rewriteModelInResponse(respBody, model.Name)

	// Update TPM from usage in response.
	h.trackUsage(r.Context(), respBody)

	// Copy response headers.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// handleStreaming is a placeholder — implemented in Ticket 8.
func (h *Handler) handleStreaming(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	backend *registry.Backend,
	model *registry.Model,
) {
	writeError(w, http.StatusNotImplemented, "streaming not yet implemented", "not_implemented")
}

// trackUsage extracts token usage from a non-streaming response and increments
// the TPM counter.
func (h *Handler) trackUsage(ctx context.Context, respBody []byte) {
	apiKey := auth.APIKeyFromContext(ctx)
	if apiKey == nil || h.limiter == nil {
		return
	}

	var resp struct {
		Usage *struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil || resp.Usage == nil {
		return
	}

	if resp.Usage.TotalTokens > 0 {
		h.limiter.IncrementTPM(ctx, apiKey.ID, resp.Usage.TotalTokens)
	}
}

// ListModels handles GET /v1/models — returns models from the registry
// (not proxied to vLLM). Filtered by the key's allowed_models.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	models := h.cache.ListModels(r.Context())

	// Filter by key's allowed models.
	apiKey := auth.APIKeyFromContext(r.Context())
	if apiKey != nil && len(apiKey.AllowedModels) > 0 {
		filtered := make([]registry.Model, 0)
		for _, m := range models {
			if apiKey.IsModelAllowed(m.Name) {
				filtered = append(filtered, m)
			}
		}
		models = filtered
	}

	// Build OpenAI-compatible response.
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.Name,
			"object":   "model",
			"created":  m.CreatedAt.Unix(),
			"owned_by": "inference-gateway",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

// ---------------------------------------------------------------------------
// Request/Response helpers
// ---------------------------------------------------------------------------

// chatCompletionRequest is a minimal parse of the request — just enough to
// extract the model name and stream flag.
type chatCompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// rewriteModelInRequest replaces the "model" field in the JSON body with
// the actual vLLM model ID.
func rewriteModelInRequest(body []byte, modelID string) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	parsed["model"] = modelID
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

// rewriteModelInResponse replaces the "model" field in the response with
// the alias name so the client sees a consistent model name.
func rewriteModelInResponse(body []byte, aliasName string) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	if _, ok := parsed["model"]; ok {
		parsed["model"] = aliasName
	}
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

func writeError(w http.ResponseWriter, status int, message string, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}
