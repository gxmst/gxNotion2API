package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminChatStreamContinuesAndLoadsLocalHistory(t *testing.T) {
	app := newConversationRequestTestApp(t)
	var requests []PromptRunRequest
	app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, emit func(string) error) (InferenceResult, error) {
		requests = append(requests, req)
		for _, chunk := range []string{"first ", "answer"} {
			if err := emit(chunk); err != nil {
				return InferenceResult{}, err
			}
		}
		return InferenceResult{Text: "first answer", ThreadID: firstNonEmpty(req.UpstreamThreadID, req.preparedThreadID)}, nil
	}
	for _, prompt := range []string{"first question", "next question"} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/admin/test", map[string]any{"prompt": prompt, "stream": true, "conversation_id": "admin-chat"}))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(rec.Body.String(), "[DONE]") {
			t.Fatalf("stream failed: status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Conversation-ID") != "admin-chat" {
			t.Fatal("stream lost the conversation ID")
		}
	}
	if len(requests) != 2 || requests[1].UpstreamThreadID != requests[0].preparedThreadID || requests[1].PinnedSpaceID != "space-primary" {
		t.Fatal("admin follow-up did not reuse its account, workspace and thread")
	}
	load := httptest.NewRequest(http.MethodGet, "/admin/conversations/admin-chat?local=1", nil)
	load.Header.Set("X-Admin-Token", "test-admin-token")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, load)
	var payload struct {
		Item ConversationEntry `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(payload.Item.Messages) != 4 || payload.Item.Messages[2].Content != "next question" {
		t.Fatalf("local history did not preserve both turns: status=%d", rec.Code)
	}
	unauthenticated := httptest.NewRecorder()
	app.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/admin/test", mustJSONBody(t, map[string]any{"prompt": "denied", "stream": true})))
	if unauthenticated.Code != http.StatusUnauthorized || len(requests) != 2 {
		t.Fatal("unauthenticated stream was dispatched")
	}
}

func TestAdminChatStreamCancellationPreservesPartialHistory(t *testing.T) {
	app := newConversationRequestTestApp(t)
	app.runPromptWithSessionOverride = func(ctx context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, emit func(string) error) (InferenceResult, error) {
		if err := emit("partial answer\n"); err != nil {
			return InferenceResult{}, err
		}
		<-ctx.Done()
		return InferenceResult{}, ctx.Err()
	}
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.ServeHTTP(w, r)
		close(finished)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/admin/test", mustJSONBody(t, map[string]any{"prompt": "question", "stream": true, "conversation_id": "cancel-chat"}))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Admin-Token", "test-admin-token")
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "partial answer") {
			break
		}
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel inference")
	}
	entry, _ := app.State.conversations().Get("cancel-chat")
	if entry.Status != "failed" || len(entry.Messages) != 2 || !strings.Contains(entry.Messages[1].Content, "partial answer") {
		t.Fatal("cancellation lost partial history or left the conversation running")
	}
	cfg, _, _ := app.State.Snapshot()
	account, _, _ := cfg.FindAccount("primary@example.com")
	if account.TotalFailures != 0 || account.CooldownUntil != "" {
		t.Fatal("cancellation cooled down a healthy account")
	}
	if app.State.AvailableDispatchCapacity([]string{account.Email}) == 0 {
		t.Fatal("cancellation leaked the dispatch slot")
	}
}
