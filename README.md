# Inference Gateway

An OpenAI-compatible API gateway written in Go that sits between clients and [vLLM](https://github.com/vllm-project/vllm) inference engine instances. It provides a model registry, request routing, weighted round-robin load balancing with health checks, API key authentication, rate limiting (RPM + TPM), SSE streaming support, and an admin API for runtime management.

```
Clients ──> [ Auth + Rate Limit ] ──> [ Route + LB ] ──> [ Proxy + Transform ] ──> vLLM backends
                                            |
                                   [ PostgreSQL + Redis ]
```

## Features

- **OpenAI-compatible API** -- drop-in replacement for `/v1/chat/completions` and `/v1/models`
- **Model registry** -- register models with aliases, map to actual vLLM model IDs
- **Weighted round-robin load balancing** -- distribute traffic across multiple vLLM backends per model
- **Active + passive health checks** -- automatic backend failover
- **API key authentication** -- per-key model access control, RPM/TPM limits, expiration
- **Rate limiting** -- sliding window RPM + TPM via Redis sorted sets, fail-open on Redis errors
- **SSE streaming** -- zero-buffering proxy with live TPM tracking via `continuous_usage_stats`
- **Request transforms** -- default params injection, reasoning config override, system prompt prefix
- **Admin API** -- full CRUD for models, backends, and API keys at runtime
- **Prometheus metrics** -- TTFT, ITL, tokens/s, backend health, active streams, rate limit hits
- **Structured logging** -- JSON request logs with request ID, model, key prefix, backend URL, duration
- **Redis-backed cache** -- shared across gateway pods, periodic refresh + immediate invalidation on admin mutations

## Quick Start

### Prerequisites

- Go 1.25+
- Docker and Docker Compose
- Make (optional)

### 1. Start infrastructure

```bash
docker compose up -d postgres redis
```

### 2. Run database migrations

```bash
export DATABASE_URL="postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable"
go run ./cmd/gateway migrate up
```

### 3. Start the gateway

```bash
export DATABASE_URL="postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable"
export REDIS_URL="redis://localhost:6379/0"
export ADMIN_API_KEY="your-admin-secret"
go run ./cmd/gateway serve
```

The gateway is now listening on `:8080`.

### 4. Register a model via the admin API

```bash
# Create a model
curl -s -X POST http://localhost:8080/admin/v1/models \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llama-3",
    "model_id": "meta-llama/Llama-3.1-70B-Instruct"
  }' | jq .

# Add a backend (pointing to your vLLM instance)
MODEL_ID=$(curl -s http://localhost:8080/admin/v1/models \
  -H "X-Admin-Key: your-admin-secret" | jq -r '.[0].id')

curl -s -X POST "http://localhost:8080/admin/v1/models/${MODEL_ID}/backends" \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "http://localhost:8000",
    "weight": 1
  }' | jq .
```

### 5. Create an API key

```bash
curl -s -X POST http://localhost:8080/admin/v1/keys \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "dev-key"}' | jq .
```

Save the `key` field from the response -- it is shown only once.

### 6. Make a request

```bash
# Non-streaming
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ml-YOUR_KEY_HERE" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-3",
    "messages": [{"role": "user", "content": "Hello!"}]
  }' | jq .

# Streaming
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ml-YOUR_KEY_HERE" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-3",
    "stream": true,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Docker Compose (full stack)

```bash
docker compose up --build
```

This starts the gateway, PostgreSQL, and Redis. Run migrations first in a separate step or use an init container.

## Configuration

The gateway is configured via a YAML file and/or environment variables. Environment variables take precedence.

```bash
# Using a config file
gateway serve gateway.yaml

# Using environment variables only (config file optional)
DATABASE_URL=... REDIS_URL=... ADMIN_API_KEY=... gateway serve
```

### Full Configuration Reference

```yaml
server:
  port: 8080                # Server listen port (env: PORT)
  read_timeout: 30s
  write_timeout: 300s       # Long timeout for streaming responses
  idle_timeout: 120s

database:
  url: "${DATABASE_URL}"    # PostgreSQL connection string (required)
  max_open_conns: 25
  max_idle_conns: 10

redis:
  url: "${REDIS_URL}"       # Redis connection string (required)

auth:
  admin_key: "${ADMIN_API_KEY}"  # Admin API key for /admin/v1 endpoints

rate_limits:
  default_rpm: 60           # Default requests per minute per key
  default_tpm: 100000       # Default tokens per minute per key

health_check:
  interval: 10s             # Active health check interval
  timeout: 5s               # Health check request timeout
  healthy_threshold: 3      # Consecutive successes to mark healthy
  unhealthy_threshold: 1    # Consecutive failures to mark unhealthy

registry:
  cache_refresh_interval: 30s  # How often to refresh the model cache from PG

logging:
  level: info               # debug, info, warn, error (env: LOG_LEVEL)
  format: json              # json or text (env: LOG_FORMAT)
```

### Environment Variable Overrides

| Variable | Config Path | Description |
|----------|-------------|-------------|
| `DATABASE_URL` | `database.url` | PostgreSQL connection string |
| `REDIS_URL` | `redis.url` | Redis connection string |
| `ADMIN_API_KEY` | `auth.admin_key` | Admin API authentication key |
| `PORT` | `server.port` | Server listen port |
| `LOG_LEVEL` | `logging.level` | Log level |
| `LOG_FORMAT` | `logging.format` | Log format (json/text) |

## API Reference

### Client API (OpenAI-compatible)

All `/v1` endpoints require a valid API key via `Authorization: Bearer <key>`.

#### `GET /v1/models`

List available models (filtered by the key's `allowed_models`).

```json
{
  "object": "list",
  "data": [
    {"id": "llama-3", "object": "model", "created": 1710000000, "owned_by": "inference-gateway"}
  ]
}
```

#### `POST /v1/chat/completions`

Proxy chat completions to the appropriate vLLM backend. Supports both streaming (`"stream": true`) and non-streaming requests. The `model` field uses the registered alias name (not the underlying vLLM model ID).

**Request:**
```json
{
  "model": "llama-3",
  "messages": [{"role": "user", "content": "Hello!"}],
  "stream": false
}
```

**Response** (non-streaming):
```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "llama-3",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "..."}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
}
```

**Response** (streaming): Server-Sent Events, each line prefixed with `data: `. The model name is rewritten to the alias in every chunk. The stream ends with `data: [DONE]`.

#### `GET /health`

Returns `{"status": "ok"}` -- no authentication required.

#### `GET /metrics`

Prometheus metrics endpoint -- no authentication required.

### Admin API

All `/admin/v1` endpoints require `X-Admin-Key: <admin_key>` header.

#### Models

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/admin/v1/models` | List all models |
| `POST` | `/admin/v1/models` | Create a model |
| `GET` | `/admin/v1/models/{id}` | Get a model by ID |
| `PUT` | `/admin/v1/models/{id}` | Update a model (partial) |
| `DELETE` | `/admin/v1/models/{id}` | Delete a model |

**Create model request:**
```json
{
  "name": "llama-3",
  "model_id": "meta-llama/Llama-3.1-70B-Instruct",
  "active": true,
  "default_params": {"temperature": 0.7, "max_tokens": 4096},
  "reasoning_config": {"enabled": true},
  "transforms": {"system_prompt_prefix": "You are a helpful assistant."}
}
```

#### Backends

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/admin/v1/models/{id}/backends` | Add a backend |
| `PUT` | `/admin/v1/models/{id}/backends/{bid}` | Update a backend |
| `DELETE` | `/admin/v1/models/{id}/backends/{bid}` | Remove a backend |

**Create backend request:**
```json
{
  "url": "http://vllm-host:8000",
  "weight": 1,
  "active": true
}
```

#### API Keys

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/admin/v1/keys` | List all keys |
| `POST` | `/admin/v1/keys` | Create a key (returns plaintext once) |
| `GET` | `/admin/v1/keys/{id}` | Get key metadata |
| `PUT` | `/admin/v1/keys/{id}` | Update a key |
| `DELETE` | `/admin/v1/keys/{id}` | Revoke a key |

**Create key request:**
```json
{
  "name": "production-service",
  "rpm_limit": 120,
  "tpm_limit": 500000,
  "allowed_models": ["llama-3"],
  "expires_at": "2025-12-31T23:59:59Z"
}
```

**Create key response:**
```json
{
  "key": "ml-abc123...",
  "api_key": {
    "id": "uuid",
    "name": "production-service",
    "key_prefix": "ml-abc123",
    "active": true,
    "rpm_limit": 120,
    "tpm_limit": 500000,
    "allowed_models": ["llama-3"]
  }
}
```

### Rate Limit Headers

All `/v1` responses include rate limit headers:

```
X-RateLimit-Limit-Requests: 60
X-RateLimit-Remaining-Requests: 58
X-RateLimit-Reset-Requests: 45s
X-RateLimit-Limit-Tokens: 100000
X-RateLimit-Remaining-Tokens: 99000
X-RateLimit-Reset-Tokens: 45s
```

When rate limited, the gateway returns `429 Too Many Requests` with a `Retry-After` header.

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│                      HTTP Server                          │
│                                                           │
│  Middleware Chain:                                         │
│  Recoverer -> RealIP -> RequestID -> Logging              │
│                                                           │
│  /v1 routes:  Auth -> RateLimit -> Metrics -> Handler     │
│  /admin routes:  AdminAuth -> Handler                     │
│  /health, /metrics:  (no auth)                            │
│                                                           │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────┐ │
│  │ Proxy       │  │ Admin        │  │ Load Balancer    │ │
│  │ Handler     │  │ Handler      │  │ (WRR + Health)   │ │
│  └──────┬──────┘  └──────┬───────┘  └────────┬─────────┘ │
│         │                │                    │           │
│  ┌──────▼──────┐  ┌──────▼───────┐  ┌────────▼─────────┐ │
│  │ Registry    │  │ Auth Store   │  │ Health Checker   │ │
│  │ Cache       │  │ (PG)         │  │ (Active+Passive) │ │
│  │ (Redis)     │  │              │  │                  │ │
│  └──────┬──────┘  └──────────────┘  └──────────────────┘ │
│         │                                                 │
│  ┌──────▼──────┐  ┌──────────────┐                        │
│  │ Registry    │  │ Rate Limiter │                        │
│  │ Store (PG)  │  │ (Redis)      │                        │
│  └─────────────┘  └──────────────┘                        │
└──────────────────────────────────────────────────────────┘
```

### Key Components

| Component | Package | Description |
|-----------|---------|-------------|
| Config | `internal/config` | YAML + env var configuration loading |
| Database | `internal/database` | PostgreSQL connection pool, embedded SQL migrations |
| Registry | `internal/registry` | Model/backend store (PG) + Redis-backed cache |
| Auth | `internal/auth` | API key generation (`ml-` prefix), SHA-256 hashing, PG store |
| Rate Limiter | `internal/ratelimit` | Redis sliding window (Lua script), TPM batcher |
| Load Balancer | `internal/loadbalancer` | Smooth WRR (Nginx algorithm), active + passive health checks |
| Proxy | `internal/proxy` | Non-streaming + SSE streaming reverse proxy, request transforms |
| Admin | `internal/admin` | REST API for model, backend, and key management |
| Middleware | `internal/middleware` | Auth, rate limit, metrics, request ID, logging |
| Metrics | `internal/metrics` | Prometheus metric definitions |
| Server | `internal/server` | HTTP server, router wiring, dependency injection |

## Development

### Build

```bash
make build          # Build binary to bin/gateway
```

### Test

```bash
make test           # Run all tests (requires Docker for testcontainers)
go test -p 1 ./...  # Run sequentially if Docker resource pressure causes flakes
```

### Lint

```bash
make lint           # Requires golangci-lint
```

### Migrations

```bash
make migrate-up     # Run pending migrations
make migrate-down   # Roll back last migration
```

### Project Structure

```
cmd/gateway/main.go                 Entry point (serve + migrate subcommands)
internal/
  config/                           Configuration loading
  database/                         PG connection + migrations (embedded SQL)
  registry/                         Model/backend store + Redis cache
  auth/                             API key management
  ratelimit/                        Redis sliding window rate limiter
  loadbalancer/                     Weighted round-robin + health checks
  proxy/                            Reverse proxy (streaming + non-streaming)
  admin/                            Admin REST API handlers
  middleware/                       HTTP middleware chain
  metrics/                          Prometheus metric definitions
  server/                           HTTP server + route wiring
```
