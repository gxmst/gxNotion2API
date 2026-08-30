package app

import (
	"strings"
	"testing"
)

// A system message carries operating instructions. Flattening it into the
// visible prompt makes the model read its own directives as if the user had
// typed them, which upstream then classifies as a user request rather than as
// instructions it should follow.
func TestSystemMessageBecomesInstructionsNotPromptText(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Seraphina, guardian of the glade."},
			map[string]any{"role": "user", "content": "你好"},
		},
	}

	normalized, err := normalizeChatInput(payload)
	if err != nil {
		t.Fatalf("normalizeChatInput: %v", err)
	}

	if !strings.Contains(normalized.HiddenPrompt, "Seraphina") {
		t.Fatalf("system content missing from HiddenPrompt: %q", normalized.HiddenPrompt)
	}
	if strings.Contains(normalized.Prompt, "Seraphina") {
		t.Fatalf("system content leaked into the visible prompt: %q", normalized.Prompt)
	}
	if strings.Contains(normalized.Prompt, "[system]") {
		t.Fatalf("visible prompt still carries a role label: %q", normalized.Prompt)
	}
	if !strings.Contains(normalized.Prompt, "你好") {
		t.Fatalf("user text missing from the prompt: %q", normalized.Prompt)
	}
}

// The OpenAI "developer" role has the same meaning as "system".
func TestDeveloperRoleAlsoBecomesInstructions(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "developer", "content": "Answer only in French."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	normalized, err := normalizeChatInput(payload)
	if err != nil {
		t.Fatalf("normalizeChatInput: %v", err)
	}
	if !strings.Contains(normalized.HiddenPrompt, "only in French") {
		t.Fatalf("developer content missing from HiddenPrompt: %q", normalized.HiddenPrompt)
	}
	if strings.Contains(normalized.Prompt, "only in French") {
		t.Fatalf("developer content leaked into the prompt: %q", normalized.Prompt)
	}
}

// Several system messages (a card, then examples, then a per-turn directive) are
// joined in order, so the persona and its examples all arrive as instructions.
func TestMultipleSystemMessagesAreJoinedInOrder(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "CARD: Seraphina is a forest guardian."},
			map[string]any{"role": "user", "content": "What are your powers?"},
			map[string]any{"role": "assistant", "content": "Healing and nature magic."},
			map[string]any{"role": "system", "content": "DIRECTIVE: write the next reply in character."},
			map[string]any{"role": "user", "content": "你好"},
		},
	}

	normalized, err := normalizeChatInput(payload)
	if err != nil {
		t.Fatalf("normalizeChatInput: %v", err)
	}

	cardAt := strings.Index(normalized.HiddenPrompt, "CARD:")
	directiveAt := strings.Index(normalized.HiddenPrompt, "DIRECTIVE:")
	if cardAt < 0 || directiveAt < 0 {
		t.Fatalf("both system messages must reach HiddenPrompt: %q", normalized.HiddenPrompt)
	}
	if cardAt > directiveAt {
		t.Fatal("system messages must keep their original order")
	}

	// The real conversation turns must stay in the transcript.
	if !strings.Contains(normalized.Prompt, "What are your powers?") {
		t.Fatalf("user history missing from the prompt: %q", normalized.Prompt)
	}
	if !strings.Contains(normalized.Prompt, "Healing and nature magic.") {
		t.Fatalf("assistant history missing from the prompt: %q", normalized.Prompt)
	}
	if strings.Contains(normalized.Prompt, "CARD:") || strings.Contains(normalized.Prompt, "DIRECTIVE:") {
		t.Fatalf("instructions leaked into the visible prompt: %q", normalized.Prompt)
	}
}

// A request with no system message must be unaffected.
func TestUserOnlyRequestKeepsEmptyInstructions(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "What is 2 plus 2?"},
		},
	}

	normalized, err := normalizeChatInput(payload)
	if err != nil {
		t.Fatalf("normalizeChatInput: %v", err)
	}
	if strings.TrimSpace(normalized.HiddenPrompt) != "" {
		t.Fatalf("expected no instructions, got %q", normalized.HiddenPrompt)
	}
	if !strings.Contains(normalized.Prompt, "2 plus 2") {
		t.Fatalf("prompt lost the user text: %q", normalized.Prompt)
	}
}

// End to end: the persona must arrive in the upstream instructions field,
// framed by the standing prefix, and must not appear in the visible prompt.
func TestSystemMessageReachesUpstreamInstructions(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:  cfg,
		Session: SessionInfo{SpaceID: "space-1", UserID: "user-1", ClientVersion: "1.0"},
	}

	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "Write Seraphina's next reply in a fictional chat."},
			map[string]any{"role": "user", "content": "你好 我是谁 你是谁"},
		},
	}
	normalized, err := normalizeChatInput(payload)
	if err != nil {
		t.Fatalf("normalizeChatInput: %v", err)
	}

	built, _ := client.buildInferencePayload(PromptRunRequest{
		Prompt:       normalized.Prompt,
		HiddenPrompt: normalized.HiddenPrompt,
	}, "thread-1", nil)

	instructions := payloadInstructions(t, built)
	if !strings.Contains(instructions, "Seraphina") {
		t.Fatalf("persona missing from upstream instructions: %q", instructions)
	}
	if !strings.HasPrefix(instructions, cfg.Prompt.SystemPrefix) {
		t.Fatal("the standing prefix should frame the persona")
	}
}
