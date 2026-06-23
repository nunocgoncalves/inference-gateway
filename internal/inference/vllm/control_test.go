package vllm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nunocgoncalves/inference-gateway/internal/inference"
)

func TestControlClientHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewControlClient(server.Client())
	status, err := client.Health(context.Background(), inference.ReplicaTarget{URL: server.URL})
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if !status.Healthy {
		t.Fatalf("expected healthy status")
	}
}

func TestControlClientLoadedAdapters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "base-model"},
				{"id": "lora_org_123_support_agent_v17"},
			},
		})
	}))
	defer server.Close()

	client := NewControlClient(server.Client())
	adapters, err := client.LoadedAdapters(context.Background(), inference.ReplicaTarget{URL: server.URL})
	if err != nil {
		t.Fatalf("LoadedAdapters returned error: %v", err)
	}
	if len(adapters) != 2 {
		t.Fatalf("expected 2 loaded entries, got %d", len(adapters))
	}
	if adapters[1].BackendModelName != "lora_org_123_support_agent_v17" || adapters[1].Status != "loaded" {
		t.Fatalf("unexpected adapter normalization: %+v", adapters[1])
	}
}

func TestControlClientLoadAndUnloadAdapter(t *testing.T) {
	seen := make(map[string]map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode payload: %v", err)
		}
		seen[r.URL.Path] = payload
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewControlClient(server.Client())
	replica := inference.ReplicaTarget{URL: server.URL}

	err := client.LoadAdapter(context.Background(), replica, inference.AdapterArtifact{
		BackendModelName: "lora_org_123_support_agent_v17",
		StorageURI:       "s3://bucket/adapter",
	})
	if err != nil {
		t.Fatalf("LoadAdapter returned error: %v", err)
	}

	err = client.UnloadAdapter(context.Background(), replica, inference.AdapterRef{
		BackendModelName: "lora_org_123_support_agent_v17",
	})
	if err != nil {
		t.Fatalf("UnloadAdapter returned error: %v", err)
	}

	load := seen["/v1/load_lora_adapter"]
	if load["lora_name"] != "lora_org_123_support_agent_v17" || load["lora_path"] != "s3://bucket/adapter" {
		t.Fatalf("unexpected load payload: %+v", load)
	}
	unload := seen["/v1/unload_lora_adapter"]
	if unload["lora_name"] != "lora_org_123_support_agent_v17" {
		t.Fatalf("unexpected unload payload: %+v", unload)
	}
}

func TestControlClientMetricsAndDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("vllm:num_requests_running 1\n"))
	}))
	defer server.Close()

	client := NewControlClient(server.Client())
	metrics, err := client.Metrics(context.Background(), inference.ReplicaTarget{URL: server.URL})
	if err != nil {
		t.Fatalf("Metrics returned error: %v", err)
	}
	if metrics.RawText != "vllm:num_requests_running 1\n" {
		t.Fatalf("unexpected metrics: %q", metrics.RawText)
	}

	err = client.Drain(context.Background(), inference.ReplicaTarget{URL: server.URL})
	if !errors.Is(err, inference.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported from Drain, got %v", err)
	}
}
