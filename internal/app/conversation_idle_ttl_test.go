package app

import (
	"testing"
	"time"
)

// The ST quiet TTL must stay at its built-in 10m when ephemeral_all is off and
// no explicit ephemeral_ttl_seconds is configured.
func TestVerifySillyTavernTTLRestored(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = false
		cfg.Features.EphemeralTTLSeconds = 0
	})
	store := app.State.conversations()
	entry := store.Create(ConversationCreateRequest{
		Ephemeral:       true,
		EphemeralReason: "sillytavern_quiet",
		Prompt:          "hello",
	})
	store.Complete(entry.ID, InferenceResult{Text: "hi", ThreadID: "t1"})
	updated, _ := store.Get(entry.ID)
	remaining := time.Until(*updated.AutoDeleteAt)
	t.Logf("quiet lifetime = %v (want ~%v)", remaining, sillyTavernQuietConversationTTL)
	if remaining < 9*time.Minute {
		t.Errorf("still shortened: %v", remaining)
	}
}

// An explicit ephemeral_ttl_seconds must still win.
func TestVerifyExplicitTTLStillOverrides(t *testing.T) {
	app := newEphemeralConfigTestApp(t, func(cfg *AppConfig) {
		cfg.Features.EphemeralAllConversations = true
		cfg.Features.EphemeralTTLSeconds = 3600
	})
	store := app.State.conversations()
	entry := store.Create(ConversationCreateRequest{Ephemeral: true, Prompt: "x"})
	store.Complete(entry.ID, InferenceResult{Text: "y", ThreadID: "t2"})
	updated, _ := store.Get(entry.ID)
	remaining := time.Until(*updated.AutoDeleteAt)
	t.Logf("configured lifetime = %v (want ~1h)", remaining)
	if remaining < 30*time.Minute {
		t.Errorf("explicit TTL ignored: %v", remaining)
	}
}

// Idle sweeping: a normal conversation older than the idle TTL is listed, a
// fresh one is not, and a running one never is.
func TestVerifyIdleSweep(t *testing.T) {
	app := newEphemeralConfigTestApp(t, nil)
	store := app.State.conversations()

	stale := store.Create(ConversationCreateRequest{Prompt: "old"})
	store.Complete(stale.ID, InferenceResult{Text: "done", ThreadID: "t-old"})
	fresh := store.Create(ConversationCreateRequest{Prompt: "new"})
	store.Complete(fresh.ID, InferenceResult{Text: "done", ThreadID: "t-new"})
	running := store.Create(ConversationCreateRequest{Prompt: "busy"})

	// Look from 25h in the future with a 24h idle TTL: both finished ones are
	// stale by then, so first check the boundary from just past 24h.
	idleTTL := 24 * time.Hour
	now := time.Now().UTC()

	if items := store.ListIdleConversations(now, idleTTL, 10); len(items) != 0 {
		t.Fatalf("nothing should be idle yet, got %d", len(items))
	}

	future := now.Add(25 * time.Hour)
	ids := map[string]bool{}
	for _, item := range store.ListIdleConversations(future, idleTTL, 10) {
		ids[item.ID] = true
	}
	if !ids[stale.ID] || !ids[fresh.ID] {
		t.Errorf("finished conversations past the idle TTL must be listed, got %v", ids)
	}
	if ids[running.ID] {
		t.Error("a running conversation must never be swept")
	}

	// Disabling it must stop all sweeping.
	if items := store.ListIdleConversations(future, 0, 10); len(items) != 0 {
		t.Errorf("idle_ttl=0 must disable sweeping, got %d", len(items))
	}
}

func TestVerifyIdleTTLConfigDefaults(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	if got := configuredConversationIdleTTL(cfg); got != 24*time.Hour {
		t.Errorf("absent config ttl = %v, want 24h", got)
	}
	zero := 0
	cfg.Features.ConversationIdleTTLHours = &zero
	if got := configuredConversationIdleTTL(cfg); got != 0 {
		t.Errorf("explicit 0 ttl = %v, want disabled", got)
	}
	six := 6
	cfg.Features.ConversationIdleTTLHours = &six
	if got := configuredConversationIdleTTL(cfg); got != 6*time.Hour {
		t.Errorf("explicit 6 ttl = %v, want 6h", got)
	}
}
