package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nunocgoncalves/inference-gateway/internal/auth"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

const tpmBatchInterval = 500 * time.Millisecond

// handleStreaming proxies a streaming SSE response from vLLM to the client,
// flushing each chunk immediately (no buffering). It injects
// continuous_usage_stats for live TPM tracking and rewrites model names.
func (h *Handler) handleStreaming(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	backend *registry.Backend,
	model *registry.Model,
) {
	// Inject stream_options for continuous usage stats.
	body = injectStreamOptions(body)

	backendURL := fmt.Sprintf("%s/v1/chat/completions", backend.URL)

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, backendURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create proxy request", "server_error")
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("X-Request-ID"); v != "" {
		proxyReq.Header.Set("X-Request-ID", v)
	}

	resp, err := h.client.Do(proxyReq)
	if err != nil {
		h.logger.Error("streaming backend request failed",
			"backend", backend.URL, "model", model.Name, "error", err)
		if h.hc != nil {
			h.hc.ReportFailure(backend.ID)
		}
		writeError(w, http.StatusBadGateway, "backend request failed", "backend_error")
		return
	}
	defer resp.Body.Close()

	// If backend returns non-200, forward the error as-is (not SSE).
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		respBody = rewriteModelInResponse(respBody, model.Name)
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported", "server_error")
		return
	}

	// Set up TPM batcher for live token tracking.
	var batcher *ratelimit.TPMBatcher
	apiKey := auth.APIKeyFromContext(r.Context())
	if apiKey != nil && h.limiter != nil {
		batcher = ratelimit.NewTPMBatcher(h.limiter, apiKey.ID, tpmBatchInterval)
		batcher.Start()
		defer batcher.Stop(context.Background())
	}

	// Stream SSE chunks from backend to client.
	var prevTotalTokens int
	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer size for long lines (e.g., large content chunks).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// Check for client disconnect.
		select {
		case <-r.Context().Done():
			return
		default:
		}

		if !strings.HasPrefix(line, "data: ") {
			// Forward empty lines (SSE event separators) and comments.
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		// Handle [DONE] sentinel.
		if data == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		// Parse the chunk to rewrite model name and extract usage.
		chunk, rewritten := processStreamChunk(data, model.Name)

		// Track TPM from continuous usage stats.
		if chunk != nil && chunk.Usage != nil && batcher != nil {
			totalTokens := chunk.Usage.TotalTokens
			delta := totalTokens - prevTotalTokens
			if delta > 0 {
				batcher.Add(delta)
				prevTotalTokens = totalTokens
			}
		}

		fmt.Fprintf(w, "data: %s\n\n", rewritten)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		h.logger.Error("error reading streaming response",
			"backend", backend.URL, "model", model.Name, "error", err)
	}
}

// streamChunk is a minimal parse of an SSE chunk — just enough to extract
// usage and the model field for rewriting.
type streamChunk struct {
	Usage *streamUsage `json:"usage,omitempty"`
}

type streamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// processStreamChunk parses a chunk, rewrites the model name, and returns
// the parsed chunk (for usage extraction) plus the rewritten JSON string.
func processStreamChunk(data string, aliasName string) (*streamChunk, string) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		// Can't parse — forward as-is.
		return nil, data
	}

	// Rewrite model name.
	if _, ok := parsed["model"]; ok {
		parsed["model"] = aliasName
	}

	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return nil, data
	}

	// Extract usage for TPM tracking.
	var chunk streamChunk
	json.Unmarshal([]byte(data), &chunk)

	return &chunk, string(rewritten)
}

// injectStreamOptions ensures stream_options.include_usage and
// stream_options.continuous_usage_stats are set to true, so we get
// token counts for TPM tracking.
func injectStreamOptions(body []byte) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}

	streamOpts, ok := parsed["stream_options"].(map[string]any)
	if !ok {
		streamOpts = make(map[string]any)
	}
	streamOpts["include_usage"] = true
	streamOpts["continuous_usage_stats"] = true
	parsed["stream_options"] = streamOpts

	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}
