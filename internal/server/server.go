package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nunocgoncalves/inference-gateway/internal/admin"
	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/config"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
)

// Deps holds all dependencies needed by the server to wire routing.
// If nil is passed to New, the server runs with stub handlers.
type Deps struct {
	ProxyHandler *proxy.Handler
	AdminHandler *admin.Handler
	AuthStore    auth.Store
	Limiter      ratelimit.Limiter
	RateLimitCfg middleware.RateLimitConfig
	AdminKey     string
}

// Server is the main HTTP server for the gateway.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
	metrics    *metrics.Metrics
}

// New creates a new Server with the provided configuration and dependencies.
// If deps is nil, the server runs with stub handlers (useful for minimal
// startup or testing the server package in isolation).
// If m is nil, a new Prometheus metrics registry is created automatically.
func New(cfg *config.Config, logger *slog.Logger, deps *Deps, m *metrics.Metrics) *Server {
	if m == nil {
		m = metrics.New(prometheus.NewRegistry())
	}
	router := newRouter(logger, m, deps)

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
			Handler:      router,
			ReadTimeout:  cfg.Server.ReadTimeout,
			WriteTimeout: cfg.Server.WriteTimeout,
			IdleTimeout:  cfg.Server.IdleTimeout,
		},
		logger:  logger,
		metrics: m,
	}
}

// Metrics returns the Prometheus metrics instance for use by other components.
func (s *Server) Metrics() *metrics.Metrics {
	return s.metrics
}

// Start begins listening for HTTP requests. It blocks until the server is
// shut down or encounters a fatal error.
func (s *Server) Start() error {
	s.logger.Info("starting server", "addr", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server listen: %w", err)
	}
	return nil
}

// Shutdown gracefully shuts down the server with a timeout.
func (s *Server) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s.logger.Info("shutting down server")
	return s.httpServer.Shutdown(ctx)
}
