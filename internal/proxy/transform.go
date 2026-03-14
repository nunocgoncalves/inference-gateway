package proxy

import (
	"encoding/json"

	"github.com/nunocgoncalves/inference-gateway/internal/registry"
)

// ApplyRequestTransforms applies all model-specific transforms to the request
// body before forwarding to the vLLM backend. Transforms are applied in order:
//  1. Default sampling params (inject if not present in request)
//  2. Reasoning config (enable / disable / passthrough)
//  3. System prompt prefix (prepend to messages)
//  4. Model name rewrite (alias → actual vLLM model ID) — already done in handler
func ApplyRequestTransforms(body []byte, model *registry.Model) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}

	applyDefaultParams(parsed, model.DefaultParams)
	applyReasoningConfig(parsed, model.ReasoningConfig)
	applySystemPromptPrefix(parsed, model.Transforms)

	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

// applyDefaultParams injects default sampling parameters from the model config
// when the client hasn't specified them. Client-provided values always take
// precedence.
func applyDefaultParams(parsed map[string]any, defaults registry.DefaultParams) {
	setIfAbsent := func(key string, val any) {
		if val == nil {
			return
		}
		if _, exists := parsed[key]; !exists {
			parsed[key] = val
		}
	}

	if defaults.Temperature != nil {
		setIfAbsent("temperature", *defaults.Temperature)
	}
	if defaults.TopP != nil {
		setIfAbsent("top_p", *defaults.TopP)
	}
	if defaults.MaxTokens != nil {
		setIfAbsent("max_tokens", *defaults.MaxTokens)
	}
	if defaults.FrequencyPenalty != nil {
		setIfAbsent("frequency_penalty", *defaults.FrequencyPenalty)
	}
	if defaults.PresencePenalty != nil {
		setIfAbsent("presence_penalty", *defaults.PresencePenalty)
	}
	if defaults.RepetitionPenalty != nil {
		setIfAbsent("repetition_penalty", *defaults.RepetitionPenalty)
	}
	if defaults.TopK != nil {
		setIfAbsent("top_k", *defaults.TopK)
	}
	if defaults.MinP != nil {
		setIfAbsent("min_p", *defaults.MinP)
	}
	if len(defaults.Stop) > 0 {
		setIfAbsent("stop", defaults.Stop)
	}
}

// applyReasoningConfig applies reasoning configuration to the request by
// injecting chat_template_kwargs.enable_thinking for vLLM:
//   - EnableThinking == nil  → passthrough (no changes)
//   - EnableThinking == true → set chat_template_kwargs.enable_thinking = true
//   - EnableThinking == false → set chat_template_kwargs.enable_thinking = false
func applyReasoningConfig(parsed map[string]any, cfg registry.ReasoningConfig) {
	if cfg.EnableThinking == nil {
		// Passthrough — don't touch the request.
		return
	}

	kwargs, ok := parsed["chat_template_kwargs"].(map[string]any)
	if !ok {
		kwargs = make(map[string]any)
	}
	kwargs["enable_thinking"] = *cfg.EnableThinking
	parsed["chat_template_kwargs"] = kwargs
}

// applySystemPromptPrefix prepends a system message to the messages array
// if configured. If the first message is already a system message, the prefix
// is prepended to its content. Otherwise a new system message is inserted.
func applySystemPromptPrefix(parsed map[string]any, transforms registry.Transforms) {
	prefix := transforms.SystemPromptPrefix
	if prefix == "" {
		return
	}

	messages, ok := parsed["messages"].([]any)
	if !ok || len(messages) == 0 {
		// No messages — insert a system message.
		parsed["messages"] = []any{
			map[string]any{"role": "system", "content": prefix},
		}
		return
	}

	firstMsg, ok := messages[0].(map[string]any)
	if !ok {
		return
	}

	role, _ := firstMsg["role"].(string)
	if role == "system" {
		// Prepend to existing system message content.
		existingContent, _ := firstMsg["content"].(string)
		firstMsg["content"] = prefix + "\n" + existingContent
		messages[0] = firstMsg
	} else {
		// Insert a new system message at the beginning.
		systemMsg := map[string]any{"role": "system", "content": prefix}
		messages = append([]any{systemMsg}, messages...)
	}
	parsed["messages"] = messages
}
