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

	"github.com/nunocgoncalves/inference-gateway/internal/middleware"
	"github.com/nunocgoncalves/inference-gateway/internal/ratelimit"
	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

const tpmBatchInterval = 500 * time.Millisecond

// handleStreaming proxies a streaming SSE response from the backend to the
// client, flushing each chunk immediately. It injects continuous_usage_stats
// for live per-identity TPM tracking and rewrites model names. Also records
// TTFT, ITL, active_streams, backend_request_duration, and token metrics.
//
//nolint:gocyclo // SSE streaming orchestration; extraction tracked separately.
func (h *Handler) handleStreaming(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	entry *snapshot.CatalogEntry,
	identityID string,
) {
	body = injectStreamOptions(body)

	backendURL := fmt.Sprintf("%s/v1/chat/completions", entry.BackendURL)

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, backendURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create proxy request", "server_error")
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	if v := middleware.RequestIDFromContext(r.Context()); v != "" {
		proxyReq.Header.Set("X-Request-ID", v)
	}

	backendStart := time.Now()
	resp, err := h.client.Do(proxyReq)
	if err != nil {
		h.logger.Error("streaming backend request failed",
			"backend", entry.BackendURL, "model", entry.ModelID, "error", err)
		writeError(w, http.StatusBadGateway, "backend request failed", "backend_error")
		return
	}
	defer resp.Body.Close()

	if h.metrics != nil {
		h.metrics.BackendRequestDuration.WithLabelValues(entry.ModelID, entry.BackendURL).
			Observe(time.Since(backendStart).Seconds())
	}

	// If backend returns non-200, forward the error as-is (not SSE).
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		respBody = rewriteModelInResponse(respBody, entry.ModelID)
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(respBody); err != nil {
			h.logger.Error("failed to write backend error response", "model", entry.ModelID, "error", err)
		}
		return
	}

	if h.metrics != nil {
		h.metrics.ActiveStreams.WithLabelValues(entry.ModelID).Inc()
		defer h.metrics.ActiveStreams.WithLabelValues(entry.ModelID).Dec()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported", "server_error")
		return
	}

	// TPM batcher keyed by identity_id (shared across the identity's keys + pods).
	var batcher *ratelimit.TPMBatcher
	if h.limiter != nil && identityID != "" {
		batcher = ratelimit.NewTPMBatcher(h.limiter, identityID, tpmBatchInterval)
		batcher.Start()
		defer batcher.Stop(context.Background())
	}

	var (
		prevTotalTokens int
		lastChunkAt     time.Time
		firstContentAt  time.Time
		gotFirstContent bool
		finalUsage      *streamUsage
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		select {
		case <-r.Context().Done():
			return
		default:
		}

		if !strings.HasPrefix(line, "data: ") {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		chunk, rewritten := processStreamChunk(data, entry.ModelID)

		now := time.Now()
		if chunk != nil && chunkHasContent(data) {
			if !gotFirstContent {
				gotFirstContent = true
				firstContentAt = now
				if h.metrics != nil {
					h.metrics.TimeToFirstToken.WithLabelValues(entry.ModelID).
						Observe(now.Sub(backendStart).Seconds())
				}
			} else if h.metrics != nil && !lastChunkAt.IsZero() {
				h.metrics.InterTokenLatency.WithLabelValues(entry.ModelID).
					Observe(now.Sub(lastChunkAt).Seconds())
			}
			lastChunkAt = now
		}

		if chunk != nil && chunk.Usage != nil {
			finalUsage = chunk.Usage
			if batcher != nil {
				totalTokens := chunk.Usage.TotalTokens
				delta := totalTokens - prevTotalTokens
				if delta > 0 {
					batcher.Add(delta)
					prevTotalTokens = totalTokens
				}
			}
		}

		fmt.Fprintf(w, "data: %s\n\n", rewritten)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		h.logger.Error("error reading streaming response",
			"backend", entry.BackendURL, "model", entry.ModelID, "error", err)
	}

	if h.metrics != nil && finalUsage != nil {
		streamEnd := time.Now()
		streamDuration := streamEnd.Sub(backendStart).Seconds()
		promptDuration := streamDuration
		decodeDuration := streamDuration
		if !firstContentAt.IsZero() {
			promptDuration = firstContentAt.Sub(backendStart).Seconds()
			decodeDuration = streamEnd.Sub(firstContentAt).Seconds()
		}
		if finalUsage.PromptTokens > 0 {
			h.metrics.PromptTokensTotal.WithLabelValues(entry.ModelID).Add(float64(finalUsage.PromptTokens))
			h.metrics.TokensPerRequest.WithLabelValues(entry.ModelID, "prompt").Observe(float64(finalUsage.PromptTokens))
		}
		if finalUsage.CompletionTokens > 0 {
			h.metrics.CompletionTokensTotal.WithLabelValues(entry.ModelID).Add(float64(finalUsage.CompletionTokens))
			h.metrics.TokensPerRequest.WithLabelValues(entry.ModelID, "completion").Observe(float64(finalUsage.CompletionTokens))
		}
		if promptDuration > 0 && finalUsage.PromptTokens > 0 {
			h.metrics.TokensPerSecond.WithLabelValues(entry.ModelID, "prompt").Observe(float64(finalUsage.PromptTokens) / promptDuration)
		}
		if decodeDuration > 0 && finalUsage.CompletionTokens > 0 {
			completionTokensPerSecond := float64(finalUsage.CompletionTokens) / decodeDuration
			h.metrics.TokensPerSecond.WithLabelValues(entry.ModelID, "completion").Observe(completionTokensPerSecond)
			h.metrics.CompletionTokensPerSecondByPromptBucket.
				WithLabelValues(entry.ModelID, promptTokensBucket(finalUsage.PromptTokens)).
				Observe(completionTokensPerSecond)
		}
	}
}

type streamChunk struct {
	Usage *streamUsage `json:"usage,omitempty"`
}

type streamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func processStreamChunk(data string, aliasName string) (*streamChunk, string) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return nil, data
	}
	if _, ok := parsed["model"]; ok {
		parsed["model"] = aliasName
	}
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return nil, data
	}
	var chunk streamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, data
	}
	return &chunk, string(rewritten)
}

func chunkHasContent(data string) bool {
	var parsed struct {
		Choices []struct {
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return false
	}
	for _, c := range parsed.Choices {
		if c.Delta.Content != nil && *c.Delta.Content != "" {
			return true
		}
	}
	return false
}

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

func promptTokensBucket(tokens int) string {
	switch {
	case tokens <= 1024:
		return "le_1k"
	case tokens <= 2048:
		return "1k_2k"
	case tokens <= 4096:
		return "2k_4k"
	case tokens <= 8192:
		return "4k_8k"
	case tokens <= 16384:
		return "8k_16k"
	case tokens <= 32768:
		return "16k_32k"
	case tokens <= 65536:
		return "32k_64k"
	default:
		return "gt_64k"
	}
}
