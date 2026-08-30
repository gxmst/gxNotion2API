package app

import (
	"strings"
	"testing"
)

func TestSystemPrefixEnabledByDefault(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	if !promptSystemPrefixEnabled(cfg) {
		t.Fatal("the standing system prefix must be enabled unless explicitly turned off")
	}
	if strings.TrimSpace(cfg.Prompt.SystemPrefix) == "" {
		t.Fatal("normalizeConfig must fill in a default system prefix")
	}
}

func TestApplyPromptSystemPrefixPrependsAheadOfClientInstructions(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())

	card := "[Seraphina's Personality= \"caring\", \"protective\"]\nScenario: a magical forest."
	got := applyPromptSystemPrefix(cfg, card)

	if !strings.HasPrefix(got, cfg.Prompt.SystemPrefix) {
		t.Fatal("system prefix must come first so it frames what follows")
	}
	if !strings.Contains(got, card) {
		t.Fatal("the client's own instructions must be preserved verbatim")
	}
}

// A retry or a continuation turn rebuilds the payload from an already-prefixed
// hidden prompt; stacking the prefix repeatedly would waste context.
func TestApplyPromptSystemPrefixIsIdempotent(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())

	once := applyPromptSystemPrefix(cfg, "be a helpful narrator")
	twice := applyPromptSystemPrefix(cfg, once)
	if once != twice {
		t.Fatalf("prefix stacked on second application:\nonce=%q\ntwice=%q", once, twice)
	}
	if n := strings.Count(twice, cfg.Prompt.SystemPrefix); n != 1 {
		t.Fatalf("prefix appears %d times, want 1", n)
	}
}

func TestApplyPromptSystemPrefixWithEmptyInstructions(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	got := applyPromptSystemPrefix(cfg, "")
	if got != cfg.Prompt.SystemPrefix {
		t.Fatalf("empty instructions should yield the prefix alone, got %q", got)
	}
}

func TestApplyPromptSystemPrefixCanBeDisabled(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	disabled := false
	cfg.Prompt.SystemPrefixEnabled = &disabled

	card := "my own instructions"
	if got := applyPromptSystemPrefix(cfg, card); got != card {
		t.Fatalf("disabled prefix must leave instructions untouched, got %q", got)
	}
}

func TestApplyPromptSystemPrefixHonoursCustomText(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	cfg.Prompt.SystemPrefix = "You are a plain assistant."

	got := applyPromptSystemPrefix(cfg, "card text")
	if !strings.HasPrefix(got, "You are a plain assistant.") {
		t.Fatalf("custom prefix not applied, got %q", got)
	}
	if strings.Contains(got, "general-purpose AI assistant") {
		t.Fatal("custom prefix must replace the built-in default, not append to it")
	}
}

// The prefix must reach the upstream payload as the instructions the model reads.
func TestInferencePayloadCarriesSystemPrefix(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:  cfg,
		Session: SessionInfo{SpaceID: "space-1", UserID: "user-1", ClientVersion: "1.0"},
	}

	card := "Roleplay as Seraphina, a forest guardian."
	payload, _ := client.buildInferencePayload(
		PromptRunRequest{Prompt: "hello", HiddenPrompt: card}, "thread-1", nil)

	transcript, ok := payload["transcript"].([]map[string]any)
	if !ok {
		t.Fatalf("transcript missing or wrong type: %T", payload["transcript"])
	}
	found := ""
	for _, step := range transcript {
		value, _ := step["value"].(map[string]any)
		if value == nil {
			continue
		}
		if instructions, ok := value["instructions"].(string); ok && instructions != "" {
			found = instructions
		}
	}
	if found == "" {
		t.Fatal("no instructions reached the upstream payload")
	}
	if !strings.HasPrefix(found, cfg.Prompt.SystemPrefix) {
		t.Fatalf("instructions do not start with the system prefix: %q", found[:minInt(120, len(found))])
	}
	if !strings.Contains(found, card) {
		t.Fatal("the client's card must still be present in the instructions")
	}
}

// payloadInstructions returns the last non-empty instructions value in a payload.
func payloadInstructions(t *testing.T, payload map[string]any) string {
	t.Helper()
	transcript, ok := payload["transcript"].([]map[string]any)
	if !ok {
		t.Fatalf("transcript missing or wrong type: %T", payload["transcript"])
	}
	found := ""
	for _, step := range transcript {
		value, _ := step["value"].(map[string]any)
		if value == nil {
			continue
		}
		if instructions, ok := value["instructions"].(string); ok && instructions != "" {
			found = instructions
		}
	}
	return found
}

// A later SillyTavern turn sends no system message, so HiddenPrompt is empty and
// the character card lives only in the continuation draft. The standing prefix
// must frame that carried card, never replace it — replacing it stripped the
// persona and the model answered as a plain product assistant.
func TestContinuationKeepsCarriedInstructionsWhenClientSendsNone(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:  cfg,
		Session: SessionInfo{SpaceID: "space-1", UserID: "user-1", ClientVersion: "1.0"},
	}

	card := "[Seraphina's Personality= \"caring\", \"protective\"]\nScenario: the forest of Eldoria."
	draft := &continuationTurnDraft{
		SessionID:    "session-1",
		ConfigID:     "11111111-1111-1111-1111-111111111111",
		ContextID:    "22222222-2222-2222-2222-222222222222",
		ContextValue: map[string]any{"instructions": card, "runtimePromptHint": card},
	}

	payload, _ := client.buildInferencePayload(
		PromptRunRequest{Prompt: "你是谁 我是谁", HiddenPrompt: "", continuationDraft: draft},
		"thread-1", nil)

	found := payloadInstructions(t, payload)
	if found == "" {
		t.Fatal("no instructions reached the upstream payload")
	}
	if !strings.Contains(found, card) {
		t.Fatalf("the carried card was dropped; instructions = %q", found[:minInt(200, len(found))])
	}
	if !strings.HasPrefix(found, cfg.Prompt.SystemPrefix) {
		t.Fatal("the standing prefix should still frame the carried card")
	}
}

// When the client does re-send its own instructions on a continuation turn,
// those win over whatever the draft carried.
func TestContinuationPrefersFreshClientInstructions(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:  cfg,
		Session: SessionInfo{SpaceID: "space-1", UserID: "user-1", ClientVersion: "1.0"},
	}

	stale := "old card that must not win"
	fresh := "new card the client just sent"
	draft := &continuationTurnDraft{
		SessionID:    "session-1",
		ConfigID:     "11111111-1111-1111-1111-111111111111",
		ContextID:    "22222222-2222-2222-2222-222222222222",
		ContextValue: map[string]any{"instructions": stale, "runtimePromptHint": stale},
	}

	payload, _ := client.buildInferencePayload(
		PromptRunRequest{Prompt: "hi", HiddenPrompt: fresh, continuationDraft: draft},
		"thread-1", nil)

	found := payloadInstructions(t, payload)
	if !strings.Contains(found, fresh) {
		t.Fatalf("fresh client instructions missing: %q", found[:minInt(200, len(found))])
	}
	if strings.Contains(found, stale) {
		t.Fatal("stale carried instructions must not survive a fresh client send")
	}
}

// A continuation turn with neither client instructions nor carried ones should
// still get the standing prefix, so a plain chat is not framed as product-only.
func TestContinuationWithoutAnyInstructionsStillGetsPrefix(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:  cfg,
		Session: SessionInfo{SpaceID: "space-1", UserID: "user-1", ClientVersion: "1.0"},
	}

	draft := &continuationTurnDraft{
		SessionID:    "session-1",
		ConfigID:     "11111111-1111-1111-1111-111111111111",
		ContextID:    "22222222-2222-2222-2222-222222222222",
		ContextValue: map[string]any{},
	}

	payload, _ := client.buildInferencePayload(
		PromptRunRequest{Prompt: "hello", HiddenPrompt: "", continuationDraft: draft},
		"thread-1", nil)

	found := payloadInstructions(t, payload)
	if found != cfg.Prompt.SystemPrefix {
		t.Fatalf("expected the prefix alone, got %q", found[:minInt(200, len(found))])
	}
}
