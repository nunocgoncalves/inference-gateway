package proxy

import (
	"encoding/json"
	"testing"

	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyRequestTransforms_DefaultParams(t *testing.T) {
	temp := 0.7
	maxTok := 1024
	entry := &snapshot.CatalogEntry{DefaultParams: snapshot.DefaultParams{Temperature: &temp, MaxTokens: &maxTok}}
	out := ApplyRequestTransforms([]byte(`{"model":"qwen3-27b","messages":[]}`), entry)
	var p map[string]any
	require.NoError(t, json.Unmarshal(out, &p))
	assert.InDelta(t, 0.7, p["temperature"], 1e-9)
	assert.EqualValues(t, 1024, p["max_tokens"])
}

func TestApplyRequestTransforms_DefaultsDoNotOverrideClient(t *testing.T) {
	temp := 0.7
	entry := &snapshot.CatalogEntry{DefaultParams: snapshot.DefaultParams{Temperature: &temp}}
	out := ApplyRequestTransforms([]byte(`{"model":"x","temperature":0.1}`), entry)
	var p map[string]any
	require.NoError(t, json.Unmarshal(out, &p))
	assert.InDelta(t, 0.1, p["temperature"], 1e-9, "client-provided value must win")
}

func TestApplyRequestTransforms_ReasoningConfig(t *testing.T) {
	off := false
	entry := &snapshot.CatalogEntry{ReasoningConfig: snapshot.ReasoningConfig{EnableThinking: &off}}
	out := ApplyRequestTransforms([]byte(`{"model":"x"}`), entry)
	var p map[string]any
	require.NoError(t, json.Unmarshal(out, &p))
	kwargs, ok := p["chat_template_kwargs"].(map[string]any)
	require.True(t, ok)
	assert.False(t, kwargs["enable_thinking"].(bool))
}

func TestApplyRequestTransforms_ReasoningPassthrough(t *testing.T) {
	entry := &snapshot.CatalogEntry{}
	out := ApplyRequestTransforms([]byte(`{"model":"x"}`), entry)
	var p map[string]any
	require.NoError(t, json.Unmarshal(out, &p))
	_, ok := p["chat_template_kwargs"]
	assert.False(t, ok, "nil EnableThinking must not inject chat_template_kwargs")
}

func TestApplyRequestTransforms_SystemPromptPrefix(t *testing.T) {
	entry := &snapshot.CatalogEntry{Transforms: snapshot.Transforms{SystemPromptPrefix: "Be brief."}}
	out := ApplyRequestTransforms([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), entry)
	var p map[string]any
	require.NoError(t, json.Unmarshal(out, &p))
	msgs := p["messages"].([]any)
	assert.Equal(t, "system", msgs[0].(map[string]any)["role"])
	assert.Equal(t, "Be brief.", msgs[0].(map[string]any)["content"])
}
