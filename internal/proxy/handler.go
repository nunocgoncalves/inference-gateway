package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/inference"
	"github.com/nunocgoncalves/inference-gateway/internal/inference/vllm"
	"github.com/nunocgoncalves/inference-gateway/internal/loadbalancer"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// Handler is the main proxy handler for OpenAI-compatible endpoints.
type Handler struct {
	cache     *registry.Cache
	balancers map[string]*loadbalancer.WeightedRoundRobin // keyed by model name
	limiter   ratelimit.Limiter
	hc        *loadbalancer.HealthChecker
	metrics   *metrics.Metrics
	executor  inference.Executor
	logger    *slog.Logger
}

// NewHandler creates a new proxy handler.
func NewHandler(
	cache *registry.Cache,
	limiter ratelimit.Limiter,
	hc *loadbalancer.HealthChecker,
	m *metrics.Metrics,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		cache:     cache,
		balancers: make(map[string]*loadbalancer.WeightedRoundRobin),
		limiter:   limiter,
		hc:        hc,
		metrics:   m,
		executor:  vllm.NewExecutor(nil),
		logger:    logger,
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

	// Store metrics data in context for the metrics middleware.
	md := &middleware.MetricsData{Model: req.Model, Streaming: req.Stream}
	ctx := middleware.SetMetricsData(r.Context(), md)
	r = r.WithContext(ctx)

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

	// Enrich metrics/logging context data with the selected backend.
	md.BackendURL = backend.URL

	// Resolve the engine-agnostic model target. The current standalone gateway
	// maps public aliases directly to backend model IDs; tenant/adapter routing
	// will populate adapter fields from local policy snapshots.
	target := modelTargetFromRegistryModel(model, req.Model)

	// Apply request transforms.
	body = ApplyRequestTransforms(body, model)
	body = rewriteModelInRequest(body, target)

	// Forward to backend through the data-plane executor boundary.
	replica := replicaTargetFromBackend(backend, target)
	if req.Stream {
		h.handleStreaming(w, r, body, backend, model, target, replica)
	} else {
		h.handleNonStreaming(w, r, body, backend, model, target, replica)
	}
}

func (h *Handler) handleNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	backend *registry.Backend,
	model *registry.Model,
	target inference.ModelTarget,
	replica inference.ReplicaTarget,
) {
	backendStart := time.Now()
	resp, err := h.executor.ChatCompletion(r.Context(), inference.ExecutionRequest{
		Body:    body,
		Headers: executionHeaders(r),
		Target:  target,
		Replica: replica,
	})
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
	backendDuration := time.Since(backendStart).Seconds()

	// Record backend request duration.
	if h.metrics != nil {
		h.metrics.BackendRequestDuration.WithLabelValues(model.Name, backend.URL).Observe(backendDuration)
	}

	// Rewrite model name in response back to the alias.
	respBody := rewriteModelInResponse(resp.Body, model.Name)

	// Update TPM from usage in response and record token metrics.
	h.trackUsage(r.Context(), respBody, model.Name, backendDuration)

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

// trackUsage extracts token usage from a non-streaming response, increments
// the TPM counter, and records Prometheus token metrics.
func (h *Handler) trackUsage(ctx context.Context, respBody []byte, modelName string, durationSec float64) {
	var resp struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil || resp.Usage == nil {
		return
	}

	// Rate limit TPM tracking.
	apiKey := auth.APIKeyFromContext(ctx)
	if apiKey != nil && h.limiter != nil && resp.Usage.TotalTokens > 0 {
		h.limiter.IncrementTPM(ctx, apiKey.ID, resp.Usage.TotalTokens)
	}

	// Prometheus token counters and tokens/s histograms.
	if h.metrics != nil {
		if resp.Usage.PromptTokens > 0 {
			h.metrics.PromptTokensTotal.WithLabelValues(modelName).Add(float64(resp.Usage.PromptTokens))
		}
		if resp.Usage.CompletionTokens > 0 {
			h.metrics.CompletionTokensTotal.WithLabelValues(modelName).Add(float64(resp.Usage.CompletionTokens))
		}
		if durationSec > 0 {
			if resp.Usage.PromptTokens > 0 {
				h.metrics.TokensPerSecond.WithLabelValues(modelName, "prompt").
					Observe(float64(resp.Usage.PromptTokens) / durationSec)
			}
			if resp.Usage.CompletionTokens > 0 {
				h.metrics.TokensPerSecond.WithLabelValues(modelName, "completion").
					Observe(float64(resp.Usage.CompletionTokens) / durationSec)
			}
		}
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

// modelTargetFromRegistryModel adapts the current standalone model registry to
// the engine-agnostic target shape. Future M5 routing will resolve this from
// organization-scoped policy/routing snapshots instead of only model aliases.
func modelTargetFromRegistryModel(model *registry.Model, publicModelName string) inference.ModelTarget {
	return inference.ModelTarget{
		PublicModelName:  publicModelName,
		BaseModelID:      model.ModelID,
		BackendModelName: model.ModelID,
	}
}

func replicaTargetFromBackend(backend *registry.Backend, target inference.ModelTarget) inference.ReplicaTarget {
	return inference.ReplicaTarget{
		ID:            backend.ID,
		URL:           backend.URL,
		BackendPoolID: target.BackendPoolID,
		EngineType:    inference.EngineVLLM,
	}
}

func executionHeaders(r *http.Request) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if v := middleware.RequestIDFromContext(r.Context()); v != "" {
		headers.Set("X-Request-ID", v)
	}
	return headers
}

// rewriteModelInRequest replaces the "model" field in the JSON body with the
// engine-specific internal model/LoRA name from the resolved ModelTarget.
func rewriteModelInRequest(body []byte, target inference.ModelTarget) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	parsed["model"] = target.EngineModelName()
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
