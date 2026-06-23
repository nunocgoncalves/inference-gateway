// Package inference defines engine-agnostic boundaries for gateway data-plane
// execution and control-plane engine operations.
package inference

import (
	"context"
	"errors"
	"io"
	"net/http"
)

const (
	// EngineVLLM identifies the vLLM serving engine.
	EngineVLLM = "vllm"
	// EngineSGLang identifies the SGLang serving engine. It is a future target;
	// the boundary is intentionally engine-agnostic so SGLang can be added
	// without changing gateway routing policy or public API handlers.
	EngineSGLang = "sglang"
)

var (
	// ErrUnsupported is returned by engine clients for operations the engine or
	// deployment mode does not support.
	ErrUnsupported = errors.New("operation unsupported by inference engine")
)

// ModelTarget is the engine-agnostic result of tenant/model/adapter resolution.
// Customers request public model names; gateway routing resolves those names to
// a base model, optional adapter version, backend pool, and internal engine
// model/LoRA name before forwarding to a replica.
//
// Current standalone gateway deployments only populate PublicModelName,
// BaseModelID, and BackendModelName. M5 tenant/adapter routing will populate the
// adapter fields from local policy/routing snapshots.
type ModelTarget struct {
	PublicModelName  string
	BaseModelID      string
	AdapterID        string
	AdapterVersion   string
	BackendModelName string
	BackendPoolID    string
}

// EngineModelName returns the model name that should be sent to the backend
// engine. For LoRA-backed targets this is the engine-specific internal LoRA
// module name. For base-model-only targets it falls back to BaseModelID.
func (t ModelTarget) EngineModelName() string {
	if t.BackendModelName != "" {
		return t.BackendModelName
	}
	return t.BaseModelID
}

// ReplicaTarget identifies the concrete backend replica selected by routing.
type ReplicaTarget struct {
	ID            string
	URL           string
	BackendPoolID string
	EngineType    string
}

// ExecutionRequest is the raw OpenAI-compatible payload plus resolved target
// metadata handed from gateway handlers/routing to an engine executor.
type ExecutionRequest struct {
	Body    []byte
	Headers http.Header
	Target  ModelTarget
	Replica ReplicaTarget
}

// ExecutionResponse is a complete non-streaming engine response.
type ExecutionResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// ExecutionStream is a streaming engine response. The caller owns Body and must
// close it when done.
type ExecutionStream struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// Executor is the gateway data-plane boundary. Public API handlers and routing
// code depend on this interface rather than vLLM/SGLang protocol details.
type Executor interface {
	ChatCompletion(ctx context.Context, req ExecutionRequest) (*ExecutionResponse, error)
	StreamChatCompletion(ctx context.Context, req ExecutionRequest) (*ExecutionStream, error)

	CreateResponse(ctx context.Context, req ExecutionRequest) (*ExecutionResponse, error)
	StreamResponse(ctx context.Context, req ExecutionRequest) (*ExecutionStream, error)
}

// HealthStatus is normalized engine health as seen by a control client.
type HealthStatus struct {
	Healthy bool
	Message string
}

// AdapterArtifact describes an immutable adapter version used by lifecycle
// managers. StorageURI is for control-plane/lifecycle use and must not be
// exposed to customers.
type AdapterArtifact struct {
	AdapterID        string
	AdapterVersion   string
	BaseModelID      string
	BackendModelName string
	StorageURI       string
	Checksum         string
	SizeBytes        int64
	Format           string
}

// AdapterRef identifies an adapter version/name for engine control operations.
type AdapterRef struct {
	AdapterID        string
	AdapterVersion   string
	BackendModelName string
}

// LoadedAdapter is normalized loaded-adapter state returned by an engine
// control client.
type LoadedAdapter struct {
	AdapterID        string
	AdapterVersion   string
	BackendModelName string
	Status           string
}

// EngineMetrics is a normalized container for engine metrics. RawText keeps the
// first vLLM implementation useful before metric normalization is finalized.
type EngineMetrics struct {
	RawText string
	Values  map[string]float64
}

// EngineControlClient is the control-plane/lifecycle boundary. It is separate
// from Executor so adapter lifecycle and drain operations cannot accidentally be
// mixed into gateway request handling.
type EngineControlClient interface {
	Health(ctx context.Context, replica ReplicaTarget) (HealthStatus, error)
	LoadedAdapters(ctx context.Context, replica ReplicaTarget) ([]LoadedAdapter, error)
	LoadAdapter(ctx context.Context, replica ReplicaTarget, adapter AdapterArtifact) error
	UnloadAdapter(ctx context.Context, replica ReplicaTarget, adapter AdapterRef) error
	Metrics(ctx context.Context, replica ReplicaTarget) (EngineMetrics, error)
	Drain(ctx context.Context, replica ReplicaTarget) error
}
