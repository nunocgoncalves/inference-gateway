package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/inference"
)

const (
	healthPath            = "/health"
	modelsPath            = "/v1/models"
	metricsPath           = "/metrics"
	loadLoRAAdapterPath   = "/v1/load_lora_adapter"
	unloadLoRAAdapterPath = "/v1/unload_lora_adapter"
)

// ControlClient implements lifecycle/control operations for vLLM. These
// operations are intended for control-plane/lifecycle services, not gateway
// request handlers.
type ControlClient struct {
	client *http.Client
}

var _ inference.EngineControlClient = (*ControlClient)(nil)

// NewControlClient creates a vLLM control client. If client is nil, a default
// short-timeout control client is used.
func NewControlClient(client *http.Client) *ControlClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &ControlClient{client: client}
}

// Health checks the vLLM /health endpoint.
func (c *ControlClient) Health(ctx context.Context, replica inference.ReplicaTarget) (inference.HealthStatus, error) {
	resp, err := c.get(ctx, replica, healthPath)
	if err != nil {
		return inference.HealthStatus{Healthy: false, Message: err.Error()}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return inference.HealthStatus{
			Healthy: false,
			Message: fmt.Sprintf("vLLM health returned HTTP %d", resp.StatusCode),
		}, nil
	}

	return inference.HealthStatus{Healthy: true, Message: "ok"}, nil
}

// LoadedAdapters returns a best-effort list of currently exposed vLLM model IDs.
// vLLM exposes LoRA modules through the OpenAI-compatible models endpoint in
// common serving modes; lifecycle services can refine this normalization later.
func (c *ControlClient) LoadedAdapters(ctx context.Context, replica inference.ReplicaTarget) ([]inference.LoadedAdapter, error) {
	resp, err := c.get(ctx, replica, modelsPath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vLLM models returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding vLLM models response: %w", err)
	}

	adapters := make([]inference.LoadedAdapter, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID == "" {
			continue
		}
		adapters = append(adapters, inference.LoadedAdapter{
			BackendModelName: item.ID,
			Status:           "loaded",
		})
	}
	return adapters, nil
}

// LoadAdapter asks vLLM to load a LoRA adapter. This should be called by the
// lifecycle manager after control-plane validation, never by request handling.
func (c *ControlClient) LoadAdapter(ctx context.Context, replica inference.ReplicaTarget, adapter inference.AdapterArtifact) error {
	if adapter.BackendModelName == "" {
		return fmt.Errorf("backend model name is required to load vLLM LoRA adapter")
	}
	if adapter.StorageURI == "" {
		return fmt.Errorf("storage URI is required to load vLLM LoRA adapter")
	}

	payload := map[string]string{
		"lora_name": adapter.BackendModelName,
		"lora_path": adapter.StorageURI,
	}
	return c.postJSON(ctx, replica, loadLoRAAdapterPath, payload)
}

// UnloadAdapter asks vLLM to unload a LoRA adapter. This should be called by
// the lifecycle manager, never by request handling.
func (c *ControlClient) UnloadAdapter(ctx context.Context, replica inference.ReplicaTarget, adapter inference.AdapterRef) error {
	if adapter.BackendModelName == "" {
		return fmt.Errorf("backend model name is required to unload vLLM LoRA adapter")
	}

	payload := map[string]string{"lora_name": adapter.BackendModelName}
	return c.postJSON(ctx, replica, unloadLoRAAdapterPath, payload)
}

// Metrics fetches raw Prometheus metrics from vLLM. Normalization into bounded
// platform fields belongs in the routing-state collector.
func (c *ControlClient) Metrics(ctx context.Context, replica inference.ReplicaTarget) (inference.EngineMetrics, error) {
	resp, err := c.get(ctx, replica, metricsPath)
	if err != nil {
		return inference.EngineMetrics{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return inference.EngineMetrics{}, fmt.Errorf("vLLM metrics returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return inference.EngineMetrics{}, fmt.Errorf("reading vLLM metrics: %w", err)
	}
	return inference.EngineMetrics{RawText: string(body)}, nil
}

// Drain is intentionally not implemented through vLLM in this gateway package.
// Production drain is a platform/lifecycle workflow that marks replicas
// non-routable before engine/process operations.
func (c *ControlClient) Drain(ctx context.Context, replica inference.ReplicaTarget) error {
	return inference.ErrUnsupported
}

func (c *ControlClient) get(ctx context.Context, replica inference.ReplicaTarget, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(replica.URL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("creating vLLM control request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing vLLM control request: %w", err)
	}
	return resp, nil
}

func (c *ControlClient) postJSON(ctx context.Context, replica inference.ReplicaTarget, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding vLLM control request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(replica.URL, path), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating vLLM control request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("executing vLLM control request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("vLLM control %s returned HTTP %d: %s", path, resp.StatusCode, message)
	}
	return nil
}
