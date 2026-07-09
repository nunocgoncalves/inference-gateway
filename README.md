# Inference Gateway

The Horizonshift/Iterabase inference gateway: an OpenAI-compatible Go service
that routes client requests to vLLM/SGLang backends. It consumes the model
catalog, API keys, capabilities, and rate limits from the **control-plane**
(directly from the shared Postgres, LISTEN/NOTIFY-synced) and enforces
per-identity rate limits via Redis. Per-customer, self-hosted; pairs with the
control-plane operator, which owns the catalog/auth/policy CRDs.

See the Platform Direction doc (Obsidian: *Horizonshift Platform Direction*)
for the full architecture; this repo is the source of truth for gateway
infrastructure intent.

## Status

**Walking skeleton.** HOR-247 rewired the gateway onto the control-plane
snapshot: it reads `catalog.effective_catalog`, `identity.api_keys`,
`permissions.effective_capabilities`, and `permissions.effective_rate_limits`
directly from the shared Postgres (in-memory cache, LISTEN/NOTIFY-refreshed)
and routes alias → backend. The legacy self-managed registry / auth / admin /
load balancer is gone. Requires control-plane v0.0.5 (HOR-328) on the shared
Postgres.

## Architecture

```
Clients ──> [ Auth: API key -> identity ] ──> [ RateLimit: per-identity (Redis) ]
          ──> [ Route alias -> backend_url + rewrite model + transforms ] ──> vLLM/SGLang
                                |
                  snapshot cache (in-memory, per-pod, LISTEN/NOTIFY-synced)
                                |
            shared Postgres (control-plane schemas)        Redis (rate-limit counters)
```

- **Catalog / auth / policy / rate-limits:** read-only from the control-plane's
  Postgres views (`catalog.effective_catalog`, `identity.api_keys`,
  `permissions.effective_capabilities`, `permissions.effective_rate_limits`).
  The gateway **owns no tables** (no `migrate` subcommand; the control-plane
  creates the schemas).
- **Snapshot cache:** in-memory, per pod; refreshed on start, on LISTEN/NOTIFY
  (`catalog_changed` / `api_keys_changed` / `permissions_changed`), and on a 30s
  poll fallback. `/readyz` goes unhealthy if the snapshot is stale, so a pod with
  a dead LISTEN drops from the Service LB. Multi-replica-safe: each pod LISTENs.
- **Routing:** alias → single `backend_url` (the catalog's `available` flag is
  the health signal, set by the control-plane `ModelBackend` reconciler). The
  request `model` field is rewritten alias → `backend_model_id`; per-alias
  transforms applied (default params, reasoning config, system prompt). No
  weighted round-robin / health checker.
- **Rate limits:** per-identity RPM/TPM (from the snapshot) enforced via a Redis
  sliding window — the only shared state. TPM incremented from response usage.
- **Redis:** rate-limit counters only.

## Quick start

The gateway reads the control-plane's schemas, so run it alongside a
control-plane (whose migrations create the schemas). The model aliases and API
keys come from control-plane `Model` / `IdentityMapping` / `PermissionPolicy`
CRs.

```bash
export DATABASE_URL="postgres://..."   # shared Postgres (control-plane schemas)
export REDIS_URL="redis://..."          # rate-limit counters
export ADMIN_API_KEY="..."              # for /admin/v1/snapshot
go run ./cmd/gateway serve
```

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer cp-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3-27b","messages":[{"role":"user","content":"Hello!"}]}'
```

## Configuration

YAML and/or env vars (env wins). Key sections:

```yaml
server:       {port: 8080, read_timeout: 30s, write_timeout: 300s, idle_timeout: 120s}
database:     {url: "${DATABASE_URL}", max_open_conns: 25, max_idle_conns: 10}
redis:        {url: "${REDIS_URL}"}
auth:         {admin_key: "${ADMIN_API_KEY}"}        # /admin/v1/snapshot only
snapshot:
  refresh_interval: 30s       # poll fallback (LISTEN/NOTIFY drives prompt updates)
  readiness_staleness: 60s    # /readyz unhealthy if older than this
logging:      {level: info, format: json}
```

| Env var | Config | |
|---|---|---|
| `DATABASE_URL` | `database.url` | shared Postgres (required) |
| `REDIS_URL` | `redis.url` | rate-limit counters (required) |
| `ADMIN_API_KEY` | `auth.admin_key` | debug endpoint |
| `PORT` | `server.port` | |
| `LOG_LEVEL` / `LOG_FORMAT` | `logging.*` | |

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | Bearer API key | Proxy to the backend (streaming + non-streaming) |
| `GET` | `/v1/models` | Bearer API key | Available aliases |
| `GET` | `/health` | – | Liveness |
| `GET` | `/readyz` | – | Readiness (snapshot freshness gate) |
| `GET` | `/metrics` | – | Prometheus |
| `GET` | `/admin/v1/snapshot` | `X-Admin-Key` | Consumed catalog / debug (read-only, no secrets) |

`/v1` responses carry rate-limit headers (`X-RateLimit-Limit/Remaining/Reset-Requests`
+ `-Tokens`); `429` + `Retry-After` when exceeded. The request `model` field uses
the alias; the response `model` is rewritten back to the alias.

## Development

```bash
make build          # bin/gateway
make test           # requires Docker (testcontainers: PG + Redis)
make lint           # golangci-lint
```

```
cmd/gateway/main.go        serve
internal/
  config/                  configuration
  database/                PG connection pool (no migrations — gateway owns no tables)
  snapshot/                control-plane views reader + in-memory LISTEN/NOTIFY cache
  proxy/                   routing + model rewrite + transforms + SSE streaming
  middleware/              auth (API key -> identity), rate limit, logging, metrics, request ID
  ratelimit/               Redis sliding window (RPM/TPM counters)
  server/                  HTTP server + routes (/v1, /readyz, /admin/v1/snapshot)
  metrics/                 Prometheus definitions
```
