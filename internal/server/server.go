package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nunocgoncalves/inference-gateway/internal/config"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
	"github.com/nunocgoncalves/inference-gateway/internal/workload"
)

// Deps holds all dependencies needed by the server to wire routing.
// If nil is passed to New, the server runs with stub handlers.
type Deps struct {
	ProxyHandler       *proxy.Handler
	Cache              snapshot.Reader
	Limiter            ratelimit.Limiter
	AdminKey           string
	ReadinessStaleness time.Duration // /readyz is unhealthy if the snapshot is older than this
	// Workload path (HOR-398). When WorkloadStore is non-nil AND a TLS config is
	// supplied via NewWorkloadTLS, the gateway also serves the OpenAI-compatible
	// endpoints on a second mTLS HTTP/2 listener with supervisor workload auth.
	WorkloadStore workload.Store
	TrustDomain   string
}

// Server is the main HTTP server for the gateway. It always serves the
// API-key listener; when a workload TLS config is attached it also serves the
// supervisor mTLS listener.
type Server struct {
	httpServer     *http.Server
	workloadServer *http.Server // nil when the workload path is disabled
	logger         *slog.Logger
	metrics        *metrics.Metrics
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

// AttachWorkload wires the supervisor mTLS workload listener (HOR-398). The
// workload router reuses the existing proxy handler behind the WorkloadAuth
// middleware; the tlsConfig must be an h2 + RequireAndVerifyClientCert config
// built by spiffe.ServerTLSConfig. When deps.WorkloadStore is nil this is a
// no-op (the workload path stays disabled).
func (s *Server) AttachWorkload(cfg *config.Config, deps *Deps, tlsConfig *tls.Config) {
	if deps == nil || deps.WorkloadStore == nil || tlsConfig == nil {
		return
	}
	router := newWorkloadRouter(s.logger, s.metrics, deps)
	// HTTP/2 only — explicitly disable HTTP/1.1. Setting NextProtos=["h2"]
	// alone is NOT sufficient: with Protocols nil, Go still serves HTTP/1.1 to a
	// TLS client that sends no ALPN. ARCH-006 requires the workload listener to
	// be HTTP/2-only, so a non-h2 client must be rejected.
	protocols := &http.Protocols{}
	protocols.SetHTTP2(true)
	s.workloadServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Workload.Port),
		Handler:      router,
		TLSConfig:    tlsConfig,
		ReadTimeout:  cfg.Workload.ReadTimeout,
		WriteTimeout: cfg.Workload.WriteTimeout,
		IdleTimeout:  cfg.Workload.IdleTimeout,
		Protocols:    protocols,
	}
}

// Metrics returns the Prometheus metrics instance for use by other components.
func (s *Server) Metrics() *metrics.Metrics {
	return s.metrics
}

// Start begins listening for HTTP requests. It blocks until the server is
// shut down or encounters a fatal error. If the workload mTLS listener is
// attached, it is started in a goroutine; an error on either listener is
// surfaced to the caller.
func (s *Server) Start() error {
	errCh := make(chan error, 2)
	if s.workloadServer != nil {
		s.logger.Info("starting workload mTLS server", "addr", s.workloadServer.Addr)
		go func() {
			// ListenAndServeTLS with a TLSConfig that carries the cert pair;
			// cert/key args are empty because the certs live in TLSConfig.
			err := s.workloadServer.ListenAndServeTLS("", "")
			if err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("workload server listen: %w", err)
				return
			}
			errCh <- nil
		}()
	}
	s.logger.Info("starting server", "addr", s.httpServer.Addr)
	go func() {
		err := s.httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("server listen: %w", err)
			return
		}
		errCh <- nil
	}()

	// Block until either listener errors (or both finish on shutdown). Return
	// the first non-nil error; on clean shutdown both send nil.
	listeners := 1
	if s.workloadServer != nil {
		listeners = 2
	}
	var firstErr error
	for i := 0; i < listeners; i++ {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
			// Best-effort: stop the other listener so Start returns.
			_ = s.httpServer.Close()
			if s.workloadServer != nil {
				_ = s.workloadServer.Close()
			}
		}
	}
	return firstErr
}

// Shutdown gracefully shuts down all listeners with a timeout.
func (s *Server) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s.logger.Info("shutting down server")
	var err error
	if err = s.httpServer.Shutdown(ctx); err == nil && s.workloadServer != nil {
		err = s.workloadServer.Shutdown(ctx)
	}
	return err
}
