package proxy

import (
	"encoding/json"

	"github.com/nunocgoncalves/inference-gateway/internal/snapshot"
)

// ApplyRequestTransforms applies the alias's per-alias config to the request
// body before forwarding to the backend. Transforms are applied in order:
//  1. Default sampling params (inject if not present in request)
//  2. Reasoning config (enable / disable / passthrough)
//  3. System prompt prefix (prepend to messages)
//
// The model-field rewrite (alias -> backend_model_id) is done in the handler.
func ApplyRequestTransforms(body []byte, entry *snapshot.CatalogEntry) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}

	applyDefaultParams(parsed, entry.DefaultParams)
	applyReasoningConfig(parsed, entry.ReasoningConfig)
	applySystemPromptPrefix(parsed, entry.Transforms)

	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return rewritten
}

func applyDefaultParams(parsed map[string]any, defaults snapshot.DefaultParams) {
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

func applyReasoningConfig(parsed map[string]any, cfg snapshot.ReasoningConfig) {
	if cfg.EnableThinking == nil {
		return
	}
	kwargs, ok := parsed["chat_template_kwargs"].(map[string]any)
	if !ok {
		kwargs = make(map[string]any)
	}
	kwargs["enable_thinking"] = *cfg.EnableThinking
	parsed["chat_template_kwargs"] = kwargs
}

func applySystemPromptPrefix(parsed map[string]any, transforms snapshot.Transforms) {
	prefix := transforms.SystemPromptPrefix
	if prefix == "" {
		return
	}
	messages, ok := parsed["messages"].([]any)
	if !ok || len(messages) == 0 {
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
		existingContent, _ := firstMsg["content"].(string)
		firstMsg["content"] = prefix + "\n" + existingContent
		messages[0] = firstMsg
	} else {
		systemMsg := map[string]any{"role": "system", "content": prefix}
		messages = append([]any{systemMsg}, messages...)
	}
	parsed["messages"] = messages
}
