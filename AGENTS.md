# AGENTS.md — Inference Gateway

This file provides guidance for AI coding agents operating in this repository.

## Project Overview

Go (1.25) OpenAI-compatible API gateway for vLLM inference backends. Routes
`/v1/chat/completions` and `/v1/models` through a load balancer with auth,
rate limiting, streaming (SSE), request transforms, and an admin CRUD API.

Key stack: Go stdlib + chi v5 router, PostgreSQL (pgx v5), Redis (go-redis v9),
Prometheus metrics, golang-migrate, testcontainers-go for integration tests.

## Build / Lint / Test Commands

```bash
# Build
make build                          # -> bin/gateway

# Lint (requires golangci-lint)
make lint                           # golangci-lint run ./...

# Run all tests (with race detector)
make test                           # go test -v -race -count=1 ./...

# Run a single test by name
go test -v -race -count=1 -run TestChatCompletions_NonStreaming ./internal/proxy/

# Run all tests in one package
go test -v -race -count=1 ./internal/loadbalancer/

# Clear test cache
go clean -testcache

# Database migrations
make migrate-up                     # bin/gateway migrate up
make migrate-down                   # bin/gateway migrate down

# Run with Docker Compose (postgres + redis + gateway)
docker compose up --build
```

**Important**: Integration tests use testcontainers-go and require Docker to
be running. They spin up real PostgreSQL and Redis containers — no mocks for
infrastructure. Tests may take 10-30s on first run while pulling images.

## Project Structure

```
cmd/gateway/main.go          Entry point — subcommands: serve, migrate
internal/
  admin/                     Admin REST API handlers + request/response types
  auth/                      API key generation (ml- prefix), SHA-256 hashing, store
  config/                    YAML + env var config loading and validation
  database/                  PostgreSQL pool + embedded SQL migrations
  loadbalancer/              Smooth weighted round-robin (Nginx algo) + health checks
  metrics/                   Prometheus metric definitions
  middleware/                Auth, logging, metrics, rate limiting, request-id
  proxy/                     Main proxy handler, SSE streaming, request transforms
  ratelimit/                 Redis sliding-window rate limiter (Lua scripts)
  registry/                  Model/Backend types, PostgreSQL CRUD store, Redis cache
  server/                    HTTP server setup + chi route wiring
```

## Code Style Guidelines

### Formatting

- Standard `gofmt` / `goimports` formatting. No custom formatter config.
- golangci-lint with default settings (no `.golangci.yml` config file).
- Line length: no strict limit, but keep lines reasonable (~100-120 chars).

### Imports

Three groups separated by blank lines, in this order:
1. Standard library (`fmt`, `net/http`, `log/slog`, etc.)
2. Third-party (`github.com/go-chi/chi/v5`, `github.com/redis/go-redis/v9`, etc.)
3. Internal (`github.com/nunocgoncalves/inference-gateway/internal/...`)

When aliasing imports, use short descriptive names:
```go
chimiddleware "github.com/go-chi/chi/v5/middleware"
gatewaymw     "github.com/nunocgoncalves/inference-gateway/internal/middleware"
```

### Types and Naming

- **Exported types**: PascalCase with doc comments (`// Handler is the main proxy handler.`).
- **Unexported types**: camelCase, used for internal state (e.g., `backendState`, `chatCompletionRequest`).
- **Constructors**: `NewXxx(deps...) *Xxx` pattern. Accept interfaces, return concrete types.
- **Sentinel errors**: package-level `var ErrXxx = errors.New(...)` (e.g., `ErrNoBackends`, `ErrAllUnhealthy`).
- **JSON struct tags**: snake_case with `omitempty` for optional fields.
- **Pointer fields for "not set" semantics**: Use `*float64`, `*int`, `*bool` when nil must be distinguished from zero value (see `DefaultParams`, `ReasoningConfig`).

### Error Handling

- Return errors up the call stack; let the handler decide the HTTP response.
- HTTP errors use OpenAI-compatible JSON format everywhere:
  ```json
  {"error": {"message": "...", "type": "invalid_request_error", "code": "..."}}
  ```
- Use `writeError(w, status, message, code)` helpers — don't write raw errors.
- Log errors with structured logging: `h.logger.Error("msg", "key", value)`.
- For infrastructure failures (backend down, DB error), report to health checker and return 502/503.

### Middleware Pattern

Chi-compatible middleware: `func(http.Handler) http.Handler`. Use context for
passing data between middleware and handlers (`auth.WithAPIKey`, `middleware.SetMetricsData`,
`middleware.RequestIDFromContext`).

### Logging

Use `log/slog` structured logger everywhere. Pass `*slog.Logger` via dependency injection.
Log with key-value pairs: `logger.Error("backend request failed", "backend", url, "error", err)`.

### Concurrency

- Protect shared state with `sync.Mutex` (see `WeightedRoundRobin`).
- Use `-race` flag in all test runs.
- Background goroutines (health checker, cache refresh, TPM batcher) accept a
  `context.Context` and stop on cancellation.

### Testing Patterns

- Use `github.com/stretchr/testify` for assertions (`assert`, `require`).
- `require` for fatal checks (setup, nil errors), `assert` for non-fatal.
- Integration tests: use `testcontainers-go` for real PostgreSQL and Redis.
  Test files contain helper functions like `setupTestDB(t)`, `setupTestRedis(t)`.
- Unit tests: use `httptest.NewServer` for mock HTTP backends.
- Test function naming: `TestMethodName_Scenario` (e.g., `TestChatCompletions_NonStreaming`).
- Use `t.Setenv()` for env vars, `t.TempDir()` for temp files.
- Tests use `-count=1` to disable caching.

### Dependency Injection

Constructor-based DI. The `server.Deps` struct aggregates all dependencies.
`cmd/gateway/main.go` wires everything together. No DI frameworks.

### Configuration

- YAML config file (`gateway.yaml`) with `os.ExpandEnv` for variable substitution.
- Env vars override YAML values (e.g., `GATEWAY_DATABASE_URL` overrides `database.url`).
- Validation in `config.Validate()` — required fields, sensible defaults.

### Database

- PostgreSQL via pgx v5 connection pool.
- Migrations via golang-migrate with embedded SQL files in `internal/database/migrations/`.
- JSONB columns for flexible nested config (DefaultParams, ReasoningConfig, etc.)
  with custom `Scan` methods.

### HTTP Responses

- Always set `Content-Type: application/json`.
- Use `json.NewEncoder(w).Encode(...)` for responses.
- Propagate `X-Request-ID` header across proxy requests.
- Rate limit headers: `X-RateLimit-Limit-RPM`, `X-RateLimit-Remaining-RPM`, etc.
