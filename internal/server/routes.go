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

func newRouter(logger *slog.Logger, m *metrics.Metrics, deps *Deps) http.Handler {
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

	if deps != nil {
		// OpenAI-compatible endpoints — fully wired.
		r.Route("/v1", func(r chi.Router) {
			if deps.AuthStore != nil {
				r.Use(gatewaymw.Auth(deps.AuthStore, logger))
			}
			if deps.Limiter != nil {
				r.Use(gatewaymw.RateLimit(deps.Limiter, deps.RateLimitCfg, m, logger))
			}
			if m != nil {
				r.Use(gatewaymw.Metrics(m))
			}
			r.Get("/models", deps.ProxyHandler.ListModels)
			r.Post("/chat/completions", deps.ProxyHandler.ChatCompletions)
		})

		// Admin endpoints — fully wired.
		r.Route("/admin/v1", func(r chi.Router) {
			if deps.AdminKey != "" {
				r.Use(gatewaymw.AdminAuth(deps.AdminKey, logger))
			}
			r.Get("/health", healthHandler)
			deps.AdminHandler.RegisterRoutes(r)
			deps.AdminHandler.RegisterKeyRoutes(r)
		})
	} else {
		// Stub mode — no dependencies wired.
		r.Route("/v1", func(r chi.Router) {
			r.Get("/models", notImplementedHandler)
			r.Post("/chat/completions", notImplementedHandler)
		})
		r.Route("/admin/v1", func(r chi.Router) {
			r.Get("/health", healthHandler)
		})
	}

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
