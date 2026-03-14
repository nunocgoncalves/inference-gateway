package proxy

import (
	"encoding/json"
	"testing"

	"github.com/nunocgoncalves/inference-gateway/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrFloat64(v float64) *float64 { return &v }
func ptrInt(v int) *int             { return &v }
func ptrBool(v bool) *bool          { return &v }

// ---------------------------------------------------------------------------
// Default params tests
// ---------------------------------------------------------------------------

func TestApplyDefaultParams_InjectsWhenAbsent(t *testing.T) {
	body := []byte(`{"model": "test", "messages": [{"role": "user", "content": "hi"}]}`)
	model := &registry.Model{
		DefaultParams: registry.DefaultParams{
			Temperature: ptrFloat64(0.7),
			TopP:        ptrFloat64(0.9),
			MaxTokens:   ptrInt(4096),
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, 0.7, parsed["temperature"])
	assert.Equal(t, 0.9, parsed["top_p"])
	assert.Equal(t, float64(4096), parsed["max_tokens"])
}

func TestApplyDefaultParams_ClientOverrides(t *testing.T) {
	// Client sets temperature=0.2 — model default of 0.7 should NOT override.
	body := []byte(`{"model": "test", "messages": [], "temperature": 0.2}`)
	model := &registry.Model{
		DefaultParams: registry.DefaultParams{
			Temperature: ptrFloat64(0.7),
			TopP:        ptrFloat64(0.9),
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, 0.2, parsed["temperature"], "client value should take precedence")
	assert.Equal(t, 0.9, parsed["top_p"], "absent value should be injected")
}

func TestApplyDefaultParams_AllParams(t *testing.T) {
	body := []byte(`{"model": "test", "messages": []}`)
	model := &registry.Model{
		DefaultParams: registry.DefaultParams{
			Temperature:      ptrFloat64(0.5),
			TopP:             ptrFloat64(0.95),
			MaxTokens:        ptrInt(2048),
			FrequencyPenalty: ptrFloat64(0.1),
			PresencePenalty:  ptrFloat64(0.2),
			TopK:             ptrInt(50),
			MinP:             ptrFloat64(0.05),
			Stop:             []string{"<stop>", "</s>"},
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, 0.5, parsed["temperature"])
	assert.Equal(t, 0.95, parsed["top_p"])
	assert.Equal(t, float64(2048), parsed["max_tokens"])
	assert.Equal(t, 0.1, parsed["frequency_penalty"])
	assert.Equal(t, 0.2, parsed["presence_penalty"])
	assert.Equal(t, float64(50), parsed["top_k"])
	assert.Equal(t, 0.05, parsed["min_p"])
	stops := parsed["stop"].([]any)
	assert.Len(t, stops, 2)
}

func TestApplyDefaultParams_NoDefaults(t *testing.T) {
	body := []byte(`{"model": "test", "messages": [], "temperature": 0.3}`)
	model := &registry.Model{}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, 0.3, parsed["temperature"])
	// No other params injected.
	_, hasTopP := parsed["top_p"]
	assert.False(t, hasTopP)
}

// ---------------------------------------------------------------------------
// Reasoning config tests
// ---------------------------------------------------------------------------

func TestApplyReasoningConfig_Passthrough(t *testing.T) {
	// Enabled == nil → passthrough, no changes.
	body := []byte(`{"model": "test", "messages": [], "reasoning_effort": "high"}`)
	model := &registry.Model{
		ReasoningConfig: registry.ReasoningConfig{
			Enabled: nil, // passthrough
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, "high", parsed["reasoning_effort"], "client value preserved in passthrough mode")
}

func TestApplyReasoningConfig_Enable(t *testing.T) {
	body := []byte(`{"model": "test", "messages": []}`)
	model := &registry.Model{
		ReasoningConfig: registry.ReasoningConfig{
			Enabled:          ptrBool(true),
			ReasoningEffort:  "medium",
			IncludeReasoning: ptrBool(true),
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, "medium", parsed["reasoning_effort"])
	assert.Equal(t, true, parsed["include_reasoning"])
}

func TestApplyReasoningConfig_EnableOverridesClient(t *testing.T) {
	// When enabled=true, model config overrides client values.
	body := []byte(`{"model": "test", "messages": [], "reasoning_effort": "high"}`)
	model := &registry.Model{
		ReasoningConfig: registry.ReasoningConfig{
			Enabled:          ptrBool(true),
			ReasoningEffort:  "low",
			IncludeReasoning: ptrBool(true),
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, "low", parsed["reasoning_effort"], "model config overrides client")
}

func TestApplyReasoningConfig_Disable(t *testing.T) {
	// Enabled == false → force off, regardless of client.
	body := []byte(`{"model": "test", "messages": [], "reasoning_effort": "high", "include_reasoning": true}`)
	model := &registry.Model{
		ReasoningConfig: registry.ReasoningConfig{
			Enabled: ptrBool(false),
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	assert.Equal(t, "none", parsed["reasoning_effort"], "should be forced to none")
	assert.Equal(t, false, parsed["include_reasoning"], "should be forced to false")
}

// ---------------------------------------------------------------------------
// System prompt prefix tests
// ---------------------------------------------------------------------------

func TestApplySystemPromptPrefix_NoExistingSystem(t *testing.T) {
	body := []byte(`{"model": "test", "messages": [{"role": "user", "content": "hello"}]}`)
	model := &registry.Model{
		Transforms: registry.Transforms{
			SystemPromptPrefix: "You are a helpful assistant.",
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	messages := parsed["messages"].([]any)
	assert.Len(t, messages, 2)

	systemMsg := messages[0].(map[string]any)
	assert.Equal(t, "system", systemMsg["role"])
	assert.Equal(t, "You are a helpful assistant.", systemMsg["content"])

	userMsg := messages[1].(map[string]any)
	assert.Equal(t, "user", userMsg["role"])
	assert.Equal(t, "hello", userMsg["content"])
}

func TestApplySystemPromptPrefix_ExistingSystem(t *testing.T) {
	body := []byte(`{"model": "test", "messages": [{"role": "system", "content": "Original system prompt"}, {"role": "user", "content": "hello"}]}`)
	model := &registry.Model{
		Transforms: registry.Transforms{
			SystemPromptPrefix: "Prefix.",
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	messages := parsed["messages"].([]any)
	assert.Len(t, messages, 2, "should not add extra message, just prepend to existing")

	systemMsg := messages[0].(map[string]any)
	assert.Equal(t, "system", systemMsg["role"])
	assert.Equal(t, "Prefix.\nOriginal system prompt", systemMsg["content"])
}

func TestApplySystemPromptPrefix_EmptyMessages(t *testing.T) {
	body := []byte(`{"model": "test", "messages": []}`)
	model := &registry.Model{
		Transforms: registry.Transforms{
			SystemPromptPrefix: "You are a bot.",
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	messages := parsed["messages"].([]any)
	assert.Len(t, messages, 1)
	assert.Equal(t, "system", messages[0].(map[string]any)["role"])
}

func TestApplySystemPromptPrefix_NoPrefix(t *testing.T) {
	body := []byte(`{"model": "test", "messages": [{"role": "user", "content": "hello"}]}`)
	model := &registry.Model{
		Transforms: registry.Transforms{
			SystemPromptPrefix: "", // No prefix configured.
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	messages := parsed["messages"].([]any)
	assert.Len(t, messages, 1, "no system message should be added")
}

// ---------------------------------------------------------------------------
// Combined transforms test
// ---------------------------------------------------------------------------

func TestApplyAllTransforms(t *testing.T) {
	body := []byte(`{"model": "alias", "messages": [{"role": "user", "content": "solve this"}]}`)
	model := &registry.Model{
		Name:    "alias",
		ModelID: "meta-llama/Llama-3.1-70B-Instruct",
		DefaultParams: registry.DefaultParams{
			Temperature: ptrFloat64(0.6),
			MaxTokens:   ptrInt(8192),
		},
		ReasoningConfig: registry.ReasoningConfig{
			Enabled:          ptrBool(true),
			ReasoningEffort:  "high",
			IncludeReasoning: ptrBool(true),
		},
		Transforms: registry.Transforms{
			SystemPromptPrefix: "Think step by step.",
		},
	}

	result := ApplyRequestTransforms(body, model)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))

	// Default params injected.
	assert.Equal(t, 0.6, parsed["temperature"])
	assert.Equal(t, float64(8192), parsed["max_tokens"])

	// Reasoning enabled.
	assert.Equal(t, "high", parsed["reasoning_effort"])
	assert.Equal(t, true, parsed["include_reasoning"])

	// System prompt prefix added.
	messages := parsed["messages"].([]any)
	assert.Len(t, messages, 2)
	assert.Equal(t, "system", messages[0].(map[string]any)["role"])
	assert.Equal(t, "Think step by step.", messages[0].(map[string]any)["content"])
}
