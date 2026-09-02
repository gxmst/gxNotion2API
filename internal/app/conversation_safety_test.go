package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConversationFingerprintIsScopedByClientModelAndAccount(t *testing.T) {
	segments := []conversationPromptSegment{{Role: "user", Text: "hello"}}
	reqA := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqA.Header.Set("User-Agent", "client-a")
	reqB := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqB.Header.Set("User-Agent", "client-b")

	scopeA := requestClientFingerprintScope(reqA, "openai", "", "chat_completions", "model-a", "a@example.com")
	scopeB := requestClientFingerprintScope(reqB, "openai", "", "chat_completions", "model-a", "a@example.com")
	scopeOtherModel := requestClientFingerprintScope(reqA, "openai", "", "chat_completions", "model-b", "a@example.com")
	scopeOtherAccount := requestClientFingerprintScope(reqA, "openai", "", "chat_completions", "model-a", "b@example.com")

	fingerprint := canonicalConversationFingerprintScoped(scopeA, "", segments)
	for name, other := range map[string]string{
		"client":  canonicalConversationFingerprintScoped(scopeB, "", segments),
		"model":   canonicalConversationFingerprintScoped(scopeOtherModel, "", segments),
		"account": canonicalConversationFingerprintScoped(scopeOtherAccount, "", segments),
	} {
		if fingerprint == other {
			t.Fatalf("%s scope produced the same fingerprint", name)
		}
	}
}

func TestUnknownExplicitThreadRequiresAccountInMultiAccountMode(t *testing.T) {
	cfg := AppConfig{Accounts: []NotionAccount{{Email: "a@example.com"}, {Email: "b@example.com"}}}
	if _, err := resolveContinuationAccount(cfg, "thread-unknown", "", ConversationEntry{ThreadID: "thread-unknown"}); err == nil {
		t.Fatal("expected an account requirement for an unknown thread in multi-account mode")
	}
	if got, err := resolveContinuationAccount(cfg, "thread-unknown", "b@example.com", ConversationEntry{ThreadID: "thread-unknown"}); err != nil || got != "b@example.com" {
		t.Fatalf("got account=%q err=%v, want b@example.com", got, err)
	}
}

func TestKnownConversationRejectsAccountMismatch(t *testing.T) {
	cfg := AppConfig{Accounts: []NotionAccount{{Email: "a@example.com"}, {Email: "b@example.com"}}}
	_, err := resolveContinuationAccount(cfg, "thread-known", "b@example.com", ConversationEntry{
		ThreadID:     "thread-known",
		AccountEmail: "a@example.com",
	})
	if err == nil {
		t.Fatal("expected account ownership mismatch")
	}
}

func TestRepeatedTurnReplaysCompletedAnswerWithoutUpstreamCall(t *testing.T) {
	request := PromptRunRequest{
		Prompt:            "hello",
		PublicModel:       "model-a",
		NotionModel:       "notion-model-a",
		SessionRepeatTurn: true,
	}
	conversation := ConversationEntry{
		ID:           "conv-1",
		Status:       "completed",
		ThreadID:     "thread-1",
		AccountEmail: "a@example.com",
		Messages: []ConversationMessage{{
			ID:      "message-1",
			Role:    "assistant",
			Status:  "completed",
			Content: "cached answer",
		}},
	}
	configureRepeatTurnReplay(&request, conversation)
	if request.replayResult == nil {
		t.Fatal("expected replay result")
	}

	called := false
	app := &App{runPromptOverride: func(_ *http.Request, _ PromptRunRequest) (InferenceResult, error) {
		called = true
		return InferenceResult{}, nil
	}}
	result, err := app.runPrompt(httptest.NewRequest("POST", "/v1/chat/completions", nil), request)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("repeat turn called the upstream override")
	}
	if result.Text != "cached answer" || result.ThreadID != "thread-1" || result.AccountEmail != "a@example.com" {
		t.Fatalf("unexpected replay result: %+v", result)
	}
}

func TestConversationStoreRejectsConcurrentTurn(t *testing.T) {
	store := newConversationStore()
	entry := store.Create(ConversationCreateRequest{Prompt: "first"})
	if _, err := store.Continue(entry.ID, ConversationCreateRequest{Prompt: "second"}); err == nil {
		t.Fatal("expected concurrent turn to be rejected")
	}
}

// configureRepeatTurnReplay arms the cached-answer replay for a repeated final
// turn. Request handlers arm it via replayResultFromConversation; tests call
// this directly.
func configureRepeatTurnReplay(request *PromptRunRequest, conversation ConversationEntry) {
	request.replayResult = replayResultFromConversation(conversation)
}
