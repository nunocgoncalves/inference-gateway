package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	gatewaymw "github.com/nunocgoncalves/inference-gateway/internal/middleware"
)

func newRouter(logger *slog.Logger, m *metrics.Metrics) http.Handler {
	r := chi.NewRouter()

	// Base middleware — order matters.
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.RealIP)
	r.Use(gatewaymw.RequestID)
	r.Use(gatewaymw.Logging(logger))

	// Health check — no auth required.
	r.Get("/health", healthHandler)

	// Prometheus metrics endpoint.
	if m != nil {
		r.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	}

	// OpenAI-compatible endpoints (auth + rate limiting will be added later).
	r.Route("/v1", func(r chi.Router) {
		r.Get("/models", notImplementedHandler)
		r.Post("/chat/completions", notImplementedHandler)
	})

	// Admin endpoints (admin auth will be added later).
	r.Route("/admin/v1", func(r chi.Router) {
		r.Get("/health", healthHandler)
	})

	return r
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func notImplementedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "not implemented",
			"type":    "not_implemented_error",
			"code":    "not_implemented",
		},
	})
}
