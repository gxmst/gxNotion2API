package app

import (
	"testing"
	"time"
)

func newEphemeralConfigTestApp(t *testing.T, configure func(*AppConfig)) *App {
	t.Helper()
	cfg := defaultConfig()
	cfg.APIKey = "test-api-key"
	cfg.Storage.SQLitePath = ""
	if configure != nil {
		configure(&cfg)
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatalf("newServerState failed: %v", err)
	}
	t.Cleanup(func() {
		_ = state.Close()
	})
	return &App{State: state}
}

func TestConfigEphemeralMarksEveryRequest(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = true
		cfg.Features.EphemeralTTLSeconds = 120
	})

	request := PromptRunRequest{}
	app.markEphemeralConversationRequest(&request)

	if !request.EphemeralConversation {
		t.Fatal("expected ephemeral_all_conversations to mark a plain API request")
	}
	if request.EphemeralReason != "config_ephemeral_all" {
		t.Fatalf("reason = %q, want config_ephemeral_all", request.EphemeralReason)
	}
	if request.EphemeralDeleteAfter.IsZero() {
		t.Fatal("expected a delete deadline to be set")
	}
	remaining := time.Until(request.EphemeralDeleteAfter)
	if remaining <= 90*time.Second || remaining > 120*time.Second {
		t.Fatalf("deadline in %v, want ~120s from now", remaining)
	}
}

func TestConfigEphemeralDisabledLeavesRequestAlone(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = false
	})

	request := PromptRunRequest{}
	app.markEphemeralConversationRequest(&request)

	if request.EphemeralConversation {
		t.Fatal("plain API request must not be ephemeral when the flag is off")
	}
}

func TestConfigEphemeralDoesNotBreakSillyTavernModes(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = false
	})

	request := PromptRunRequest{
		ClientProfile: sillyTavernClientProfile,
		ClientMode:    sillyTavernModeQuiet,
	}
	app.markEphemeralConversationRequest(&request)

	if !request.EphemeralConversation {
		t.Fatal("sillytavern quiet requests must stay ephemeral")
	}
	if request.EphemeralReason != "sillytavern_quiet" {
		t.Fatalf("reason = %q, want sillytavern_quiet", request.EphemeralReason)
	}
}

func TestConfiguredEphemeralTTLFallsBackToDefault(t *testing.T) {
	cfg := defaultConfig()
	cfg.Features.EphemeralTTLSeconds = 0
	if got := configuredEphemeralTTL(cfg); got != defaultConfigEphemeralConversationTTL {
		t.Fatalf("ttl = %v, want %v", got, defaultConfigEphemeralConversationTTL)
	}

	cfg.Features.EphemeralTTLSeconds = 45
	if got := configuredEphemeralTTL(cfg); got != 45*time.Second {
		t.Fatalf("ttl = %v, want 45s", got)
	}
}

// Completing a turn must not clamp the deadline back to the built-in
// SillyTavern TTL when a longer lifetime is configured.
func TestCompleteHonoursConfiguredEphemeralTTL(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = true
		cfg.Features.EphemeralTTLSeconds = 3600
	})

	store := app.State.conversations()
	entry := store.Create(ConversationCreateRequest{
		Ephemeral:       true,
		EphemeralReason: "config_ephemeral_all",
		AutoDeleteAt:    time.Now().UTC().Add(time.Hour),
		Prompt:          "hello",
	})
	store.Complete(entry.ID, InferenceResult{Text: "hi", ThreadID: "thread-1"})

	updated, ok := store.Get(entry.ID)
	if !ok {
		t.Fatal("conversation vanished after Complete")
	}
	if updated.AutoDeleteAt == nil {
		t.Fatal("expected an auto-delete deadline")
	}
	remaining := time.Until(*updated.AutoDeleteAt)
	if remaining <= 30*time.Minute {
		t.Fatalf("deadline in %v, want ~1h (hardcoded 10m TTL leaked through)", remaining)
	}
}

func TestExpiredEphemeralConversationsAreListed(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = true
		cfg.Features.EphemeralTTLSeconds = 60
	})

	store := app.State.conversations()
	entry := store.Create(ConversationCreateRequest{
		Ephemeral:       true,
		EphemeralReason: "config_ephemeral_all",
		Prompt:          "stale",
	})

	// A turn still in flight must never be swept, however old its deadline.
	if items := store.ListExpiredEphemeral(time.Now().UTC().Add(24*time.Hour), 10); len(items) != 0 {
		t.Fatalf("a running conversation must not be swept, got %d", len(items))
	}

	store.Complete(entry.ID, InferenceResult{Text: "done", ThreadID: "thread-stale"})

	// Once finished it becomes eligible, but only after its deadline passes.
	if items := store.ListExpiredEphemeral(time.Now().UTC(), 10); len(items) != 0 {
		t.Fatalf("conversation swept before its deadline, got %d", len(items))
	}

	future := time.Now().UTC().Add(2 * time.Minute)
	found := false
	for _, item := range store.ListExpiredEphemeral(future, 10) {
		if item.ID == entry.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("a finished ephemeral conversation past its deadline must be listed for cleanup")
	}

	// A non-ephemeral conversation must never be swept, whatever its age.
	keeper := store.Create(ConversationCreateRequest{Prompt: "keep me"})
	store.Complete(keeper.ID, InferenceResult{Text: "kept", ThreadID: "thread-keep"})
	for _, item := range store.ListExpiredEphemeral(time.Now().UTC().Add(72*time.Hour), 10) {
		if item.ID == keeper.ID {
			t.Fatal("non-ephemeral conversation must not be swept")
		}
	}
}
