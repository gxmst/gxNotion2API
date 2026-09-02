package app

import (
	"strings"
	"testing"
)

func TestChatToolMessageIsIncludedInPrompt(t *testing.T) {
	normalized, err := normalizeChatInput(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "check weather"},
			map[string]any{"role": "tool", "name": "weather", "content": "sunny, 27C"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalized.Prompt, "[tool result weather]") || !strings.Contains(normalized.Prompt, "sunny, 27C") {
		t.Fatalf("tool result was dropped: %q", normalized.Prompt)
	}
}

func TestResponsesFunctionCallOutputIsIncludedInPrompt(t *testing.T) {
	normalized, err := normalizeResponsesInput(map[string]any{
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "weather-call", "output": "rain tomorrow"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalized.Prompt, "[tool result weather-call]") || !strings.Contains(normalized.Prompt, "rain tomorrow") {
		t.Fatalf("function output was dropped: %q", normalized.Prompt)
	}
}
