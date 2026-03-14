package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// Handler provides the admin REST API for managing models, backends, and API keys.
type Handler struct {
	store    registry.Store
	keyStore auth.Store
	cache    *registry.Cache
	logger   *slog.Logger
}

// NewHandler creates a new admin API handler.
func NewHandler(store registry.Store, keyStore auth.Store, cache *registry.Cache, logger *slog.Logger) *Handler {
	return &Handler{
		store:    store,
		keyStore: keyStore,
		cache:    cache,
		logger:   logger,
	}
}

// RegisterRoutes mounts the admin API routes on the given chi router.
// Admin auth middleware should be applied by the caller.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/models", h.ListModels)
	r.Post("/models", h.CreateModel)
	r.Get("/models/{id}", h.GetModel)
	r.Put("/models/{id}", h.UpdateModel)
	r.Delete("/models/{id}", h.DeleteModel)

	r.Post("/models/{id}/backends", h.CreateBackend)
	r.Put("/models/{id}/backends/{bid}", h.UpdateBackend)
	r.Delete("/models/{id}/backends/{bid}", h.DeleteBackend)
}

// ---------------------------------------------------------------------------
// Model handlers
// ---------------------------------------------------------------------------

func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.store.ListModels(r.Context(), false)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to list models", err)
		return
	}
	h.writeJSON(w, http.StatusOK, models)
}

func (h *Handler) CreateModel(w http.ResponseWriter, r *http.Request) {
	var req CreateModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeValidationError(w, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		h.writeValidationError(w, "name is required")
		return
	}
	if strings.TrimSpace(req.ModelID) == "" {
		h.writeValidationError(w, "model_id is required")
		return
	}

	model := &registry.Model{
		Name:    strings.TrimSpace(req.Name),
		ModelID: strings.TrimSpace(req.ModelID),
		Active:  true,
	}
	if req.Active != nil {
		model.Active = *req.Active
	}
	if req.DefaultParams != nil {
		model.DefaultParams = *req.DefaultParams
	}
	if req.ReasoningConfig != nil {
		model.ReasoningConfig = *req.ReasoningConfig
	}
	if req.Transforms != nil {
		model.Transforms = *req.Transforms
	}
	if req.RateLimits != nil {
		model.RateLimits = *req.RateLimits
	}

	if err := h.store.CreateModel(r.Context(), model); err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			h.writeValidationError(w, "a model with this name already exists")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to create model", err)
		return
	}

	h.invalidateCache(r)
	h.writeJSON(w, http.StatusCreated, model)
}

func (h *Handler) GetModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	model, err := h.store.GetModelByID(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "model")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to get model", err)
		return
	}
	h.writeJSON(w, http.StatusOK, model)
}

func (h *Handler) UpdateModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	existing, err := h.store.GetModelByID(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "model")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to get model", err)
		return
	}

	var req UpdateModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeValidationError(w, "invalid JSON body")
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			h.writeValidationError(w, "name cannot be empty")
			return
		}
		existing.Name = name
	}
	if req.ModelID != nil {
		modelID := strings.TrimSpace(*req.ModelID)
		if modelID == "" {
			h.writeValidationError(w, "model_id cannot be empty")
			return
		}
		existing.ModelID = modelID
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}
	if req.DefaultParams != nil {
		existing.DefaultParams = *req.DefaultParams
	}
	if req.ReasoningConfig != nil {
		existing.ReasoningConfig = *req.ReasoningConfig
	}
	if req.Transforms != nil {
		existing.Transforms = *req.Transforms
	}
	if req.RateLimits != nil {
		existing.RateLimits = *req.RateLimits
	}

	if err := h.store.UpdateModel(r.Context(), existing); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to update model", err)
		return
	}

	h.invalidateCache(r)
	h.writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) DeleteModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteModel(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "model")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to delete model", err)
		return
	}

	h.invalidateCache(r)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Backend handlers
// ---------------------------------------------------------------------------

func (h *Handler) CreateBackend(w http.ResponseWriter, r *http.Request) {
	modelID := chi.URLParam(r, "id")

	// Verify model exists.
	if _, err := h.store.GetModelByID(r.Context(), modelID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "model")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to verify model", err)
		return
	}

	var req CreateBackendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeValidationError(w, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.URL) == "" {
		h.writeValidationError(w, "url is required")
		return
	}
	if req.Weight <= 0 {
		h.writeValidationError(w, "weight must be greater than 0")
		return
	}

	backend := &registry.Backend{
		ModelID: modelID,
		URL:     strings.TrimSpace(req.URL),
		Weight:  req.Weight,
		Active:  true,
	}
	if req.Active != nil {
		backend.Active = *req.Active
	}

	if err := h.store.CreateBackend(r.Context(), backend); err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			h.writeValidationError(w, "a backend with this URL already exists for this model")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to create backend", err)
		return
	}

	h.invalidateCache(r)
	h.writeJSON(w, http.StatusCreated, backend)
}

func (h *Handler) UpdateBackend(w http.ResponseWriter, r *http.Request) {
	bid := chi.URLParam(r, "bid")

	// Load existing backends for the model to find the one we're updating.
	modelID := chi.URLParam(r, "id")
	backends, err := h.store.ListBackends(r.Context(), modelID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to list backends", err)
		return
	}

	var existing *registry.Backend
	for i := range backends {
		if backends[i].ID == bid {
			existing = &backends[i]
			break
		}
	}
	if existing == nil {
		h.writeNotFound(w, "backend")
		return
	}

	var req UpdateBackendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeValidationError(w, "invalid JSON body")
		return
	}

	if req.URL != nil {
		url := strings.TrimSpace(*req.URL)
		if url == "" {
			h.writeValidationError(w, "url cannot be empty")
			return
		}
		existing.URL = url
	}
	if req.Weight != nil {
		if *req.Weight <= 0 {
			h.writeValidationError(w, "weight must be greater than 0")
			return
		}
		existing.Weight = *req.Weight
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}

	if err := h.store.UpdateBackend(r.Context(), existing); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to update backend", err)
		return
	}

	h.invalidateCache(r)
	h.writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) DeleteBackend(w http.ResponseWriter, r *http.Request) {
	bid := chi.URLParam(r, "bid")
	if err := h.store.DeleteBackend(r.Context(), bid); err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "backend")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to delete backend", err)
		return
	}

	h.invalidateCache(r)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) invalidateCache(r *http.Request) {
	if h.cache != nil {
		if err := h.cache.Invalidate(r.Context()); err != nil {
			h.logger.Error("failed to invalidate cache", "error", err)
		}
	}
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message string, err error) {
	h.logger.Error(message, "error", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
		},
	})
}

func (h *Handler) writeValidationError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "validation_error",
		},
	})
}

func (h *Handler) writeNotFound(w http.ResponseWriter, resource string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": resource + " not found",
			"type":    "not_found",
		},
	})
}
