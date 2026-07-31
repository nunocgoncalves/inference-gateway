package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBufferLogger returns a logger that writes JSON to the provided buffer.
func newBufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// parseLogLine parses a single JSON log line into a map.
func parseLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	return m
}

func TestLogging_BasicFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Chain: RequestID -> Logging -> handler
	handler := RequestID(Logging(logger)(inner))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)

	assert.Equal(t, "request completed", m["msg"])
	assert.Equal(t, "GET", m["method"])
	assert.Equal(t, "/v1/models", m["path"])
	assert.Equal(t, float64(200), m["status"])
	assert.NotEmpty(t, m["request_id"])
	assert.Contains(t, m, "duration_ms")
}

func TestLogging_IncludesModelAndBackend(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the proxy handler setting MetricsData.
		md := &MetricsData{
			Model:      "gpt-4",
			Streaming:  true,
			BackendURL: "http://vllm-1:8000",
		}
		ctx := SetMetricsData(r.Context(), md)
		*r = *r.WithContext(ctx)
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(Logging(logger)(inner))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)

	assert.Equal(t, "gpt-4", m["model"])
	assert.Equal(t, "http://vllm-1:8000", m["backend_url"])
	assert.Equal(t, true, m["streaming"])
}

func TestLogging_IncludesIdentityID(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(Logging(logger)(inner))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req = req.WithContext(WithIdentityID(req.Context(), "identity-1"))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "identity-1", m["identity_id"])
}

func TestLogging_WarnOn4xx(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("GET", "/not-found", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "WARN", m["level"])
	assert.Equal(t, float64(404), m["status"])
}

func TestLogging_ErrorOn5xx(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "ERROR", m["level"])
	assert.Equal(t, float64(502), m["status"])
}

func TestLogging_InfoOn2xx(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "INFO", m["level"])
}

func TestLogging_NoMetricsData(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)

	// Without MetricsData, model/backend/streaming should not be present.
	assert.NotContains(t, m, "model")
	assert.NotContains(t, m, "backend_url")
	assert.NotContains(t, m, "streaming")
}

func TestLogging_NoIdentity(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.NotContains(t, m, "identity_id")
}

func TestLogging_RequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	// Manually set request ID in context (simulating RequestID middleware).
	ctx := context.WithValue(req.Context(), requestIDKey{}, "test-request-id-42")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "test-request-id-42", m["request_id"])
}

// On the workload (mTLS) path, the completion log must carry run/turn/pool so
// outcomes and denials remain attributable to the run (SCN-009). WorkloadAuth
// propagates its context by mutating r in place; this test mirrors that.
func TestLogging_IncludesWorkloadScope(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wc := WorkloadContext{PoolID: "p1", PoolKey: "default/pool-1", RunID: "run-9", TurnID: "turn-9", AssignedModel: "qwen3-27b"}
		ctx := WithWorkloadContext(r.Context(), wc)
		ctx = WithIdentityID(ctx, "id-7")
		*r = *r.WithContext(ctx)
		w.WriteHeader(http.StatusOK)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "run-9", m["run_id"])
	assert.Equal(t, "turn-9", m["turn_id"])
	assert.Equal(t, "default/pool-1", m["pool_key"])
	assert.Equal(t, "id-7", m["identity_id"])
	assert.Equal(t, "completed", m["disposition"])
}

// A canceled workload stream writes HTTP 200 before the client cancels; the
// completion log must still report a "canceled" disposition rather than
// looking successful (HOR-398: cancellation reports an attributable outcome).
func TestLogging_CanceledStreamDisposition(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Logging(logger)(inner)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	cancel()
	handler.ServeHTTP(rec, req)

	m := parseLogLine(t, &buf)
	assert.Equal(t, "canceled", m["disposition"])
}

func TestLogging_DispositionByStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusOK, "completed"},
		{http.StatusForbidden, "denied"},
		{http.StatusBadGateway, "error"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var buf bytes.Buffer
			logger := newBufferLogger(&buf)
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })
			Logging(logger)(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
			m := parseLogLine(t, &buf)
			assert.Equal(t, tc.want, m["disposition"])
		})
	}
}
