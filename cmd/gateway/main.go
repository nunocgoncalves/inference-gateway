package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/config"
	"github.com/nunocgoncalves/inference-gateway/internal/database"
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

	srv := server.New(cfg, logger)

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
