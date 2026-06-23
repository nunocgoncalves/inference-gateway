# M5-P1-T02 — Production ownership split

Date: 2026-06-22
Ticket: HOR-117 / M5-P1-T02
Scope: production ownership boundaries between auth/IAM, resource/control plane, routing-state/lifecycle services, and inference gateway.

## Summary

The production M5 inference platform is split into four ownership domains:

1. **Auth/IAM** owns identities, credentials, memberships, revocation, capabilities/scopes, and policy generation.
2. **Resource/control plane** owns desired state for model catalog, tenant model bindings, adapters, rollout, placement, backend pools, and backend/adapter lifecycle intent.
3. **Routing-state collector and lifecycle manager** owns observed replica state, engine-specific adapter operations, reconciliation, and operational workflows such as drain/prewarm/re-admit.
4. **Inference gateway** owns hot-path request handling, local snapshot enforcement, tenant/model/adapter resolution, adapter-aware routing, proxy/streaming execution, `/v1/responses` persistence, usage extraction, and local overlays.

The gateway must not become the production source of truth for identities, tenant membership, model catalog, adapter artifacts, rollout policies, placement policy, or backend desired state.

## Hard request-path invariant

Ordinary inference request handling must not perform synchronous network calls to auth/IAM, the resource/control plane, or the routing-state collector.

Allowed request-path data sources:

- in-process immutable/RCU-style policy, model, adapter, rollout, placement, and replica-state snapshots
- local in-flight/session-affinity overlays
- Redis or equivalent centralized counters for rate limits, token accounting, and admission/concurrency where policy explicitly allows it
- gateway-owned persistence for `/v1/responses`, conversations/messages, response events, and usage records where required by the endpoint semantics

Disallowed ordinary request-path data sources:

- auth/IAM credential or membership lookup calls
- control-plane model/adapter/rollout lookup calls
- PostgreSQL reads of auth/control-plane source tables
- Redis read-through policy/model routing lookups
- synchronous adapter load/unload/warm calls to an inference engine

If correctness-sensitive snapshots are stale, missing, or have version gaps, the gateway should fail closed for affected requests or mark itself unready according to environment/tier policy.

## Ownership matrix

| Capability / state | Auth/IAM | Resource / control plane | Routing-state collector / lifecycle manager | Inference gateway |
| --- | --- | --- | --- | --- |
| Users and human identity | Owns | Reads via explicit views only if needed | No ownership | No direct ownership |
| Sessions and login state | Owns | No ownership | No ownership | No direct ownership |
| JWT/JWKS validation material | Owns and publishes | May consume for control APIs | No ownership | Consumes via local snapshot/config, not request-path fetch |
| Personal API keys | Owns | No ownership | No ownership | Validates/enforces from local credential policy snapshot |
| Service accounts | Owns | Consumes subject identity for control mutations | May use service identity | Enforces from local credential policy snapshot |
| Workload identities | Owns | Consumes subject identity for control mutations | May use service identity | Enforces from local credential policy snapshot |
| Credential revocation | Owns and publishes policy/version changes | No ownership | No ownership | Enforces local revocation snapshot; fail closed if stale where required |
| Capabilities/scopes | Owns and publishes | Uses for control-plane authorization | No ownership | Enforces local capabilities/scopes snapshot |
| Organization/project membership | Owns and publishes | Uses to validate catalog/binding mutations | No ownership | Resolves exactly one `organization_id` from local snapshot; optional `project_id` |
| Tenant policy generation | Owns auth-derived policy | May compose resource policy into published routing/policy views | No ownership | Consumes flattened local policy snapshot |
| Public model catalog | No ownership | Owns | No ownership | Reads flattened local model snapshot |
| Tenant model bindings | No ownership | Owns | No ownership | Resolves from local binding snapshot |
| Adapter artifacts and immutable versions | No ownership | Owns metadata and desired status | Observes/load-validates operational status | Reads local adapter target snapshot only |
| Adapter artifact storage URI/checksum | No ownership | Owns | Uses for lifecycle operations | Must not expose to clients; normally not needed on hot path |
| Adapter rollout policy | No ownership | Owns stable/candidate policy and rollback decisions | Publishes observed health/resource signals | Performs sticky assignment from local rollout snapshot |
| Adapter placement policy | No ownership | Owns desired placement tiers/min/max hot replicas | Reconciles actual placement | Reads hotness/availability from replica snapshot |
| Backend pools | No ownership | Owns desired pools, base model, modalities, engine type | Observes replicas in pools | Routes from local pool/replica snapshots |
| Backend replica desired state | No ownership | Owns desired replica lifecycle state | Reconciles and reports observed state | Treats only serving/fresh/eligible replicas as routable |
| Observed replica state | No ownership | Consumes aggregated state | Owns collection/normalization/publication | Consumes local replica-state snapshot |
| Adapter load/unload/warm/pin/evict | No ownership | Specifies desired placement/lifecycle | Owns engine-specific execution and reconciliation | Must not call synchronously on request path |
| Drain/restart/prewarm/re-admit workflows | No ownership | Owns desired operation intent/policy | Owns operational reconciliation | Stops routing to draining/non-serving replicas from local snapshot |
| OpenAI-compatible request handling | No ownership | No ownership | No ownership | Owns |
| `/v1/chat/completions` proxy/streaming | No ownership | No ownership | No ownership | Owns |
| `/v1/responses` endpoint behavior | No ownership | Provides model/policy views | No ownership | Owns external behavior and persistence |
| Conversation/message/response records | No ownership | No ownership except future product views | No ownership | Owns gateway DB writes |
| Durable inference usage records | No ownership | May consume for billing/analytics | May consume operational aggregates | Owns extraction and writing at request completion/streaming finalization |
| Local session/workflow affinity overlay | No ownership | No ownership | May consume aggregate routing outcomes | Owns local overlay; not source of truth |
| Gateway operational observations | No ownership | May consume | May consume | Owns emission/writes where applicable |
| Production admin CRUD | Owns identity/admin authorization | Owns primary model/backend/adapter mutations | Owns operational lifecycle APIs | Limited to internal, narrow, dev/bootstrap, or break-glass paths |

## Auth/IAM responsibilities

Auth/IAM is the authoritative owner for:

- users
- login sessions
- JWT/JWKS material and validation semantics
- personal API keys
- service accounts
- workload identities
- credential lifecycle, expiration, and revocation
- organization and project membership
- subject capabilities and scopes
- provider/admin override authorization
- auth-derived policy generation and versioning

Auth/IAM publishes flattened credential/policy views for the gateway. The gateway consumes those views as local snapshots.

### Auth/IAM output to gateway

Auth/IAM should publish or contribute to a gateway policy snapshot containing at least:

```text
credential_fingerprint
credential_type
subject_type
subject_id
organization_id              required unless provider/admin override is explicit
project_id                   optional
capabilities/scopes
allowed_models/endpoints
rate_limit_policy_ref
admission_policy_ref
tenant_tier/SLA
revocation_version
policy_version
```

### Auth/IAM non-responsibilities

Auth/IAM does not own:

- model catalog
- tenant model bindings
- adapter artifacts or versions
- rollout policy
- placement policy
- backend pools
- observed replica state
- gateway request proxying
- response/conversation persistence

## Resource/control-plane responsibilities

The resource/control plane is the authoritative owner for desired inference serving state:

- public model catalog
- model availability by organization/project/endpoint/modality
- tenant model bindings
- base model mapping
- adapter artifact records
- immutable adapter versions
- active/candidate adapter selection
- rollout policy, rollout salt, rollout state, and rollback decisions
- adapter placement policy
- backend pools
- backend replica desired state
- desired adapter lifecycle state
- full snapshot APIs and Redis Streams event publication for gateway consumption

### Resource/control-plane source-of-truth records

Recommended authoritative records include:

```text
model_catalog_entries
tenant_model_bindings
adapter_artifacts
adapter_versions
adapter_rollout_policies
adapter_placement_policies
backend_pools
backend_replicas_desired_state
routing_policy_snapshots
```

### Resource/control-plane output to gateway

The control plane publishes flattened, denormalized gateway views such as:

```text
credential/resource policy joins
organization_id + public_model_name + endpoint/modality bindings
public model visibility per tenant/project/capability
model target metadata
rollout assignment inputs
backend pool metadata
fallback policy
rate/admission policy refs
snapshot version and stream positions
```

The gateway reads these through:

1. startup full snapshot API
2. Redis Streams deltas
3. full snapshot recovery after gaps, reconnects, invalid deltas, or staleness

The gateway should not read normalized control-plane source tables directly on the inference hot path.

### Resource/control-plane non-responsibilities

The resource/control plane does not own:

- user/session credential lifecycle
- JWT/JWKS issuance
- request proxy execution
- SSE streaming mechanics
- local per-gateway in-flight routing overlay
- `/v1/responses` persistence details, except consuming resulting records/views where needed

## Routing-state collector and lifecycle-manager responsibilities

The routing-state collector and lifecycle manager form the operational bridge between desired control-plane state and observed engine state.

They own:

- polling/scraping vLLM/SGLang health endpoints and metrics
- normalizing engine-specific state into platform `ReplicaState`
- tracking loaded adapters per replica
- measuring queue depth, running/waiting requests, KV usage, GPU memory pressure, prefill/decode pressure, throughput, and staleness
- publishing compact replica-state snapshots/events
- adapter load/unload/warm/pin/evict reconciliation
- honoring placement policies such as pinned/warm/on-demand and `min_hot_replicas`
- backend drain/restart/prewarm/re-admit workflows
- verifying replicas after restart before marking them serving

### Routing-state output to gateway

The gateway consumes local snapshots with fields such as:

```text
replica_id
backend_pool_id
engine_type
base_model_id
supported_modalities
serving_state: serving | warming | draining | disabled
health
loaded_adapters
warming_adapters
pinned_adapters
queue_depth
running_requests
waiting_requests
kv_cache_usage
gpu_memory_usage
prefill_load
decode_load
last_seen
state_version
```

A replica is routable only if it is healthy, serving, fresh, compatible, and hot for the required adapter/version. Process health alone is not enough.

### Lifecycle non-responsibilities

The lifecycle manager does not decide tenant identity, credential validity, or customer-visible model access. It reconciles desired operational state supplied by the control plane.

## Inference gateway responsibilities

The inference gateway owns the latency-sensitive data plane:

- OpenAI-compatible HTTP API handling
- request ID / trace correlation
- local AuthContext enforcement from snapshots
- tenant/model/adapter resolution from local snapshots
- rollout assignment from local rollout policy
- adapter-aware replica eligibility and scoring
- local in-flight and session/workflow affinity overlay
- request rewrite to internal backend model/LoRA names
- vLLM/SGLang executor abstraction for data-plane calls
- non-streaming proxy execution
- SSE streaming proxy execution
- `/v1/responses` external behavior
- conversation/message/response persistence
- response event persistence where required
- usage extraction and durable usage-record writes
- operational metrics/logging/tracing
- local short-circuit enforcement for clearly invalid/over-limit requests

### Gateway-owned database writes

Gateway write ownership is limited to gateway-owned state, including:

```text
conversations
conversation_messages
responses
response_events
inference_usage_records
gateway_request_observations
gateway_stream_observations
local operational observations where applicable
```

Gateway-owned records may include high-cardinality correlation and accounting fields:

```text
organization_id
project_id
subject_id
credential_fingerprint
public_model_name
base_model_id
adapter_id
adapter_version
rollout_assignment
backend_pool_id
replica_id
request_id
trace_id
span_id
workflow_id/session_id
conversation_id
response_id
latency stages
tokens in/out
error_code
created_at
```

These are durable records/log/trace fields, not default Prometheus labels.

### Gateway read model

The gateway may read:

- local in-process snapshots
- local gateway-owned DB state for `/v1/responses` and conversation/message APIs
- Redis counters for rate/admission/concurrency checks
- optional gateway-owned operational records needed for response/conversation semantics

The gateway must not read normalized auth/control-plane tables directly in the inference request path.

### Gateway non-responsibilities

The gateway does not own:

- credential issuance
- credential revocation source of truth
- organization/project membership source of truth
- public model catalog source of truth
- adapter artifacts or storage URIs
- rollout policy source of truth
- adapter placement policy source of truth
- backend desired state source of truth
- observed replica-state collection source of truth
- engine lifecycle reconciliation

## Current gateway admin CRUD classification

The current `inference-gateway` admin endpoints for models, backends, and API keys are classified as:

```text
local/dev/bootstrap only by default
```

They are useful for:

- local development
- integration tests
- bootstrap of standalone deployments
- smoke testing vLLM-compatible proxy behavior

They are not the target production mutation plane.

Production options:

1. remove them from production builds/deployments,
2. guard them behind internal network and service identity,
3. narrow them to gateway-owned operational functions only,
4. keep only break-glass endpoints with audit logging and explicit authorization,
5. route primary mutations through auth/IAM and control-plane APIs instead.

## Snapshot and event ownership

### Full snapshots

Control-plane/auth systems own snapshot generation. Gateway owns snapshot consumption and local readiness.

Startup sequence:

1. Gateway fetches full policy/routing snapshot.
2. Gateway validates version, completeness, and compatibility.
3. Gateway installs immutable local snapshot.
4. Gateway starts Redis Streams readers from the snapshot stream IDs.
5. Gateway becomes ready only after required snapshots are installed.

### Deltas

Redis Streams carry versioned deltas such as:

```text
routing:policy:events
routing:replica_state:events
```

Gateway responsibilities:

- apply deltas in order
- reject invalid/out-of-order deltas
- detect version gaps
- recover via full snapshot
- publish heartbeat/lag/readiness state

Control-plane responsibilities:

- publish monotonic versions
- expose full snapshot recovery APIs
- alert/drain/restart lagging gateways

## Production mutation flow examples

### Credential revocation

1. Auth/IAM revokes credential.
2. Auth/IAM publishes policy/revocation version delta.
3. Gateway applies local snapshot delta.
4. Gateway rejects future requests for that credential without calling Auth/IAM.
5. If revocation freshness exceeds policy threshold, gateway fails closed or becomes unready.

### Tenant model binding change

1. Control plane updates tenant binding.
2. Control plane publishes routing policy delta.
3. Gateway updates local binding snapshot.
4. Future requests resolve to the new base model/adapter/backend pool locally.

### Adapter rollout canary

1. Control plane creates candidate adapter version and rollout policy.
2. Lifecycle manager warms candidate on enough eligible replicas.
3. Routing-state collector publishes loaded adapter state.
4. Control plane marks rollout canary/ramping when warm criteria are met.
5. Gateway performs sticky stable/candidate assignment from local policy.
6. Gateway routes only to replicas hot for the assigned adapter version.

### Backend restart

1. Control plane requests drain/restart.
2. Lifecycle manager marks replica draining and reconciles engine operation.
3. Collector publishes draining state.
4. Gateway stops new assignments to that replica.
5. Existing streams drain until timeout.
6. Replica restarts and returns as warming, not serving.
7. Lifecycle manager prewarms required adapters.
8. Collector verifies health, base model, modalities, loaded adapters, and metrics.
9. Control plane/collector publishes serving state.
10. Gateway re-admits the replica from local snapshot.

## Least-privilege data access

Gateway service credentials should be scoped to:

- read full policy/routing snapshots
- read Redis Streams events
- read/write Redis counters needed for rate/admission/concurrency
- write gateway-owned persistence records
- emit metrics/logs/traces

Gateway service credentials should not be able to:

- mutate user/session/membership state
- create or revoke credentials directly, except local/dev/bootstrap mode
- mutate model catalog source of truth
- mutate adapter artifacts/versions
- mutate rollout/placement policy
- directly load/unload adapters on production engines from request handlers

## Acceptance criteria mapping

- Auth/IAM ownership of users, sessions, JWT/JWKS, keys, service accounts, workload identities, revocation, policy generation, capabilities/scopes, and memberships: covered in **Auth/IAM responsibilities**.
- Resource/control-plane ownership of catalog, bindings, artifacts/versions, rollout, placement, backend pools, and desired state: covered in **Resource/control-plane responsibilities**.
- Routing-state/lifecycle ownership of observed replica state, adapter reconciliation, and drain/prewarm/re-admit workflows: covered in **Routing-state collector and lifecycle-manager responsibilities**.
- Gateway ownership of hot-path snapshot enforcement, tenant/model/adapter resolution, adapter-aware routing, rewrite/proxy/streaming, `/v1/responses`, usage extraction, and overlays: covered in **Inference gateway responsibilities**.
- Gateway DB write ownership limited to conversations, messages, responses, usage records, and operational observations: covered in **Gateway-owned database writes**.
- Gateway direct reads of auth/control state only through explicit snapshots/views; no request-path auth/control service calls: covered in **Hard request-path invariant** and **Gateway read model**.
