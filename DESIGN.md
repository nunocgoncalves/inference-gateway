# Inference API Gateway — Technical Design

## 1. Overview

An OpenAI-compatible API gateway written in **Go** that sits between clients and **vLLM** inference engine instances. It provides a model registry, request routing, weighted round-robin load balancing with health checks, API key authentication, rate limiting (RPM + TPM), and full SSE streaming support.

```
┌─────────┐     ┌─────────────────────────────────┐     ┌────────────┐
│ Clients  │────▶│       Inference Gateway          │────▶│ vLLM #1    │
│          │◀────│                                   │◀────│ (model-a)  │
│ (OpenAI  │     │  ┌───────┐  ┌──────┐  ┌───────┐ │     ├────────────┤
│  compat) │     │  │ Auth  │─▶│Route │─▶│Proxy  │ │────▶│ vLLM #2    │
│          │     │  │ + RL  │  │ + LB │  │+ Xfm  │ │◀────│ (model-a)  │
└─────────┘     │  └───────┘  └──────┘  └───────┘ │     ├────────────┤
                │                                   │────▶│ vLLM #3    │
                │  ┌──────────┐  ┌───────┐         │◀────│ (model-b)  │
                │  │PostgreSQL│  │ Redis │         │     └────────────┘
                │  │(registry)│  │(rates)│         │
                │  └──────────┘  └───────┘         │
                └─────────────────────────────────┘
```

## 2. Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.23+ | High-performance, excellent concurrency, stdlib `net/http/httputil.ReverseProxy` |
| HTTP Router | `chi` (go-chi/chi) | Lightweight, idiomatic, middleware-friendly |
| PostgreSQL driver | `pgx` | Fastest pure-Go PG driver, connection pooling built-in |
| Redis client | `go-redis/redis` | Rate limiting state, health cache |
| Config | `envconfig` + YAML | Env vars for secrets, YAML for static bootstrap config |
| Migrations | `golang-migrate/migrate` | SQL migration files, embedded in binary |
| Logging | `slog` (stdlib) | Structured logging, zero dependencies |
| Metrics | `prometheus/client_golang` | Prometheus-compatible metrics endpoint |
| Testing | `testify` + `testcontainers-go` | Real DB/Redis in tests, no mocks for infrastructure |
| Containerization | Docker multi-stage build | Small final image (~20MB) |
| Dev environment | Docker Compose | PostgreSQL + Redis + Gateway |

## 3. Project Structure

```
inference-gateway/
├── cmd/
│   └── gateway/
│       └── main.go                 # Entry point, subcommands: serve, migrate
├── internal/
│   ├── config/
│   │   ├── config.go               # Config struct + loading
│   │   └── config_test.go
│   ├── server/
│   │   ├── server.go               # HTTP server setup
│   │   └── routes.go               # Route registration
│   ├── middleware/
│   │   ├── auth.go                 # API key authentication
│   │   ├── ratelimit.go            # RPM + TPM rate limiting
│   │   ├── logging.go              # Request/response logging
│   │   ├── requestid.go            # X-Request-ID injection
│   │   └── metrics.go              # Prometheus metrics middleware
│   ├── proxy/
│   │   ├── handler.go              # Reverse proxy handler
│   │   ├── streaming.go            # SSE streaming proxy
│   │   ├── transform.go            # Request/response transforms
│   │   └── handler_test.go
│   ├── registry/
│   │   ├── models.go               # Model registry data types
│   │   ├── store.go                # PostgreSQL store interface + impl
│   │   ├── cache.go                # In-memory cache with refresh
│   │   └── store_test.go
│   ├── loadbalancer/
│   │   ├── balancer.go             # Weighted round-robin implementation
│   │   ├── healthcheck.go          # Backend health checking
│   │   └── balancer_test.go
│   ├── auth/
│   │   ├── apikey.go               # API key validation + lookup
│   │   ├── store.go                # PostgreSQL key store
│   │   └── apikey_test.go
│   ├── ratelimit/
│   │   ├── limiter.go              # Rate limiter interface
│   │   ├── redis.go                # Redis sliding window impl
│   │   └── limiter_test.go
│   └── admin/
│       ├── handler.go              # Admin API handlers
│       ├── models.go               # Admin API request/response types
│       └── handler_test.go
├── migrations/
│   ├── 000001_create_models.up.sql
│   ├── 000001_create_models.down.sql
│   ├── 000002_create_api_keys.up.sql
│   └── 000002_create_api_keys.down.sql
├── docker-compose.yml
├── Dockerfile
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

## 4. Database Schema

### Models Table

```sql
CREATE TABLE models (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL UNIQUE,          -- "gpt-4o" (alias)
    model_id      TEXT NOT NULL,                  -- "meta-llama/Llama-3.1-70B" (actual vLLM model)
    active        BOOLEAN NOT NULL DEFAULT true,

    -- Default sampling parameters (JSON, overridable per request)
    default_params JSONB NOT NULL DEFAULT '{}',
    -- e.g. {"temperature": 0.7, "top_p": 0.9, "max_tokens": 4096}

    -- Reasoning configuration
    -- enabled: true = force reasoning on, false = force reasoning off, null = passthrough
    -- e.g. {"enabled": true, "reasoning_effort": "medium", "include_reasoning": true}
    -- e.g. {"enabled": false} -> injects reasoning_effort: "none", include_reasoning: false
    -- e.g. {} or {"enabled": null} -> passthrough, no override
    reasoning_config JSONB NOT NULL DEFAULT '{}',

    -- Request transform rules
    transforms JSONB NOT NULL DEFAULT '{}',
    -- e.g. {"system_prompt_prefix": "You are...", "rewrite_model_name": true}

    -- Per-model rate limits (overrides key-level limits)
    rate_limits JSONB NOT NULL DEFAULT '{}',
    -- e.g. {"rpm": 100, "tpm": 50000}

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Model Backends Table

```sql
CREATE TABLE model_backends (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id      UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    url           TEXT NOT NULL,                  -- "http://vllm-1:8000"
    weight        INT NOT NULL DEFAULT 1,         -- LB weight
    active        BOOLEAN NOT NULL DEFAULT true,

    -- Health check state (managed by gateway, not user-set)
    healthy       BOOLEAN NOT NULL DEFAULT true,
    last_health_check TIMESTAMPTZ,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE(model_id, url)
);
```

### API Keys Table

```sql
CREATE TABLE api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,                   -- Human-readable label
    key_hash      TEXT NOT NULL UNIQUE,             -- SHA-256 hash of the key
    key_prefix    TEXT NOT NULL,                    -- First 8 chars for identification
    active        BOOLEAN NOT NULL DEFAULT true,

    -- Rate limits for this key
    rpm_limit     INT,                             -- null = use global default
    tpm_limit     INT,                             -- null = use global default

    -- Optional: restrict key to specific models
    allowed_models TEXT[],                          -- null = all models

    expires_at    TIMESTAMPTZ,                     -- null = never expires
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## 5. Core Components Design

### 5.1 Request Flow

```
Client Request
     │
     ▼
[RequestID Middleware]     ── Inject X-Request-ID
     │
     ▼
[Logging Middleware]       ── Log request start
     │
     ▼
[Metrics Middleware]       ── Record request metrics
     │
     ▼
[Auth Middleware]          ── Validate API key, load key config
     │                        Return 401 if invalid
     ▼
[Rate Limit Middleware]    ── Check RPM limit against Redis
     │                        Return 429 if exceeded
     ▼
[Route Handler]            ── Extract model from request body
     │                        Look up model in registry
     │                        Return 404 if model not found
     ▼
[Request Transform]        ── Apply model-specific transforms:
     │                        - Rewrite model name to actual model_id
     │                        - Inject default params (if not in request)
     │                        - Apply reasoning config (enable/disable/passthrough)
     │                        - Inject system prompt prefix
     ▼
[Load Balancer]            ── Select backend via weighted round-robin
     │                        Skip unhealthy backends
     ▼
[Reverse Proxy]            ── Forward to selected vLLM backend
     │                        Stream SSE if stream=true
     ▼
[Response Processing]      ── Extract usage from response
     │                        Update TPM counter in Redis (live during streaming)
     │                        Rewrite model name back to alias
     │                        Record TTFT, ITL, tokens/s metrics
     ▼
Client Response
```

### 5.2 Load Balancer — Weighted Round-Robin

Smooth weighted round-robin (Nginx-style). Each backend has a `currentWeight` that gets incremented by its `weight` each selection cycle. The backend with the highest `currentWeight` is selected, and its `currentWeight` is decremented by the total weight sum. This distributes requests proportionally and smoothly.

Backends marked unhealthy are skipped. If all backends for a model are unhealthy, return 503 Service Unavailable.

### 5.3 Health Checks

- **Active health checks**: Goroutine polls `GET /health` on each backend every 10s (configurable)
- **Passive health checks**: If a proxy request fails (connection refused, timeout), mark backend unhealthy immediately
- **Recovery**: After marking unhealthy, probe every 5s. After 3 consecutive successes, mark healthy again
- **Circuit breaker**: If all backends for a model are unhealthy, return 503 Service Unavailable

### 5.4 Rate Limiting — Redis Sliding Window

Using Redis sorted sets for sliding window rate limiting:

```
Key: ratelimit:{key_id}:rpm
Key: ratelimit:{key_id}:tpm
```

**RPM**: Sliding window counter. Each request adds a member with score = current timestamp. Count members in `[now - 60s, now]`.

**TPM**: Same structure but increment by token count. During streaming, tokens are tracked live using `continuous_usage_stats` from vLLM. Updates are batched locally and flushed to Redis every 500ms to reduce load.

Rate limit headers returned to client:

```
X-RateLimit-Limit-Requests: 100
X-RateLimit-Remaining-Requests: 95
X-RateLimit-Reset-Requests: 45s
X-RateLimit-Limit-Tokens: 100000
X-RateLimit-Remaining-Tokens: 87234
X-RateLimit-Reset-Tokens: 23s
```

### 5.5 SSE Streaming Proxy

The gateway does **not buffer** the streaming response. It flushes each SSE chunk to the client as it arrives from vLLM.

Key design:
- Set response headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`
- Inject `stream_options: {include_usage: true, continuous_usage_stats: true}` into all requests to vLLM
- Parse each SSE `data:` line, rewrite model name in chunks
- Track cumulative `usage` from each chunk, compute deltas
- Batch TPM updates to Redis every 500ms during streaming
- Record TTFT (time to first content chunk) and ITL (inter-token latency) for metrics
- Handle client disconnection via context cancellation → close backend connection
- Forward `data: [DONE]\n\n` terminator

### 5.6 Request Transforms

Transforms are applied per-model based on the registry configuration:

1. **Model name rewrite**: Replace the alias (`gpt-4o`) with the actual vLLM model ID (`meta-llama/Llama-3.1-70B`)
2. **Default params injection**: If the request doesn't specify `temperature`, `top_p`, `max_tokens`, etc., inject the model's defaults
3. **Reasoning config**:
   - `enabled: true` → inject `reasoning_effort` and `include_reasoning` from config
   - `enabled: false` → inject `reasoning_effort: "none"`, `include_reasoning: false` (disables reasoning)
   - `enabled: null` / not set → passthrough, no override
4. **System prompt prefix**: Prepend a configured system prompt to the messages array
5. **Response model rewrite**: In the response, rewrite the model name back to the alias

### 5.7 Admin API

All admin endpoints are under `/admin/v1/` and require a separate admin API key (configured via environment variable, validated via `X-Admin-Key` header).

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/admin/v1/models` | List all models |
| `POST` | `/admin/v1/models` | Create a model |
| `GET` | `/admin/v1/models/{id}` | Get model details |
| `PUT` | `/admin/v1/models/{id}` | Update model |
| `DELETE` | `/admin/v1/models/{id}` | Delete model |
| `POST` | `/admin/v1/models/{id}/backends` | Add a backend |
| `PUT` | `/admin/v1/models/{id}/backends/{bid}` | Update backend |
| `DELETE` | `/admin/v1/models/{id}/backends/{bid}` | Remove backend |
| `GET` | `/admin/v1/keys` | List API keys |
| `POST` | `/admin/v1/keys` | Create API key (returns key once) |
| `GET` | `/admin/v1/keys/{id}` | Get key details |
| `PUT` | `/admin/v1/keys/{id}` | Update key |
| `DELETE` | `/admin/v1/keys/{id}` | Revoke key |
| `GET` | `/admin/v1/health` | System health + backend status |

### 5.8 Model Registry Cache

In-memory cache of the model registry, refreshed periodically (every 30s) from PostgreSQL.

- On startup: load all models + backends from DB
- Every 30s: refresh from DB
- On admin API mutation: invalidate cache immediately
- All proxy request reads use `RLock` for zero-contention reads

## 6. Client-Facing API Endpoints

Fully OpenAI-compatible:

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/models` | List models from registry (filtered by key's `allowed_models`) |
| `POST` | `/v1/chat/completions` | Chat completions (streaming + non-streaming) |
| `GET` | `/health` | Gateway health check |
| `GET` | `/metrics` | Prometheus metrics |

The `/v1/models` response is synthesized from the registry, not proxied to vLLM.

## 7. Configuration

```yaml
# gateway.yaml
server:
  port: 8080
  read_timeout: 30s
  write_timeout: 300s  # Long for streaming
  idle_timeout: 120s

database:
  url: "${DATABASE_URL}"  # postgres://user:pass@host:5432/gateway
  max_open_conns: 25
  max_idle_conns: 10

redis:
  url: "${REDIS_URL}"  # redis://host:6379/0

auth:
  admin_key: "${ADMIN_API_KEY}"

rate_limits:
  default_rpm: 60
  default_tpm: 100000

health_check:
  interval: 10s
  timeout: 5s
  healthy_threshold: 3
  unhealthy_threshold: 1

registry:
  cache_refresh_interval: 30s

logging:
  level: info
  format: json
```

## 8. Prometheus Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gateway_requests_total` | counter | model, status_code, streaming | Total requests |
| `gateway_request_duration_seconds` | histogram | model, streaming | Total request duration (incl. streaming) |
| `gateway_time_to_first_token_seconds` | histogram | model | TTFT: time from request to first content chunk |
| `gateway_inter_token_latency_seconds` | histogram | model | ITL: time between consecutive content chunks |
| `gateway_tokens_per_second` | histogram | model, type=prompt\|completion | Tokens generated per second per request |
| `gateway_prompt_tokens_total` | counter | model | Total prompt tokens processed |
| `gateway_completion_tokens_total` | counter | model | Total completion tokens generated |
| `gateway_active_streams` | gauge | model | Currently active streaming connections |
| `gateway_backend_health` | gauge | model, backend_url | 1=healthy, 0=unhealthy |
| `gateway_rate_limit_hits_total` | counter | key_prefix, limit_type | Rate limit rejections |
| `gateway_backend_request_duration_seconds` | histogram | model, backend_url | Backend response time |

## 9. Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Registry in PostgreSQL | Admin API can mutate at runtime without restart | Dynamic model/key management |
| In-memory cache for registry | Avoid DB round-trip on every request | 30s refresh; admin mutations trigger immediate invalidation |
| Redis sorted sets for rate limiting | Sliding window is more accurate than fixed window | Prevents burst at window boundaries |
| `continuous_usage_stats` from vLLM | Live TPM tracking during streaming | Accurate token counting even on client disconnect |
| 500ms batch flush for TPM | Reduce Redis load during streaming | Good balance of accuracy and performance |
| Hash API keys (SHA-256) | Store only hashes in DB | Key plaintext shown once at creation, never stored |
| Migration as subcommand with exit | Clean init container support | `gateway migrate up` runs migrations and exits |
| testcontainers-go for tests | Real DB/Redis in tests | No mocks for infrastructure, higher confidence |
| Admin API on same port | Simpler deployment | Protected by separate admin key |
| Model name aliasing | Clients use friendly names | Decouples client API from vLLM model identifiers |
| Reasoning config with enable/disable/passthrough | Flexible per-model reasoning control | Can serve same model with different reasoning behavior via aliases |

---

## 10. Implementation Tickets

### Ticket 1: Project Scaffolding & Build Infrastructure
**Priority**: P0 | **Estimate**: Small

- Initialize Go module
- Set up project directory structure
- Create `Makefile` with targets: `build`, `test`, `lint`, `run`, `migrate`
- Create `Dockerfile` (multi-stage: build + runtime)
- Create `docker-compose.yml` (gateway + PostgreSQL + Redis)
- Set up `slog`-based structured logging
- Set up config loading (YAML + env vars)
- Add `.gitignore`
- Create `main.go` with subcommand structure (`serve`, `migrate`) and `/health` endpoint

**Done**: `make build` produces a binary, `docker-compose up` starts all services, `/health` returns 200.

---

### Ticket 2: Database Schema & Migrations
**Priority**: P0 | **Estimate**: Small

- Set up `golang-migrate` with embedded SQL migrations
- Create migration 001: `models` + `model_backends` tables
- Create migration 002: `api_keys` table
- Implement `gateway migrate up/down` subcommand — runs migrations then exits
- Wire PostgreSQL connection in config

**Done**: `gateway migrate up` creates all tables and exits 0. `gateway migrate down` rolls back. Clean init container support.

---

### Ticket 3: Model Registry — Store & Cache
**Priority**: P0 | **Estimate**: Medium

- Implement `registry.Store` interface with PostgreSQL implementation (CRUD for models + backends)
- Implement `registry.Cache` — in-memory cache with periodic refresh
- Data types: `Model`, `Backend`, `ModelConfig` (default params, reasoning config, transforms, rate limits)
- Reasoning config supports `enabled: true/false/null` (enable, disable, passthrough)
- Tests use `testcontainers-go` with real PostgreSQL

**Done**: Can store/retrieve models and backends. Cache serves reads without DB round-trip. Tests pass with real DB.

---

### Ticket 4: API Key Authentication
**Priority**: P0 | **Estimate**: Medium

- Implement `auth.Store` — PostgreSQL store for API keys (create with hash, validate, load config)
- Implement `auth.Middleware` — chi middleware (extract Bearer token, validate, inject context)
- OpenAI-compatible error format on 401
- Tests use `testcontainers-go` with real PostgreSQL

**Done**: Requests without valid API key get 401. Valid keys pass through with metadata in context.

---

### Ticket 5: Rate Limiting (RPM + TPM)
**Priority**: P0 | **Estimate**: Medium

- Implement `ratelimit.RedisLimiter` using sorted sets (sliding window)
- RPM: check before forwarding, return 429 if exceeded
- TPM: increment live during streaming with 500ms batch flush to Redis
- Implement `ratelimit.Middleware` with rate limit response headers
- Tests use `testcontainers-go` with real Redis

**Done**: RPM enforcement works. TPM increments live. Rate limit headers correct. 429 on exceeded.

---

### Ticket 6: Weighted Round-Robin Load Balancer + Health Checks
**Priority**: P0 | **Estimate**: Medium

- Implement smooth weighted round-robin (Nginx algorithm)
- Health checker: active checks (GET /health), passive checks (proxy failure)
- Configurable interval, timeout, healthy/unhealthy thresholds
- Integration with registry cache

**Done**: Requests distributed by weight. Unhealthy backends skipped. Health checks detect failures.

---

### Ticket 7: Reverse Proxy — Non-Streaming
**Priority**: P0 | **Estimate**: Medium

- Proxy handler for `POST /v1/chat/completions` (non-streaming)
- Parse request, look up model, apply transforms, select backend, forward, process response
- Implement `GET /v1/models` (synthesized from registry)
- Error handling: 404 model not found, 503 all backends down, 504 timeout

**Done**: Non-streaming chat completions work end-to-end. Model aliasing works.

---

### Ticket 8: SSE Streaming Proxy
**Priority**: P0 | **Estimate**: Medium-Large

- Streaming path: inject `continuous_usage_stats`, flush chunks, no buffering
- Live TPM tracking with 500ms batch flush
- TTFT and ITL measurement for metrics
- Client disconnection handling
- Model name rewrite in stream chunks

**Done**: Streaming works end-to-end. First byte latency minimal. TPM tracked live.

---

### Ticket 9: Request Transforms
**Priority**: P1 | **Estimate**: Small-Medium

- Transform engine: model name rewrite, default params, reasoning config (enable/disable/passthrough), system prompt
- Response transforms: rewrite model name back to alias

**Done**: All transform types work. Reasoning disable mode works.

---

### Ticket 10: Admin API — Models & Backends CRUD
**Priority**: P1 | **Estimate**: Medium

- CRUD endpoints under `/admin/v1/` for models and backends
- Admin auth via `X-Admin-Key` header
- Input validation, cache invalidation on mutation

**Done**: Full CRUD for models and backends. Cache invalidated on mutations.

---

### Ticket 11: Admin API — API Keys Management
**Priority**: P1 | **Estimate**: Small-Medium

- Key lifecycle: create (return plaintext once), list, update, revoke
- Never expose key hash, show prefix for identification

**Done**: Full key lifecycle. Revoked keys immediately rejected.

---

### Ticket 12: Prometheus Metrics
**Priority**: P1 | **Estimate**: Small-Medium

- `/metrics` endpoint with all metrics from section 8
- TTFT, ITL, tokens/s histograms
- Backend health gauge, active streams gauge, rate limit hits counter

**Done**: All metrics tracked and exposed. Correct under load.

---

### Ticket 13: Observability — Structured Logging & Request ID
**Priority**: P1 | **Estimate**: Small

- Request ID middleware (generate or use incoming `X-Request-ID`)
- Structured logging: request ID, method, path, status, duration, model, key_prefix, backend_url
- Propagate `X-Request-ID` to backends

**Done**: Every request has unique ID. All requests logged with structured fields.

---

### Ticket 14: End-to-End Integration Tests
**Priority**: P1 | **Estimate**: Medium

- testcontainers for PostgreSQL + Redis
- Mock vLLM server for realistic responses
- Test all major flows: auth, rate limiting, routing, proxy, streaming, admin, failover

**Done**: Integration suite passes against real infrastructure.

---

### Ticket 15: Documentation
**Priority**: P2 | **Estimate**: Small

- README: architecture, quick start, configuration reference, API reference, examples

**Done**: New developer can get gateway running by following README.

---

### Implementation Order

```
T1 (Scaffolding)
 │
 ▼
T2 (Migrations)
 │
 ├──────────────┬──────────────┐
 ▼              ▼              ▼
T3 (Registry)  T4 (Auth)     T5 (Rate Limit)
 │              │              │
 ├──────────────┴──────────────┘
 ▼
T6 (Load Balancer)
 │
 ▼
T7 (Proxy: Non-Streaming)
 │
 ▼
T8 (Proxy: Streaming)
 │
 ├──────────┬──────────────┬───────────────┐
 ▼          ▼              ▼               ▼
T9 (Xfm)  T10 (Admin)   T11 (Keys)     T12 (Metrics)
 │          │              │               │
 ├──────────┴──────────────┴───────────────┘
 ▼
T13 (Logging)
 │
 ▼
T14 (Integration Tests)
 │
 ▼
T15 (Docs)
```
