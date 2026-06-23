# M5-P1-T03 — Inference execution and engine-control boundaries

Date: 2026-06-22
Ticket: HOR-118 / M5-P1-T03
Scope: engine-agnostic gateway data-plane and control-plane engine lifecycle boundaries.

## Summary

This ticket introduces the initial boundary between:

1. **Gateway public API/routing/transform/usage code**, which should remain engine-agnostic.
2. **Data-plane engine executors**, which know how to call a specific backend protocol for inference requests.
3. **Control-plane engine-control clients**, which know how to perform lifecycle/inspection operations such as health, loaded adapters, load/unload, metrics, and drain.

The first implementation target is vLLM. SGLang can be added later by implementing the same interfaces. TensorRT-LLM is explicitly excluded from target backend options because multimodal support is required.

## Added packages

```text
internal/inference
internal/inference/vllm
```

### `internal/inference`

Defines engine-agnostic contracts and shared target/state types.

Key data-plane types:

```go
type ModelTarget struct {
    PublicModelName  string
    BaseModelID      string
    AdapterID        string
    AdapterVersion   string
    BackendModelName string
    BackendPoolID    string
}

type ReplicaTarget struct {
    ID            string
    URL           string
    BackendPoolID string
    EngineType    string
}
```

`ModelTarget.EngineModelName()` returns the internal engine model/LoRA name used in request rewrites. Current standalone gateway behavior maps aliases directly to `model_id`, while future M5 routing will populate adapter fields from local tenant policy/routing snapshots.

Key data-plane interface:

```go
type Executor interface {
    ChatCompletion(ctx context.Context, req ExecutionRequest) (*ExecutionResponse, error)
    StreamChatCompletion(ctx context.Context, req ExecutionRequest) (*ExecutionStream, error)

    CreateResponse(ctx context.Context, req ExecutionRequest) (*ExecutionResponse, error)
    StreamResponse(ctx context.Context, req ExecutionRequest) (*ExecutionStream, error)
}
```

This supports both `/v1/chat/completions` and OpenAI-compatible `/v1/responses`, each in non-streaming and streaming form.

Key control-plane interface:

```go
type EngineControlClient interface {
    Health(ctx context.Context, replica ReplicaTarget) (HealthStatus, error)
    LoadedAdapters(ctx context.Context, replica ReplicaTarget) ([]LoadedAdapter, error)
    LoadAdapter(ctx context.Context, replica ReplicaTarget, adapter AdapterArtifact) error
    UnloadAdapter(ctx context.Context, replica ReplicaTarget, adapter AdapterRef) error
    Metrics(ctx context.Context, replica ReplicaTarget) (EngineMetrics, error)
    Drain(ctx context.Context, replica ReplicaTarget) error
}
```

This interface is separate from request execution so adapter lifecycle cannot be mixed into ordinary gateway request handling.

### `internal/inference/vllm`

Provides the first implementation target:

- `vllm.Executor`
  - `ChatCompletion` -> `POST /v1/chat/completions`
  - `StreamChatCompletion` -> `POST /v1/chat/completions`
  - `CreateResponse` -> `POST /v1/responses`
  - `StreamResponse` -> `POST /v1/responses`
- `vllm.ControlClient`
  - `Health` -> `GET /health`
  - `LoadedAdapters` -> best-effort normalization from `GET /v1/models`
  - `LoadAdapter` -> `POST /v1/load_lora_adapter`
  - `UnloadAdapter` -> `POST /v1/unload_lora_adapter`
  - `Metrics` -> raw `GET /metrics`
  - `Drain` -> currently returns `inference.ErrUnsupported`

Control operations are intended for the control-plane/lifecycle manager, not gateway request handlers.

## Proxy wiring change

The existing `/v1/chat/completions` handler now forwards through `inference.Executor` instead of constructing direct vLLM HTTP requests in the proxy handler.

Current behavior remains the same:

1. Authenticate API key.
2. Check allowed model alias.
3. Read the registry model.
4. Select backend with existing weighted round-robin.
5. Build `ModelTarget` from current model registry data.
6. Build `ReplicaTarget` from the selected backend.
7. Apply existing transforms.
8. Rewrite request `model` to `ModelTarget.EngineModelName()`.
9. Execute through the vLLM data-plane executor.
10. Preserve gateway-side response rewrite, streaming processing, usage extraction, TPM tracking, and metrics.

The current adapter fields are empty because tenant/adapter policy does not exist yet. The shape is now ready for:

```text
organization_id + public_model_name + endpoint/modality
-> base_model_id
-> adapter_id/version
-> backend_pool_id
-> BackendModelName
```

## Adapter lifecycle rule

Adapter load/unload is deliberately part of `EngineControlClient`, not `Executor` and not public API handler code.

Ordinary gateway request handling must not call:

- `LoadAdapter`
- `UnloadAdapter`
- adapter warm/pin/evict operations
- engine drain operations

Those actions belong to the control-plane lifecycle manager after validation and desired-state reconciliation.

## SGLang extension path

SGLang can be added later by implementing:

```text
internal/inference/Executor
internal/inference/EngineControlClient
```

No gateway public API handler or routing policy should need to change if the target/routing snapshots resolve `ReplicaTarget.EngineType = "sglang"` and the executor selection layer chooses the SGLang implementation.

A future ticket should add an executor registry/factory once multiple engine implementations exist.

## TensorRT-LLM exclusion

TensorRT-LLM is not a target backend for this platform path because multimodal support is required. The current boundary intentionally names vLLM first and SGLang as a future/benchmark path only.

## Validation

Ran:

```text
cd inference-gateway && go test ./internal/inference/... ./internal/config ./internal/loadbalancer ./internal/metrics ./internal/middleware ./internal/proxy -run 'TestExecutor|TestControl|TestEndpoint|TestRewrite|TestApply|TestProcess|TestChunk|TestInject'
make -C inference-gateway build
```

Both passed.

## Acceptance criteria mapping

- Data-plane interface supports chat completions and OpenAI-compatible responses, streaming and non-streaming: `internal/inference.Executor`.
- Control-plane engine-control interface is separate and supports health, loaded adapters, load/unload, metrics, and drain where supported: `internal/inference.EngineControlClient`.
- vLLM executor/control client is the first implementation target: `internal/inference/vllm`.
- SGLang can be added later without changing gateway routing policy or public API handlers: same interface contracts; future executor selection layer needed when SGLang is implemented.
- TensorRT-LLM excluded because multimodal support is required: documented above.
- Request rewrite uses engine-agnostic `ModelTarget`: proxy now rewrites through `ModelTarget.EngineModelName()`.
