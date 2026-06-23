package vllm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nunocgoncalves/inference-gateway/internal/inference"
)

func TestExecutorChatCompletion(t *testing.T) {
	var gotPath string
	var gotRequestID string
	var gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRequestID = r.Header.Get("X-Request-ID")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("X-Test", "ok")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"model":"internal-model"}`))
	}))
	defer server.Close()

	executor := NewExecutor(server.Client())
	resp, err := executor.ChatCompletion(context.Background(), inference.ExecutionRequest{
		Body: []byte(`{"model":"alias"}`),
		Headers: http.Header{
			"X-Request-ID": []string{"req_123"},
		},
		Target:  inference.ModelTarget{BackendModelName: "internal-model"},
		Replica: inference.ReplicaTarget{URL: server.URL, EngineType: inference.EngineVLLM},
	})
	if err != nil {
		t.Fatalf("ChatCompletion returned error: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected chat completions path, got %q", gotPath)
	}
	if gotRequestID != "req_123" {
		t.Fatalf("expected request ID header, got %q", gotRequestID)
	}
	if gotBody != `{"model":"alias"}` {
		t.Fatalf("expected request body to be forwarded, got %q", gotBody)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d", http.StatusAccepted, resp.StatusCode)
	}
	if resp.Header.Get("X-Test") != "ok" {
		t.Fatalf("expected response header to be copied")
	}
	if string(resp.Body) != `{"model":"internal-model"}` {
		t.Fatalf("unexpected response body: %s", resp.Body)
	}
}

func TestExecutorResponses(t *testing.T) {
	paths := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	executor := NewExecutor(server.Client())
	req := inference.ExecutionRequest{Replica: inference.ReplicaTarget{URL: server.URL, EngineType: inference.EngineVLLM}}

	if _, err := executor.CreateResponse(context.Background(), req); err != nil {
		t.Fatalf("CreateResponse returned error: %v", err)
	}
	stream, err := executor.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse returned error: %v", err)
	}
	defer stream.Body.Close()

	if len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/responses" {
		t.Fatalf("expected responses path for both calls, got %v", paths)
	}
}

func TestEndpointTrimsTrailingSlash(t *testing.T) {
	got := endpoint("http://backend/", "/v1/chat/completions")
	if !strings.EqualFold(got, "http://backend/v1/chat/completions") {
		t.Fatalf("unexpected endpoint: %q", got)
	}
}
