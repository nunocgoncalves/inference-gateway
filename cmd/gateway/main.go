package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/nunocgoncalves/inference-gateway/internal/config"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/server"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		if err := runServe(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: gateway <command> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  serve              Start the API gateway server")
}

func runServe() error {
	configPath := ""
	if len(os.Args) > 2 {
		configPath = os.Args[2]
	}
	// Also check -config flag style.
	for i, arg := range os.Args {
		if arg == "-config" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	logger := newLogger(cfg.Logging)
	ctx := context.Background()

	// --- Connect to PostgreSQL ---
	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer pool.Close()
	logger.Info("connected to database")

	// --- Connect to Redis ---
	redisOpts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return fmt.Errorf("failed to parse redis URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to redis: %w", err)
	}
	logger.Info("connected to redis")

	// --- Create rate limiter (Redis — the only shared state, for rate-limit counters) ---
	limiter := ratelimit.NewRedisLimiter(rdb)

	// --- Create snapshot cache (in-memory, per-pod; LISTEN/NOTIFY-synced) ---
	cache := snapshot.NewCache(snapshot.NewPGStore(pool), cfg.Database.URL, logger, cfg.Snapshot.RefreshInterval)
	if err := cache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start snapshot cache: %w", err)
	}
	defer cache.Stop()
	logger.Info("snapshot cache started")

	// --- Create metrics ---
	m := metrics.New(prometheus.NewRegistry())

	// --- Create handlers ---
	proxyHandler := proxy.NewHandler(cache, limiter, m, logger)

	// --- Create server with all deps ---
	srv := server.New(cfg, logger, &server.Deps{
		ProxyHandler:       proxyHandler,
		Cache:              cache,
		Limiter:            limiter,
		AdminKey:           cfg.Auth.AdminKey,
		ReadinessStaleness: cfg.Snapshot.ReadinessStaleness,
	}, m)

	// Start server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Wait for interrupt or server error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig)
		if err := srv.Shutdown(30 * time.Second); err != nil {
			return fmt.Errorf("shutdown error: %w", err)
		}
		return nil
	}
}

// runMigrate is removed — the gateway owns no tables; the control-plane's
// migrate job creates the schemas (identity, permissions, catalog) the gateway
// reads. There is no `migrate` subcommand.

func newLogger(cfg config.LoggingConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
