package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeleteMessagePersistsAndSurvivesRemoteMerge(t *testing.T) {
	a := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, a)
	user, answer := conversationMessageIDs(entry)
	r := conversationTestHTTPRequest(t, "/admin/conversations/"+entry.ID+"/messages/"+answer, nil)
	r.Method = http.MethodDelete
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	stored, ok, err := a.State.Store.LoadConversation(entry.ID)
	if err != nil || !ok || len(stored.Messages) != 1 || stored.Messages[0].ID != user {
		t.Fatalf("delete did not persist: %v %v %+v", ok, err, stored.Messages)
	}
	merged := mergeConversationEntry(stored, entry)
	if len(merged.Messages) != 1 || merged.Messages[0].ID != user {
		t.Fatal("remote merge resurrected deleted message")
	}
	denied := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, r.URL.Path, nil)
	a.ServeHTTP(denied, req)
	if denied.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated delete accepted")
	}
}

func TestDeleteMessageRejectsRunningConversation(t *testing.T) {
	a := newConversationRequestTestApp(t)
	entry := a.State.conversations().Create(ConversationCreateRequest{Prompt: "active"})
	if _, err := a.State.conversations().DeleteMessage(entry.ID, entry.Messages[0].ID); err == nil {
		t.Fatal("deleted a running turn")
	}
}

func TestDeleteLegacyMessagesSurvivesSyncInEitherOrder(t *testing.T) {
	for _, order := range [][]string{{"local-user", "answer-2"}, {"answer-2", "local-user"}} {
		a := newConversationRequestTestApp(t)
		local := ConversationEntry{ID: "legacy", Status: "completed", MessageID: "answer-2", Messages: []ConversationMessage{
			{ID: "user-1", Role: "user", Content: "keep question"},
			{ID: "answer-1", Role: "assistant", Content: "keep answer"},
			{ID: "local-user", Role: "user", Content: "delete question"},
			{ID: "answer-2", Role: "assistant", Content: "delete answer"},
		}}
		remote := cloneConversationEntry(&local)
		remote.Messages[2].ID = "remote-user"
		a.State.conversations().ImportRemote(local)
		for step, id := range order {
			updated, err := a.State.conversations().DeleteMessage(local.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			a.State.persistConversationSnapshot(local.ID)
			stored, found, err := a.State.Store.LoadConversation(local.ID)
			if err != nil || !found {
				t.Fatalf("reload: %v %v", found, err)
			}
			merged := mergeConversationEntry(stored, remote)
			if len(merged.Messages) != 3-step || merged.Messages[0].ID != "user-1" || merged.Messages[1].ID != "answer-1" {
				t.Fatalf("order=%v step=%d incorrect merged messages: %+v (local=%+v)", order, step, merged.Messages, updated.Messages)
			}
			for _, deleted := range order[:step+1] {
				for _, message := range merged.Messages {
					if message.ID == deleted || (deleted == "local-user" && message.ID == "remote-user") {
						t.Fatal("deleted message resurrected")
					}
				}
			}
		}
	}
}

func TestAdminRemoteContinuationKeepsOriginalThread(t *testing.T) {
	a := newConversationRequestTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v3/syncRecordValuesSpaceInitial" {
			t.Errorf("unexpected read %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": map[string]any{
			"thread":         map[string]any{"original-thread": map[string]any{"value": map[string]any{"value": map[string]any{"messages": []string{"old-user"}, "space_id": "space-primary"}}}},
			"thread_message": map[string]any{"old-user": map[string]any{"value": map[string]any{"value": map[string]any{"step": map[string]any{"id": "old-user", "type": "user", "value": [][]string{{"old prompt"}}}}}}},
		}})
	}))
	defer server.Close()
	cfg, _, _ := a.State.Snapshot()
	cfg.UpstreamBaseURL = server.URL
	cfg.UpstreamOrigin = server.URL
	if err := a.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	called := false
	a.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, _ func(string) error) (InferenceResult, error) {
		called = true
		if req.UpstreamThreadID != "original-thread" {
			t.Fatalf("continued wrong thread %q", req.UpstreamThreadID)
		}
		return InferenceResult{Text: "continued", ThreadID: req.UpstreamThreadID}, nil
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, conversationTestHTTPRequest(t, "/admin/test", map[string]any{"prompt": "next", "conversation_id": "notion_thread:original-thread", "stream": false}))
	if rec.Code != 200 || !called {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	entry, ok := a.State.conversations().Get("notion_thread:original-thread")
	if !ok || entry.ThreadID != "original-thread" || len(entry.Messages) != 3 {
		t.Fatalf("lost original history: %+v", entry)
	}
}
