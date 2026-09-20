package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestExpiredResponseContinuesExistingThreadAcrossRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "memory"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.APIKey = "test-api-key"
			cfg.Storage.SQLitePath = filepath.Join(t.TempDir(), "state.sqlite")
			cfg.Accounts = []NotionAccount{{Email: "owner@example.com", SpaceID: "business", PlanType: "business"}}
			state, err := newServerState(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = state.Close() })
			entry := state.conversations().Create(ConversationCreateRequest{Prompt: "first question", Model: "gpt-5.4"})
			state.conversations().Complete(entry.ID, InferenceResult{Text: "first answer", ThreadID: "thread-retained", AccountEmail: "owner@example.com", SpaceID: "business"})
			entry, _ = state.conversations().Get(entry.ID)
			if err := state.Store.SaveConversation(entry); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-48 * time.Hour)
			record := StoredResponse{CreatedAt: old, Payload: map[string]any{"id": "resp-old", "output_text": "first answer"}, ConversationID: entry.ID, ThreadID: entry.ThreadID, AccountEmail: entry.AccountEmail}
			state.mu.Lock()
			state.ResponseStore.save("resp-old", record, old)
			state.mu.Unlock()
			if err := state.Store.SaveResponse("resp-old", record.Payload, old, entry.ID, entry.ThreadID, entry.AccountEmail); err != nil {
				t.Fatal(err)
			}
			if restart {
				if err := state.Close(); err != nil {
					t.Fatal(err)
				}
				state, err = newServerState(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, found := state.getResponse("resp-old"); found {
				t.Fatal("expired response body remains readable")
			}
			if _, found := state.getContinuationResponse("resp-old"); !found {
				t.Fatal("expired body lost continuation link")
			}
			app := &App{State: state}
			calls := 0
			app.runPromptOverride = func(_ *http.Request, request PromptRunRequest) (InferenceResult, error) {
				calls++
				if request.UpstreamThreadID != "thread-retained" || request.Prompt != "next question" || request.ConversationID != entry.ID {
					t.Fatalf("expired response replayed history or changed threads: %+v", request)
				}
				return InferenceResult{Text: "next answer", ThreadID: request.UpstreamThreadID, AccountEmail: "owner@example.com", SpaceID: "business"}, nil
			}
			request := func(workspace string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/responses", map[string]any{"model": "gpt-5.4", "previous_response_id": "resp-old", "workspace_id": workspace, "input": "next question"}))
				return rec
			}
			if rec := request("other"); rec.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("expired response bypassed workspace ownership: %d %s", rec.Code, rec.Body.String())
			}
			if rec := request(""); rec.Code != http.StatusOK || calls != 1 {
				t.Fatalf("continuation failed: %d %s", rec.Code, rec.Body.String())
			}
			if err := state.conversations().Delete(entry.ID); err != nil {
				t.Fatal(err)
			}
			state.deleteResponsesByConversationOrThread(entry.ID, entry.ThreadID)
			if rec := request(""); rec.Code != http.StatusNotFound || calls != 1 {
				t.Fatalf("deleted thread resurrected: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
