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

	"github.com/nunocgoncalves/inference-gateway/internal/admin"
	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/config"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
	"github.com/nunocgoncalves/inference-gateway/internal/loadbalancer"
	"github.com/nunocgoncalves/inference-gateway/internal/metrics"
	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/proxy"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
	"github.com/nunocgoncalves/inference-gateway/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runServe()
	case "migrate":
		runMigrate()
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
	fmt.Fprintln(os.Stderr, "  migrate up         Run all pending migrations, then exit")
	fmt.Fprintln(os.Stderr, "  migrate down [N]   Roll back N migrations (default 1), then exit")
}

func runServe() {
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
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.Logging)
	ctx := context.Background()

	// --- Connect to PostgreSQL ---
	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	logger.Info("connected to database")

	// --- Connect to Redis ---
	redisOpts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		logger.Error("failed to parse redis URL", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	logger.Info("connected to redis")

	// --- Create stores ---
	registryStore := registry.NewPGStore(pool)
	authStore := auth.NewPGStore(pool)

	// --- Create rate limiter ---
	limiter := ratelimit.NewRedisLimiter(rdb)

	// --- Create registry cache ---
	cache := registry.NewCache(registryStore, rdb, logger, cfg.Registry.CacheRefreshInterval)
	if err := cache.Start(ctx); err != nil {
		logger.Error("failed to start registry cache", "error", err)
		os.Exit(1)
	}
	defer cache.Stop()
	logger.Info("registry cache started")

	// --- Create metrics ---
	m := metrics.New(prometheus.NewRegistry())

	// --- Create health checker ---
	hc := loadbalancer.NewHealthChecker(loadbalancer.HealthCheckConfig{
		Interval:           cfg.HealthCheck.Interval,
		Timeout:            cfg.HealthCheck.Timeout,
		HealthyThreshold:   cfg.HealthCheck.HealthyThreshold,
		UnhealthyThreshold: cfg.HealthCheck.UnhealthyThreshold,
	}, logger, nil, m)

	// --- Create handlers ---
	proxyHandler := proxy.NewHandler(cache, limiter, hc, m, logger)
	adminHandler := admin.NewHandler(registryStore, authStore, cache, logger)

	// --- Create server with all deps ---
	srv := server.New(cfg, logger, &server.Deps{
		ProxyHandler: proxyHandler,
		AdminHandler: adminHandler,
		AuthStore:    authStore,
		Limiter:      limiter,
		RateLimitCfg: middleware.RateLimitConfig{
			DefaultRPM: cfg.RateLimits.DefaultRPM,
			DefaultTPM: cfg.RateLimits.DefaultTPM,
		},
		AdminKey: cfg.Auth.AdminKey,
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
		logger.Error("server error", "error", err)
		os.Exit(1)
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig)
		if err := srv.Shutdown(30 * time.Second); err != nil {
			logger.Error("shutdown error", "error", err)
			os.Exit(1)
		}
	}
}

func runMigrate() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL environment variable is required")
		os.Exit(1)
	}

	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: gateway migrate <up|down> [N]")
		os.Exit(1)
	}

	direction := os.Args[2]

	switch direction {
	case "up":
		fmt.Println("running migrations up...")
		if err := database.MigrateUp(dbURL); err != nil {
			fmt.Fprintf(os.Stderr, "migration failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("migrations completed successfully")

	case "down":
		steps := 1
		if len(os.Args) > 3 {
			if _, err := fmt.Sscanf(os.Args[3], "%d", &steps); err != nil {
				fmt.Fprintf(os.Stderr, "invalid step count: %s\n", os.Args[3])
				os.Exit(1)
			}
		}
		fmt.Printf("rolling back %d migration(s)...\n", steps)
		if err := database.MigrateDown(dbURL, steps); err != nil {
			fmt.Fprintf(os.Stderr, "rollback failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("rollback completed successfully")

	default:
		fmt.Fprintf(os.Stderr, "unknown migrate direction: %s\n", direction)
		fmt.Fprintln(os.Stderr, "Usage: gateway migrate <up|down> [N]")
		os.Exit(1)
	}

	// Exit after migration — designed for init container usage.
}

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
