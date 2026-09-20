package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The suffix-matching fallback exists to rescue continuations across volatile
// drift (IP, user agent), so it must only bridge conversations created under
// the exact same stable client scope.
func TestContinuationFallbackRespectsClientScope(t *testing.T) {
	store := newConversationStore()
	scoped := "profile=openai\ntransport=chat_completions\nmodel=gpt-5.4"
	entry := store.Create(ConversationCreateRequest{
		Prompt:      "hello",
		Transport:   "chat_completions",
		ClientScope: scoped,
	})
	store.Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "thread-a", AccountEmail: "a@example.com"})

	legacy := store.Create(ConversationCreateRequest{Prompt: "hello"})
	store.Complete(legacy.ID, InferenceResult{Text: "answer", ThreadID: "thread-b", AccountEmail: "a@example.com"})

	history := []conversationPromptSegment{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "answer"},
	}
	if found, ok := store.FindContinuationBySegments(history, scoped); !ok || found.ID != entry.ID {
		t.Fatalf("an identical client scope must match its own entry: found=%+v ok=%v", found, ok)
	}
	if _, ok := store.FindContinuationBySegments(history, scoped+"\nclient=other"); ok {
		t.Fatal("fallback must not bridge different client scopes")
	}
	// A scope-less lookup only reaches legacy entries that never recorded a
	// scope; scoped entries stay isolated from it.
	if found, ok := store.FindContinuationBySegments(history, ""); !ok || found.ID != legacy.ID {
		t.Fatalf("legacy entries stay reachable for scope-less callers: found=%+v ok=%v", found, ok)
	}
}

// A busy or deleting conversation is a conflict: the request already carries
// the upstream thread, so silently creating a fresh local record would run a
// second turn against the same thread in parallel.
func TestStartConversationTurnReportsBusyConversation(t *testing.T) {
	state := &ServerState{Conversations: newConversationStore()}
	app := &App{State: state}
	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "first"})
	request := PromptRunRequest{UpstreamThreadID: "thread-1", Prompt: "second"}

	if _, err := app.startConversationTurn(entry.ID, "", "api", "chat_completions", "second", request); !isConversationTurnConflict(err) {
		t.Fatalf("expected a busy conflict, got %v", err)
	}
	if got := len(state.conversations().List()); got != 1 {
		t.Fatalf("busy conflict created extra conversations: %d", got)
	}

	// A deleting conversation must also surface as a conflict. The claim
	// itself refuses busy conversations, so complete this one first.
	deleting := state.conversations().Create(ConversationCreateRequest{Prompt: "third"})
	state.conversations().Complete(deleting.ID, InferenceResult{Text: "done", ThreadID: "thread-2"})
	if _, err := state.conversations().ClaimForDeletion(deleting.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.startConversationTurn(deleting.ID, "", "api", "chat_completions", "again", request); !isConversationTurnConflict(err) {
		t.Fatalf("expected a deleting conflict, got %v", err)
	}
}

func TestStreamedRepeatTurnReplaysCachedAnswerWithoutDispatch(t *testing.T) {
	request := PromptRunRequest{
		Prompt:       "hello",
		replayResult: &InferenceResult{Text: "cached answer", ThreadID: "thread-1", AccountEmail: "a@example.com"},
	}
	var streamed strings.Builder
	app := &App{runPromptStreamSinkOverride: func(_ *http.Request, _ PromptRunRequest, _ InferenceStreamSink) (InferenceResult, error) {
		t.Error("sink override reached despite an armed replay")
		return InferenceResult{}, nil
	}}
	result, err := app.runPromptStreamWithSink(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request, InferenceStreamSink{
		Text: func(delta string) error {
			streamed.WriteString(delta)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "cached answer" || result.Text != "cached answer" {
		t.Fatalf("stream replay mismatch: streamed=%q result=%q", streamed.String(), result.Text)
	}

	onDeltaCalled := false
	result, err = app.runPromptStream(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request, func(delta string) error {
		onDeltaCalled = true
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !onDeltaCalled || result.Text != "cached answer" {
		t.Fatalf("onDelta replay mismatch: called=%v result=%q", onDeltaCalled, result.Text)
	}
}

// Message counts alone cannot distinguish a repeated request from an edited
// final message; only the exact final turn may replay the cached answer.
func TestRequestMatchesConversationFinalTurnRejectsEditedMessage(t *testing.T) {
	conversation := ConversationEntry{
		Messages: []ConversationMessage{
			{Role: "user", Content: "explain A"},
			{Role: "assistant", Content: "answer"},
		},
	}
	request := PromptRunRequest{}
	if !requestMatchesConversationFinalTurn(request, []conversationPromptSegment{{Role: "user", Text: "explain A"}}, conversation) {
		t.Fatal("identical final turn must match")
	}
	if requestMatchesConversationFinalTurn(request, []conversationPromptSegment{{Role: "user", Text: "explain B"}}, conversation) {
		t.Fatal("an edited final message must not match")
	}
	withAttachment := PromptRunRequest{Attachments: []InputAttachment{{Name: "notes.txt", ContentType: "text/plain", Data: []byte("notes")}}}
	if requestMatchesConversationFinalTurn(withAttachment, []conversationPromptSegment{{Role: "user", Text: "explain A"}}, conversation) {
		t.Fatal("an attachment change must not match")
	}
	conversation.Messages[0].Attachments = summarizeInputAttachments(withAttachment.Attachments)
	if !requestMatchesConversationFinalTurn(withAttachment, []conversationPromptSegment{{Role: "user", Text: "explain A"}}, conversation) {
		t.Fatal("the same attachment set must match")
	}
}

func TestContinuationFallbackClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		peer      string
		userAgent string
		firstID   string
		secondID  string
		wantMatch bool
	}{
		{name: "same client new port", peer: "192.0.2.1:2000", userAgent: "client-a", wantMatch: true},
		{name: "different peer", peer: "192.0.2.2:2000", userAgent: "client-a"},
		{name: "different user agent", peer: "192.0.2.1:2000", userAgent: "client-b"},
		{name: "different explicit clients", peer: "192.0.2.1:2000", userAgent: "client-a", firstID: "a", secondID: "b"},
		{name: "explicit identity survives connection change", peer: "192.0.2.2:2000", userAgent: "client-b", firstID: "a", secondID: "a", wantMatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{State: &ServerState{Conversations: newConversationStore()}}
			first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			first.RemoteAddr = "192.0.2.1:1000"
			first.Header.Set("User-Agent", "client-a")
			first.Header.Set("X-Client-ID", tc.firstID)
			second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			second.RemoteAddr = tc.peer
			second.Header.Set("User-Agent", tc.userAgent)
			second.Header.Set("X-Client-ID", tc.secondID)
			scope := requestClientContinuationScope(first, "openai", "", "chat_completions", "gpt-5.4", "", "")
			entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "hello", ClientScope: scope})
			app.State.conversations().Complete(entry.ID, InferenceResult{Text: "hi", ThreadID: "thread-first-client"})
			segments := []conversationPromptSegment{{Role: "user", Text: "hello"}, {Role: "assistant", Text: "hi"}, {Role: "user", Text: "next question"}}
			fingerprint := canonicalConversationFingerprintScoped(requestClientFingerprintScope(second, "openai", "", "chat_completions", "gpt-5.4", "", ""), "", segments)
			clientScope := requestClientContinuationScope(second, "openai", "", "chat_completions", "gpt-5.4", "", "")
			target, matched := app.resolveContinuationConversationWithExplicit("", fingerprint, clientScope, segments, "", "")
			if matched != tc.wantMatch || (matched && target.Conversation.ID != entry.ID) {
				t.Fatalf("matched=%v want=%v target=%q", matched, tc.wantMatch, target.Conversation.ID)
			}
		})
	}
}

func TestSillyTavernBindingFallbackRespectsClientScope(t *testing.T) {
	for _, scopeKind := range []string{"same", "different", "legacy"} {
		t.Run(scopeKind, func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			payload := map[string]any{"type": "normal", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
			ctx, err := buildSillyTavernContext(payload)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			clientScope := requestClientContinuationScope(r, "sillytavern", ctx.ProfileKey, "chat_completions", "gpt-5.4", "", "")
			entryScope := clientScope
			if scopeKind == "different" {
				entryScope += "\nclient=another"
			} else if scopeKind == "legacy" {
				entryScope = ""
			}
			entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "hello", ClientScope: entryScope})
			app.completeConversation(entry.ID, InferenceResult{Text: "hi", ThreadID: "thread-binding"})
			app.persistSillyTavernBinding(entry.ID, ctx.ProfileKey, ctx.Mode)
			// A single user message cannot match the general history fallback.
			// The saved binding is the only possible implicit continuation here.
			target, matched := app.resolveSillyTavernContinuation(r, payload, ctx, "missing-fingerprint", clientScope)
			if matched != (scopeKind == "same") || (matched && target.Target.Conversation.ID != entry.ID) {
				t.Fatalf("binding bypassed client scope: matched=%v target=%q", matched, target.Target.Conversation.ID)
			}
		})
	}
}

func TestRepeatTurnRequiresPersistedAttachmentContent(t *testing.T) {
	original := InputAttachment{Name: "image.png", ContentType: "image/png", Data: []byte("image-A")}
	conversation := ConversationEntry{Messages: []ConversationMessage{{Role: "user", Content: "describe this", Attachments: summarizeInputAttachments([]InputAttachment{original})}}}
	raw, err := json.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	var restored ConversationEntry
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		attachment InputAttachment
		legacy     bool
		wantMatch  bool
	}{
		{name: "same inline bytes after reload", attachment: original, wantMatch: true},
		{name: "same name different bytes", attachment: InputAttachment{Name: original.Name, ContentType: original.ContentType, Data: []byte("image-B")}},
		{name: "different filename", attachment: InputAttachment{Name: "other.png", ContentType: original.ContentType, Data: original.Data}},
		{name: "mutable URL", attachment: InputAttachment{Name: original.Name, ContentType: original.ContentType, URL: "https://example.invalid/image.png"}},
		{name: "mutable path", attachment: InputAttachment{Name: original.Name, ContentType: original.ContentType, Path: "image.png"}},
		{name: "legacy record without digest", attachment: original, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := cloneConversationEntry(&restored)
			if tc.legacy {
				entry.Messages[0].Attachments[0].ContentSHA256 = ""
			}
			matched := requestMatchesConversationFinalTurn(PromptRunRequest{Attachments: []InputAttachment{tc.attachment}}, []conversationPromptSegment{{Role: "user", Text: "describe this"}}, entry)
			if matched != tc.wantMatch {
				t.Fatalf("attachment replay match=%v want=%v", matched, tc.wantMatch)
			}
		})
	}
}

func eligibleTestAccount(t *testing.T, cfg AppConfig) NotionAccount {
	t.Helper()
	account := ensureAccountPaths(cfg, NotionAccount{Email: "cooldown@example.com", PlanType: "business"})
	if err := os.MkdirAll(filepath.Dir(account.StorageStatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(account.StorageStatePath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return account
}

// A dispatch failure must apply the computed cooldown so the account really
// steps back instead of being retried on the very next request.
func TestDispatchFailureAppliesCooldown(t *testing.T) {
	cfg := defaultConfig()
	account := eligibleTestAccount(t, cfg)
	now := time.Now()

	failed := markAccountDispatchFailure(account, now, errors.New("boom"), true)
	if !accountCooldownActive(failed, now) {
		t.Fatalf("failure did not apply a cooldown: cooldown_until=%q", failed.CooldownUntil)
	}
	if elapsed := parseOptionalRFC3339(failed.CooldownUntil); elapsed.IsZero() || !elapsed.After(now) {
		t.Fatalf("cooldown_until = %q, want a future timestamp", failed.CooldownUntil)
	}
	if ok, reason := accountDispatchEligible(cfg, failed, now); ok || reason != "cooldown" {
		t.Fatalf("cooled-down account still dispatchable: ok=%v reason=%q", ok, reason)
	}

	after := now.Add(computeAccountCooldown(failed, true) + time.Minute)
	if ok, reason := accountDispatchEligible(cfg, failed, after); !ok || reason != "ready" {
		t.Fatalf("account stayed ineligible after the cooldown: ok=%v reason=%q", ok, reason)
	}
}

// A locally budgeted account with an exhausted window must stop being picked
// until the window resets.
func TestQuotaExhaustedAccountIsNotDispatchable(t *testing.T) {
	cfg := defaultConfig()
	account := eligibleTestAccount(t, cfg)
	now := time.Now()

	exhausted := account
	exhausted.HourlyQuota = 2
	exhausted.WindowStartedAt = formatRFC3339OrEmpty(now)
	exhausted.WindowRequestCount = 2
	if ok, reason := accountDispatchEligible(cfg, exhausted, now); ok || reason != "quota_exhausted" {
		t.Fatalf("exhausted account still dispatchable: ok=%v reason=%q", ok, reason)
	}

	reset := now.Add(2 * time.Hour)
	if ok, reason := accountDispatchEligible(cfg, exhausted, reset); !ok || reason != "ready" {
		t.Fatalf("account did not recover after the window reset: ok=%v reason=%q", ok, reason)
	}

	unbudgeted := account
	unbudgeted.HourlyQuota = 0
	unbudgeted.WindowRequestCount = 500
	if ok, reason := accountDispatchEligible(cfg, unbudgeted, now); !ok || reason != "ready" {
		t.Fatalf("accounts without a local budget must stay dispatchable: ok=%v reason=%q", ok, reason)
	}
}

// End to end: a session persisted under the handler-computed fingerprint must
// be found again by that same fingerprint, and an identical message count with
// an edited final message must run upstream instead of replaying.
func TestChatCompletionsRepeatTurnRoundTripsPersistedFingerprint(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "test-api-key"
	cfg.Storage.SQLitePath = filepath.Join(t.TempDir(), "state.db")
	cfg.Storage.PersistConversations = true
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}

	modelID := "gpt-5.4"
	entry := state.conversations().Create(ConversationCreateRequest{
		Source:       "api",
		Transport:    "chat_completions",
		Model:        modelID,
		Prompt:       "A",
		UseWebSearch: cfg.Features.UseWebSearch,
	})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "X", ThreadID: "thread-e2e", AccountEmail: "seed@example.com"})
	if _, err := state.conversations().Continue(entry.ID, ConversationCreateRequest{Prompt: "B", UseWebSearch: cfg.Features.UseWebSearch}); err != nil {
		t.Fatal(err)
	}
	state.conversations().Complete(entry.ID, InferenceResult{Text: "Y", ThreadID: "thread-e2e", AccountEmail: "seed@example.com"})

	segments := []conversationPromptSegment{
		{Role: "user", Text: "A"},
		{Role: "assistant", Text: "X"},
		{Role: "user", Text: "B"},
	}
	scopeRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fingerprint := canonicalConversationFingerprintScoped(
		requestClientFingerprintScope(scopeRequest, "openai", "", "chat_completions", modelID, "", ""),
		"", segments,
	)
	app.persistConversationSession(entry.ID, PromptRunRequest{
		SessionFingerprint: fingerprint,
		RawMessageCount:    len(segments),
	}, InferenceResult{ThreadID: "thread-e2e", AccountEmail: "seed@example.com"})

	buildRequest := func(finalUser string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", mustJSONBody(t, map[string]any{
			"model": modelID,
			"messages": []map[string]any{
				{"role": "user", "content": "A"},
				{"role": "assistant", "content": "X"},
				{"role": "user", "content": finalUser},
			},
		}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-api-key")
		return req
	}

	// A true repeat replays the cached answer without any dispatch.
	dispatched := false
	app.runPromptOverride = func(_ *http.Request, _ PromptRunRequest) (InferenceResult, error) {
		dispatched = true
		return InferenceResult{Text: "fresh", ThreadID: "thread-e2e"}, nil
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, buildRequest("B"))
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat turn status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if dispatched {
		t.Fatal("a repeated turn must replay the cached answer, not dispatch")
	}
	if len(payload.Choices) == 0 || payload.Choices[0].Message.Content != "Y" {
		t.Fatalf("repeat turn did not replay the cached answer: %s", rec.Body.String())
	}

	// Same message count, edited final message: must run upstream.
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, buildRequest("B2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("edited turn status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !dispatched {
		t.Fatal("an edited final message must not replay the cached answer")
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Choices) == 0 || payload.Choices[0].Message.Content != "fresh" {
		t.Fatalf("edited turn did not run upstream: %s", rec.Body.String())
	}
}
