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

	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

// Handler is the main proxy handler for OpenAI-compatible endpoints. It routes
// by alias (from the control-plane catalog snapshot) to a single backend_url,
// rewrites the model field to the backend's HF id, applies per-alias transforms,
// and proxies. No weighted LB / health checker — the catalog's `available` flag
// is the health signal (set by the control-plane ModelBackend reconciler).
type Handler struct {
	cache   snapshot.Reader
	limiter ratelimit.Limiter
	metrics *metrics.Metrics
	client  *http.Client
	logger  *slog.Logger
}

// NewHandler creates a new proxy handler.
func NewHandler(cache snapshot.Reader, limiter ratelimit.Limiter, m *metrics.Metrics, logger *slog.Logger) *Handler {
	return &Handler{
		cache:   cache,
		limiter: limiter,
		metrics: m,
		client: &http.Client{
			Timeout: 300 * time.Second, // Long timeout for inference
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}

	identityID := middleware.IdentityIDFromContext(r.Context())

	md := &middleware.MetricsData{Model: req.Model, Streaming: req.Stream}
	ctx := middleware.SetMetricsData(r.Context(), md)
	r = r.WithContext(ctx)

	// Capability check: broad-default wildcard allows all; model access is a
	// capability resource "model:<alias>" / "model:*" (v1 broad-default = all).
	if !h.allowsModel(identityID, req.Model) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("identity is not allowed to access model '%s'", req.Model),
			"model_not_allowed")
		return
	}

	entry, ok := h.cache.CatalogEntry(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model '%s' not found", req.Model), "model_not_found")
		return
	}
	if !entry.Available {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("model '%s' is not available", req.Model), "service_unavailable")
		return
	}

	md.BackendURL = entry.BackendURL

	body = ApplyRequestTransforms(body, &entry)
	if entry.Transforms.RewriteModelName && entry.BackendModelID != "" {
		body = rewriteModelInRequest(body, entry.BackendModelID)
	}

	if req.Stream {
		h.handleStreaming(w, r, body, &entry, identityID)
	} else {
		h.handleNonStreaming(w, r, body, &entry, identityID)
	}
}

// allowsModel reports whether the identity's capabilities grant access to the
// model. Wildcard '*' / 'model:*' allow any; 'model:<alias>' allows one.
func (h *Handler) allowsModel(identityID, model string) bool {
	for _, c := range h.cache.Capabilities(identityID) {
		if c.Resource == "*" || c.Resource == "model:*" {
			return true
		}
		if c.Resource == "model:"+model {
			return true
		}
	}
	return false
}

func (h *Handler) handleNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	entry *snapshot.CatalogEntry,
	identityID string,
) {
	backendURL := fmt.Sprintf("%s/v1/chat/completions", entry.BackendURL)

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, backendURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create proxy request", "server_error")
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	if v := middleware.RequestIDFromContext(r.Context()); v != "" {
		proxyReq.Header.Set("X-Request-ID", v)
	}

	backendStart := time.Now()
	resp, err := h.client.Do(proxyReq)
	if err != nil {
		h.logger.Error("backend request failed", "backend", entry.BackendURL, "model", entry.ModelID, "error", err)
		writeError(w, http.StatusBadGateway, "backend request failed", "backend_error")
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	backendDuration := time.Since(backendStart).Seconds()
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read backend response", "backend_error")
		return
	}
	if h.metrics != nil {
		h.metrics.BackendRequestDuration.WithLabelValues(entry.ModelID, entry.BackendURL).Observe(backendDuration)
	}

	// Rewrite the response model field back to the alias.
	respBody = rewriteModelInResponse(respBody, entry.ModelID)
	h.trackUsage(r.Context(), respBody, entry.ModelID, backendDuration, identityID)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		h.logger.Error("failed to write backend response", "model", entry.ModelID, "error", err)
	}
}

// trackUsage extracts token usage from a non-streaming response, increments the
// per-identity TPM counter, and records Prometheus token metrics.
func (h *Handler) trackUsage(ctx context.Context, respBody []byte, modelName string, durationSec float64, identityID string) {
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

	if h.limiter != nil && resp.Usage.TotalTokens > 0 && identityID != "" {
		if err := h.limiter.IncrementTPM(ctx, identityID, resp.Usage.TotalTokens); err != nil {
			h.logger.Error("failed to increment TPM", "model", modelName, "error", err)
		}
	}

	if h.metrics != nil {
		if resp.Usage.PromptTokens > 0 {
			h.metrics.PromptTokensTotal.WithLabelValues(modelName).Add(float64(resp.Usage.PromptTokens))
			h.metrics.TokensPerRequest.WithLabelValues(modelName, "prompt").Observe(float64(resp.Usage.PromptTokens))
		}
		if resp.Usage.CompletionTokens > 0 {
			h.metrics.CompletionTokensTotal.WithLabelValues(modelName).Add(float64(resp.Usage.CompletionTokens))
			h.metrics.TokensPerRequest.WithLabelValues(modelName, "completion").Observe(float64(resp.Usage.CompletionTokens))
		}
		if durationSec > 0 {
			if resp.Usage.PromptTokens > 0 {
				h.metrics.TokensPerSecond.WithLabelValues(modelName, "prompt").Observe(float64(resp.Usage.PromptTokens) / durationSec)
			}
			if resp.Usage.CompletionTokens > 0 {
				completionTokensPerSecond := float64(resp.Usage.CompletionTokens) / durationSec
				h.metrics.TokensPerSecond.WithLabelValues(modelName, "completion").Observe(completionTokensPerSecond)
				h.metrics.CompletionTokensPerSecondByPromptBucket.
					WithLabelValues(modelName, promptTokensBucket(resp.Usage.PromptTokens)).
					Observe(completionTokensPerSecond)
			}
		}
	}
}

// ListModels handles GET /v1/models — returns available aliases from the snapshot.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	entries := h.cache.ListCatalog()
	data := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !e.Available {
			continue
		}
		data = append(data, map[string]any{
			"id":       e.ModelID,
			"object":   "model",
			"created":  0,
			"owned_by": "inference-gateway",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data}); err != nil {
		h.logger.Error("failed to encode models response", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Request/Response helpers
// ---------------------------------------------------------------------------

type chatCompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// rewriteModelInRequest replaces the "model" field with the backend's HF id.
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

// rewriteModelInResponse replaces the "model" field with the alias so the client
// sees a consistent name.
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
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	}); err != nil {
		slog.Error("failed to write error response", "error", err)
	}
}
