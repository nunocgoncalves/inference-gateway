package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/config"
)

// Server is the main HTTP server for the gateway.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

// New creates a new Server with the provided configuration.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	router := newRouter(logger)

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
			Handler:      router,
			ReadTimeout:  cfg.Server.ReadTimeout,
			WriteTimeout: cfg.Server.WriteTimeout,
			IdleTimeout:  cfg.Server.IdleTimeout,
		},
		logger: logger,
	}
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
