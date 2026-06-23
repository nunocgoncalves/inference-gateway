# M5-P1-T01 — Inference Gateway implementation audit

Date: 2026-06-22
Ticket: HOR-116 / M5-P1-T01
Scope: current `inference-gateway` working tree, including tracked files and the current untracked `scripts/benchmark.py`. The sibling `control-plane` repository currently has no tracked source files to audit.

## Validation performed

- `make -C inference-gateway build` succeeds.
- Fast non-container tests pass:
  - `cd inference-gateway && go test ./internal/config ./internal/loadbalancer ./internal/metrics ./internal/middleware`
  - `cd inference-gateway && go test ./internal/config ./internal/loadbalancer ./internal/metrics ./internal/middleware ./internal/proxy -run 'TestApply|TestRewrite|TestProcess|TestChunk|TestInject'`
- Full test suite is not run here because many packages use `testcontainers-go` for PostgreSQL and Redis and require Docker.

## Current implementation map

### Entrypoint and service wiring

- `cmd/gateway/main.go` exposes two subcommands:
  - `gateway serve [config]`: loads YAML/env configuration, connects to PostgreSQL and Redis, starts the registry cache, creates metrics, constructs proxy/admin handlers, and starts the HTTP server.
  - `gateway migrate up|down`: runs embedded database migrations using `DATABASE_URL`.
- `internal/server/routes.go` wires:
  - unauthenticated `GET /health`
  - unauthenticated `GET /metrics`
  - authenticated `/v1` routes:
    - `GET /v1/models`
    - `POST /v1/chat/completions`
  - admin `/admin/v1` routes protected by `X-Admin-Key` when configured.

### `/v1/models`

Implemented in `internal/proxy/handler.go`.

Current behavior:

- Lists active registry models from `registry.Cache.ListModels`.
- Filters by the authenticated API key's `allowed_models` list, if present.
- Returns an OpenAI-compatible list shape with public model alias IDs.

Important current boundaries:

- Model visibility is key/model-alias based only.
- No organization, project, endpoint, modality, capability, tenant-tier, rollout, or adapter binding context participates in model visibility.

### `/v1/chat/completions`

Implemented in `internal/proxy/handler.go` and `internal/proxy/streaming.go`.

Current request path:

1. Read and parse the request body to extract `model` and `stream`.
2. Check authenticated API key model access against `allowed_models`.
3. Lookup the model by public alias through the Redis-backed registry cache.
4. Select a backend with smooth weighted round-robin, keyed by model alias.
5. Apply model-level request transforms:
   - default sampling params
   - `chat_template_kwargs.enable_thinking`
   - system prompt prefix
6. Rewrite the public `model` field to the stored backend/vLLM `model_id`.
7. Proxy to `<backend_url>/v1/chat/completions`.
8. Rewrite response/chunk `model` fields back to the public alias.

Non-streaming behavior:

- Reads the backend response fully.
- Tracks usage from `usage.prompt_tokens`, `usage.completion_tokens`, and `usage.total_tokens`.
- Increments Redis TPM counters after the backend response completes.
- Records backend duration and token metrics.

Streaming behavior:

- Injects `stream_options.include_usage=true` and `stream_options.continuous_usage_stats=true`.
- Proxies SSE line-by-line with immediate flush and `X-Accel-Buffering: no`.
- Rewrites each JSON chunk's `model` field back to the alias.
- Tracks TTFT and inter-token latency from content-bearing chunks.
- Batches live TPM increments from continuous usage stats every 500ms.
- Records final prompt/completion token metrics from the last usage chunk.

Important current boundaries:

- The handler is directly coupled to vLLM's OpenAI-compatible chat endpoint.
- There is no internal executor boundary yet.
- There is no `/v1/responses` implementation.
- There is no conversation, response, message, event, or durable usage persistence.

### Admin endpoints

Implemented in `internal/admin/handler.go` and `internal/admin/keys.go`.

Current admin CRUD:

- Models:
  - `GET /admin/v1/models`
  - `POST /admin/v1/models`
  - `GET /admin/v1/models/{id}`
  - `PUT /admin/v1/models/{id}`
  - `DELETE /admin/v1/models/{id}`
- Backends:
  - `POST /admin/v1/models/{id}/backends`
  - `PUT /admin/v1/models/{id}/backends/{bid}`
  - `DELETE /admin/v1/models/{id}/backends/{bid}`
- API keys:
  - `GET /admin/v1/keys`
  - `POST /admin/v1/keys`
  - `GET /admin/v1/keys/{id}`
  - `PUT /admin/v1/keys/{id}`
  - `DELETE /admin/v1/keys`

Production classification:

- These endpoints are useful for local development, bootstrap, and smoke testing.
- They should not be the target production ownership model for M5.
- In production, auth/IAM should own credentials and policy generation; the control plane should own model/backend/adapter/rollout/placement desired state.
- Gateway admin endpoints should become internal-only, narrow, service-identity protected, and/or break-glass oriented.

### API-key authentication

Implemented in `internal/auth` and `internal/middleware/auth.go`.

Current behavior:

- Client requests use `Authorization: Bearer <ml-key>`.
- Keys are generated with an `ml-` prefix and 32 random bytes.
- Only a SHA-256 hash is stored.
- Validation loads active, unexpired key metadata from PostgreSQL.
- Request context carries an `auth.APIKey` with:
  - ID
  - name
  - key prefix
  - active flag
  - optional RPM/TPM limits
  - optional allowed model aliases
  - optional expiry

Important current gaps:

- No `AuthContext` abstraction.
- No `organization_id`, `project_id`, subject type/ID, credential type/fingerprint, scopes/capabilities, tenant tier/SLA, policy version, or provider/admin override model.
- No JWT/JWKS, service account, workload identity, or external auth/IAM validation boundary.

### PostgreSQL schema

Current migrations create three core tables:

- `models`
  - public alias `name`
  - backend `model_id`
  - active flag
  - default params JSONB
  - reasoning config JSONB
  - transforms JSONB
  - rate limits JSONB
- `model_backends`
  - model foreign key
  - backend URL
  - weight
  - active flag
  - health fields
- `api_keys`
  - key hash/prefix
  - active flag
  - optional RPM/TPM limits
  - optional allowed model aliases
  - optional expiry

Missing target schemas include:

- organizations/projects in the inference hot path
- tenant model bindings
- adapter artifacts and immutable versions
- adapter rollout policy
- adapter placement policy
- backend pools and normalized replicas
- routing snapshots/events
- response/conversation/message persistence
- durable inference usage records
- media references

### Redis usage

Current Redis use has two roles.

Registry cache:

- `internal/registry/cache.go` stores serialized models in Redis by model name, model ID, and active model list.
- Gateway request paths read Redis for model lookup/listing.
- Cache misses fall back to PostgreSQL and write through to Redis.
- Admin mutations call `Invalidate`, which refreshes Redis from PostgreSQL.
- A version counter exists, but request paths do not consume versioned deltas.

Rate limiting:

- `internal/ratelimit/redis.go` implements sliding-window RPM/TPM counters with Redis sorted sets and Lua.
- RPM is checked and incremented before forwarding.
- TPM is checked before forwarding but incremented later from response usage.
- Streaming TPM is batched from continuous usage chunks.
- Middleware currently fails open on Redis errors.

Production routing concern:

- The current registry cache is unsuitable as the target low-latency production routing mechanism because ordinary inference requests perform Redis reads and can fall back to PostgreSQL.
- M5 target should use in-process immutable/RCU-style snapshots refreshed by Redis Streams deltas, with full snapshot recovery after gaps/reconnect/staleness.
- Redis should remain on the request path only for centralized counters/admission where required, not for policy/routing lookup.

### Load balancing and health checks

Implemented in `internal/loadbalancer`.

Current load balancing:

- Smooth weighted round-robin over active backends.
- Backends marked unhealthy are skipped.
- The proxy keeps a balancer map keyed by model alias.

Current health-check code:

- Active health-check logic exists and probes `<backend_url>/health`.
- Passive failure reporting exists and is called when backend requests fail.
- Prometheus backend health gauge support exists.

Important wiring gap:

- `cmd/gateway/main.go` constructs a health checker, but does not start it for registered models/backends.
- The health checker is created with `onChange=nil`, so health changes do not update the in-memory weighted-round-robin balancers.
- Passive `ReportFailure` updates the health checker's internal probe state, but does not mark the corresponding proxy balancer backend unhealthy in the running gateway.
- As currently wired, active/passive health state does not reliably remove failed backends from routing unless the `healthy` field in registry data is already false.

Target gap:

- M5 should move toward centralized routing-state collection and normalized replica snapshots, not independent per-gateway backend probes as the source of truth.

### Metrics and logging

Current metrics in `internal/metrics` include:

- request count and duration
- TTFT
- inter-token latency
- prompt/completion token totals
- tokens/sec histogram
- active streams
- backend health
- rate-limit hits
- backend request duration

Current structured logs include:

- request ID
- method/path/status/duration
- model alias
- backend URL
- streaming flag
- API key prefix

Important target gaps:

- No organization/project/subject/policy/adapter/rollout/backend-pool/replica correlation fields.
- No durable high-cardinality usage/analytics records.
- No latency breakdown by auth/context resolution, tenant/model/adapter resolution, routing, admission, backend queue, prefill, decode, and persistence stages.
- Prometheus labels should remain bounded-cardinality; tenant/adapter/request analytics belong in durable records/logs/traces.

### Benchmark script

Current `scripts/benchmark.py` is present in the working tree but untracked.

Capabilities:

- Async HTTPX streaming benchmark for OpenAI-compatible chat completions.
- Configured via CLI args and environment variables.
- Supports prompt-token targets, concurrency sweeps, warmups, and JSONL output.
- Simulates a coding-agent prompt shape with large system/tool/context payloads.
- Requests streaming usage stats and computes TTFT, decode duration, end-to-end latency, decode tokens/sec, end-to-end completion tokens/sec, mean ITL, and system tokens/sec.

Target gaps:

- It benchmarks only `/v1/chat/completions`.
- It does not exercise `/v1/responses`, multimodal media references, adapter rollout/stickiness, adapter-aware routing, vLLM control operations, SGLang, or control-plane snapshot/event behavior.

## Gap list against M5 target design

### 1. No tenant/organization request context

Current gateway identity is API-key metadata only. The hot path has no required `organization_id`, optional `project_id`, subject identity, capabilities/scopes, credential fingerprint/type, tenant tier/SLA, policy version, or provider/admin override semantics.

M5 target requires an explicit `AuthContext` and routing/policy decisions keyed primarily by:

```text
organization_id + public_model_name + endpoint/modality
```

### 2. Routing is model-alias only

Current routing key is just the public model alias. The selected target is:

```text
model alias -> model_id -> weighted backend URL
```

M5 target requires local resolution of:

```text
organization_id + model + endpoint/modality
-> base_model_id
-> adapter_id/version after rollout assignment
-> backend_pool_id
-> fallback policy
-> tenant tier/SLA
```

### 3. No `/v1/responses` or persistence model

The current external surface supports `/v1/models` and `/v1/chat/completions` only.

Missing:

- OpenAI-compatible `/v1/responses`
- response records
- conversation records
- conversation/message tree
- response events
- edit/regenerate/branch semantics
- adapter/rollout assignment recording
- durable inference usage records
- correlation IDs suitable for future agent/orchestrator traces

### 4. No adapter registry, binding, rollout, or lifecycle

Current schema and runtime have no first-class concepts for:

- adapter artifact
- adapter immutable version
- base-model compatibility
- tenant model binding
- active/candidate adapter version
- rollout salt/percentage/state
- manual promotion
- automatic rollback guardrails
- placement tiers: pinned/warm/on-demand
- `min_hot_replicas` / `max_hot_replicas`
- warm/load/unload/pin/evict desired state

### 5. No adapter-aware routing

Current balancer only considers backend active/healthy state and configured static weight.

Missing target routing inputs:

- backend pool
- engine type
- base model
- modality compatibility
- loaded adapter/version hotness
- serving/warming/draining/disabled state
- fresh replica state version
- queue depth
- running/waiting requests
- KV cache usage
- GPU memory pressure
- prefill/decode pressure
- session/workflow affinity
- tenant priority/SLA
- recent local failures/in-flight overlay

### 6. Request-path Redis registry reads are unsuitable for target production routing

Current model lookup/listing is Redis read-through with PostgreSQL fallback. That is acceptable for a standalone gateway/bootstrap service, but not for the M5 low-latency target.

Target should be:

- startup full snapshot from control plane
- in-process immutable/RCU-style policy/routing state
- Redis Streams deltas for versioned updates
- full snapshot recovery after gaps, invalid deltas, reconnect, or staleness
- gateway readiness/fail-closed behavior for stale correctness-sensitive policy

### 7. No centralized routing-state collector

Current code contains per-backend health-check primitives, but no central service that polls/scrapes engines, normalizes replica state, coalesces snapshots, and publishes compact events.

Missing normalized replica state includes:

- replica ID and backend pool ID
- engine type
- supported modalities
- serving state
- loaded adapters
- queue/running/waiting request metrics
- KV/GPU pressure
- prefill/decode load
- throughput
- last seen and state version

### 8. No vLLM control client or engine abstraction

The gateway directly constructs HTTP requests to vLLM's OpenAI-compatible `/v1/chat/completions` endpoint.

Missing:

- `InferenceExecutor` boundary for data-plane execution
- `EngineControlClient` boundary for health, loaded adapters, load/unload, metrics, and drain
- vLLM multi-LoRA control integration
- engine-agnostic internal `ModelTarget` / `ReplicaTarget` shapes

### 9. No multimodal media reference flow

The current gateway forwards chat completion JSON bodies. It does not provide target multimodal behavior:

- media upload/reference model
- media authorization
- MIME/type/size/dimension/duration validation
- internal media URL/object reference rewrite
- multimodal-compatible pool routing
- sensitive media logging protections
- strict inline/base64/data URL limits

### 10. No SGLang abstraction or benchmark path in the gateway

SGLang is not present in code. This is acceptable for the first production path because M5 target keeps vLLM first and SGLang as non-blocking benchmark/future work.

### 11. Rate limiting and admission are key-centric only

Current Redis counters enforce API-key RPM and TPM. Model rate-limit fields exist in the model schema but are not applied by the middleware because rate limiting runs before the body/model lookup.

Missing target controls:

- organization/project quotas
- model/endpoint-specific policies
- tenant tier/SLA policy
- concurrency/admission counters
- per-tenant/model admission
- explicit fail-open/fail-closed policy by environment/tier
- durable usage/accounting records

### 12. Health/routing operational semantics are incomplete

Beyond the current wiring gap, M5 needs the distinction:

```text
healthy != routable
```

A backend process being healthy is not enough. A replica must also be serving, fresh, compatible with the base model/modality, not draining/warming/disabled, and hot for the required adapter/version.

### 13. Streaming behavior is functional but has target risks

Current streaming behavior is appropriate for a standalone OpenAI-compatible proxy, but target work should revisit:

- `bufio.Scanner` maximum SSE line size assumptions
- behavior when backends do not support `continuous_usage_stats`
- cancellation and final TPM flush guarantees under disconnects
- separating backend queue/prefill/decode timings from gateway-level TTFT/ITL
- response/event persistence for `/v1/responses`

## Integration risks for migrating to M5

1. **Routing key migration**: existing model-alias-only registry must evolve to `organization_id + model + endpoint/modality` without breaking current clients.
2. **Credential migration**: existing `ml-` API keys do not carry tenant context; platform auth/IAM must map credentials to exactly one organization context or a controlled provider/admin override.
3. **Admin ownership migration**: current gateway CRUD APIs overlap with future auth/control-plane ownership and should be demoted to dev/bootstrap/internal operations.
4. **Hot-path cache migration**: Redis read-through registry lookups and PostgreSQL fallback need to be replaced with in-process snapshots while preserving safe startup/recovery behavior.
5. **Backend target migration**: current `model_id` rewrite must evolve into internal `ModelTarget` resolution with base model, adapter ID/version, and backend engine serving name.
6. **Replica-state migration**: current static backend URL + health boolean is far less expressive than target normalized replica state.
7. **Rate/admission semantics**: existing fail-open Redis behavior may be fine for dev, but paid/SLA/abuse-sensitive tiers need explicit fail-closed policy.
8. **Metrics cardinality**: new tenant/adapter/correlation dimensions must not become unbounded Prometheus labels.
9. **Streaming compatibility**: current clients use `/v1/chat/completions`; `/v1/responses` must be added without regressing existing non-streaming/SSE behavior.
10. **Control-plane dependency**: the sibling `control-plane` repo is currently empty, so M5 control-plane source-of-truth APIs/schemas/events need to be introduced from scratch or generated from an external source.

## Recommended next implementation order

1. Keep this audit as the M5-P1-T01 baseline.
2. Define gateway/control-plane/auth ownership split and explicitly classify existing admin CRUD as dev/bootstrap/internal-only.
3. Introduce internal request/execution boundary types before adding more API surface.
4. Define `AuthContext` and organization-scoped policy model.
5. Replace model-alias hot-path routing with local in-process policy/routing snapshots backed by Redis Streams and full snapshot recovery.
6. Add tenant model binding and adapter target schemas in the control plane before implementing adapter-aware routing in the gateway.
