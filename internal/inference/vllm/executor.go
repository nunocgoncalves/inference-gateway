// Package vllm implements inference boundaries for vLLM-compatible engines.
package vllm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/inference"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	responsesPath       = "/v1/responses"
)

// Executor executes OpenAI-compatible data-plane requests against vLLM.
type Executor struct {
	client *http.Client
}

var _ inference.Executor = (*Executor)(nil)

// NewExecutor creates a vLLM executor. If client is nil, a default client with
// a long inference timeout is used.
func NewExecutor(client *http.Client) *Executor {
	if client == nil {
		client = &http.Client{
			Timeout: 300 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Executor{client: client}
}

// ChatCompletion executes a non-streaming chat completion request.
func (e *Executor) ChatCompletion(ctx context.Context, req inference.ExecutionRequest) (*inference.ExecutionResponse, error) {
	return e.do(ctx, req, chatCompletionsPath)
}

// StreamChatCompletion executes a streaming chat completion request.
func (e *Executor) StreamChatCompletion(ctx context.Context, req inference.ExecutionRequest) (*inference.ExecutionStream, error) {
	return e.doStream(ctx, req, chatCompletionsPath)
}

// CreateResponse executes a non-streaming OpenAI-compatible Responses request.
func (e *Executor) CreateResponse(ctx context.Context, req inference.ExecutionRequest) (*inference.ExecutionResponse, error) {
	return e.do(ctx, req, responsesPath)
}

// StreamResponse executes a streaming OpenAI-compatible Responses request.
func (e *Executor) StreamResponse(ctx context.Context, req inference.ExecutionRequest) (*inference.ExecutionStream, error) {
	return e.doStream(ctx, req, responsesPath)
}

func (e *Executor) do(ctx context.Context, req inference.ExecutionRequest, path string) (*inference.ExecutionResponse, error) {
	resp, err := e.doHTTP(ctx, req, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading vLLM response: %w", err)
	}

	return &inference.ExecutionResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       body,
	}, nil
}

func (e *Executor) doStream(ctx context.Context, req inference.ExecutionRequest, path string) (*inference.ExecutionStream, error) {
	resp, err := e.doHTTP(ctx, req, path)
	if err != nil {
		return nil, err
	}

	return &inference.ExecutionStream{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       resp.Body,
	}, nil
}

func (e *Executor) doHTTP(ctx context.Context, req inference.ExecutionRequest, path string) (*http.Response, error) {
	backendURL := endpoint(req.Replica.URL, path)
	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, backendURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("creating vLLM request: %w", err)
	}

	copyHeaders(proxyReq.Header, req.Headers)
	if proxyReq.Header.Get("Content-Type") == "" {
		proxyReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := e.client.Do(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("executing vLLM request: %w", err)
	}
	return resp, nil
}

func endpoint(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
