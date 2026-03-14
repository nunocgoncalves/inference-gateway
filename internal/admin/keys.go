package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	authpkg "github.com/nunocgoncalves/inference-gateway/internal/auth"
)

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// CreateKeyRequest is the request body for POST /admin/v1/keys.
type CreateKeyRequest struct {
	Name          string   `json:"name"`
	RPMLimit      *int     `json:"rpm_limit,omitempty"`
	TPMLimit      *int     `json:"tpm_limit,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	ExpiresAt     *string  `json:"expires_at,omitempty"` // RFC3339
}

// CreateKeyResponse includes the plaintext key — shown once, never again.
type CreateKeyResponse struct {
	Key    string         `json:"key"` // Plaintext — shown once
	APIKey authpkg.APIKey `json:"api_key"`
}

// UpdateKeyRequest is the request body for PUT /admin/v1/keys/{id}.
type UpdateKeyRequest struct {
	Name          *string  `json:"name,omitempty"`
	Active        *bool    `json:"active,omitempty"`
	RPMLimit      *int     `json:"rpm_limit,omitempty"`
	TPMLimit      *int     `json:"tpm_limit,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	ExpiresAt     *string  `json:"expires_at,omitempty"` // RFC3339, "null" to clear
}

// KeyResponse is used for GET/list — never includes the key hash.
type KeyResponse struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	KeyPrefix     string   `json:"key_prefix"`
	Active        bool     `json:"active"`
	RPMLimit      *int     `json:"rpm_limit,omitempty"`
	TPMLimit      *int     `json:"tpm_limit,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	ExpiresAt     *string  `json:"expires_at,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

func toKeyResponse(k *authpkg.APIKey) KeyResponse {
	resp := KeyResponse{
		ID:            k.ID,
		Name:          k.Name,
		KeyPrefix:     k.KeyPrefix,
		Active:        k.Active,
		RPMLimit:      k.RPMLimit,
		TPMLimit:      k.TPMLimit,
		AllowedModels: k.AllowedModels,
		CreatedAt:     k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:     k.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if k.ExpiresAt != nil {
		s := k.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
		resp.ExpiresAt = &s
	}
	return resp
}

// ---------------------------------------------------------------------------
// Key handlers
// ---------------------------------------------------------------------------

// RegisterKeyRoutes mounts the key management routes on the given chi router.
func (h *Handler) RegisterKeyRoutes(r chi.Router) {
	r.Get("/keys", h.ListKeys)
	r.Post("/keys", h.CreateKey)
	r.Get("/keys/{id}", h.GetKey)
	r.Put("/keys/{id}", h.UpdateKey)
	r.Delete("/keys/{id}", h.DeleteKey)
}

func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.keyStore.ListKeys(r.Context())
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to list keys", err)
		return
	}

	resp := make([]KeyResponse, len(keys))
	for i := range keys {
		resp[i] = toKeyResponse(&keys[i])
	}
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) CreateKey(w http.ResponseWriter, r *http.Request) {
	var req CreateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeValidationError(w, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		h.writeValidationError(w, "name is required")
		return
	}

	plaintext, hash, prefix, err := authpkg.GenerateKey()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to generate key", err)
		return
	}

	key := &authpkg.APIKey{
		Name:          strings.TrimSpace(req.Name),
		KeyHash:       hash,
		KeyPrefix:     prefix,
		Active:        true,
		RPMLimit:      req.RPMLimit,
		TPMLimit:      req.TPMLimit,
		AllowedModels: req.AllowedModels,
	}

	if req.ExpiresAt != nil {
		t, err := parseTime(*req.ExpiresAt)
		if err != nil {
			h.writeValidationError(w, "invalid expires_at format, use RFC3339")
			return
		}
		key.ExpiresAt = &t
	}

	if err := h.keyStore.CreateKey(r.Context(), key); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to create key", err)
		return
	}

	h.writeJSON(w, http.StatusCreated, map[string]any{
		"key":     plaintext,
		"api_key": toKeyResponse(key),
	})
}

func (h *Handler) GetKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	key, err := h.keyStore.GetKey(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "key")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to get key", err)
		return
	}
	h.writeJSON(w, http.StatusOK, toKeyResponse(key))
}

func (h *Handler) UpdateKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	existing, err := h.keyStore.GetKey(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "key")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to get key", err)
		return
	}

	var req UpdateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeValidationError(w, "invalid JSON body")
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			h.writeValidationError(w, "name cannot be empty")
			return
		}
		existing.Name = name
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}
	if req.RPMLimit != nil {
		existing.RPMLimit = req.RPMLimit
	}
	if req.TPMLimit != nil {
		existing.TPMLimit = req.TPMLimit
	}
	if req.AllowedModels != nil {
		existing.AllowedModels = req.AllowedModels
	}
	if req.ExpiresAt != nil {
		if *req.ExpiresAt == "null" || *req.ExpiresAt == "" {
			existing.ExpiresAt = nil
		} else {
			t, err := parseTime(*req.ExpiresAt)
			if err != nil {
				h.writeValidationError(w, "invalid expires_at format, use RFC3339")
				return
			}
			existing.ExpiresAt = &t
		}
	}

	if err := h.keyStore.UpdateKey(r.Context(), existing); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to update key", err)
		return
	}

	h.writeJSON(w, http.StatusOK, toKeyResponse(existing))
}

func (h *Handler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.keyStore.DeleteKey(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeNotFound(w, "key")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to delete key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
