package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestRemoteHistoryPreservesPerTurnModelSelection(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := app.State.conversations().Create(ConversationCreateRequest{Model: "requested", Prompt: "first"})
	for _, id := range []string{"first", "second"} {
		if id == "second" {
			if _, err := app.State.conversations().Continue(entry.ID, ConversationCreateRequest{Model: "requested", Prompt: "next"}); err != nil {
				t.Fatal(err)
			}
		}
		app.State.conversations().Complete(entry.ID, InferenceResult{Model: "requested", Text: "answer", MessageID: id, ModelSelectionMode: "auto_fallback", ModelObservations: []ModelObservation{{Model: id + "-model", StepID: id, Source: "stream"}}})
	}
	entry, _ = app.State.conversations().Get(entry.ID)
	before := cloneConversationEntry(&entry)
	remote := ConversationEntry{Messages: []ConversationMessage{
		{ID: "first", Role: "assistant", Content: "first answer", ModelObservations: []ModelObservation{{Model: "first-model", StepID: "first", Provider: "provider", Source: "thread_record"}}},
		{ID: "second", Role: "assistant", Content: "second answer"},
		{ID: "unrelated", Role: "assistant", Content: "external answer"},
	}}
	merged := mergeConversationEntry(entry, remote)
	for i, id := range []string{"first", "second"} {
		message := merged.Messages[i]
		if message.RequestedModel != "requested" || message.ModelSelectionMode != "auto_fallback" || len(message.ModelObservations) != 1 || message.ModelObservations[0].Model != id+"-model" {
			t.Fatalf("turn metadata lost or mixed: %+v", message)
		}
	}
	if merged.Messages[0].ModelObservations[0].Provider != "provider" || merged.Messages[2].RequestedModel != "" {
		t.Fatal("remote evidence was lost or attached to an unrelated turn")
	}
	if !reflect.DeepEqual(entry, before) || remote.Messages[0].RequestedModel != "" {
		t.Fatal("history merge mutated its input")
	}
}

func TestScheduledRefreshRespectsCredentialCooldown(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].CredentialCooldownUntil = time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	cfg.SessionRefresh.Enabled = true
	cfg.SessionRefresh.AutoSwitch = false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unexpected refresh while cooling down", http.StatusTooManyRequests)
	}))
	defer server.Close()
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	before, _, _ := app.State.Snapshot()
	if err := app.State.RefreshSession(t.Context(), "periodic_check"); err == nil || calls != 0 {
		t.Fatalf("cooling credential was refreshed: calls=%d err=%v", calls, err)
	}
	after, _, _ := app.State.Snapshot()
	if !reflect.DeepEqual(before.Accounts, after.Accounts) {
		t.Fatal("deferred refresh changed login state or cooldown")
	}
}

func TestConversationReadAndDeleteUseBoundWorkspace(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].Workspaces = append(cfg.Accounts[0].Workspaces, NotionWorkspace{ID: "bound-space", PlanType: "business"})
	reads, deletes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/api/v3/syncRecordValuesSpaceInitial":
			reads++
			for _, request := range sliceValue(payload["requests"]) {
				if mapValue(mapValue(request)["pointer"])["spaceId"] != "bound-space" {
					t.Error("read used the account's default workspace")
				}
			}
			records := buildThreadErrorRecordMap("bound-thread", "bound-space", "upstream-message", "", "", "trace")
			value := unwrapRecordValue(mapValue(records["thread_message"])["upstream-message"])
			value["step"] = map[string]any{"type": "agent-inference", "value": []any{map[string]any{"type": "text", "content": "remote answer"}}}
			value["data"] = map[string]any{"completed": true}
			_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": records})
		case "/api/v3/saveTransactionsFanout":
			deletes++
			transaction := mapValue(sliceValue(payload["transactions"])[0])
			operation := mapValue(sliceValue(transaction["operations"])[0])
			if operation["id"] != "bound-thread" || mapValue(operation["args"])["alive"] != false {
				t.Error("deleted a different thread")
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected upstream endpoint: %s", r.URL.Path)
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "question"})
	app.State.conversations().Complete(entry.ID, InferenceResult{Text: "local answer", MessageID: "upstream-message", ThreadID: "bound-thread", AccountEmail: "primary@example.com", SpaceID: "bound-space", ModelSelectionMode: "auto"})
	request := httptest.NewRequest(http.MethodGet, "/admin/conversations/"+entry.ID, nil)
	request.Header.Set("X-Admin-Token", "test-admin-token")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	var response struct {
		Item ConversationEntry `json:"item"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || reads != 2 || len(response.Item.Messages) != 1 || response.Item.Messages[0].Content != "remote answer" || response.Item.Messages[0].ModelSelectionMode != "auto" {
		t.Fatalf("bound conversation was not read correctly: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, err := app.notionClientForWorkspace(t.Context(), "primary@example.com", "missing"); err == nil {
		t.Fatal("missing workspace silently fell back")
	}
	if err := app.deleteConversation(entry.ID); err != nil || deletes != 1 {
		t.Fatalf("bound conversation could not be deleted: calls=%d err=%v", deletes, err)
	}
	if _, exists := app.State.conversations().Get(entry.ID); exists {
		t.Fatal("deleted conversation remains in local history")
	}
}
