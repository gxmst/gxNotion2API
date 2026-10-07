package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebChatFirstTurnPersistsInitialization(t *testing.T) {
	for _, image := range []bool{false, true} {
		a := newConversationRequestTestApp(t)
		called := false
		a.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, _ func(string) error) (InferenceResult, error) {
			called = true
			if req.SuppressUpstreamThreadPersistence {
				t.Error("web chat discarded initialization required for continuation")
			}
			return InferenceResult{Text: "ok", ThreadID: req.preparedThreadID}, nil
		}
		payload := map[string]any{"prompt": "first turn", "stream": false}
		if image {
			payload["attachments"] = []any{map[string]any{"name": "test.png", "content_type": "image/png", "data": "data:image/png;base64,aW1hZ2U="}}
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, conversationTestHTTPRequest(t, "/admin/test", payload))
		if !called || rec.Code != 200 {
			t.Fatalf("image=%v status=%d body=%s", image, rec.Code, rec.Body.String())
		}
	}
}

func TestContinuationRepairsOnlyConfirmedMissingMetadata(t *testing.T) {
	for _, repair := range []bool{false, true} {
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		}))
		cfg := defaultConfig()
		cfg.UpstreamBaseURL = server.URL
		cfg.UpstreamOrigin = server.URL
		client := newProtocolTestClient(cfg)
		draft := &continuationTurnDraft{MissingConfig: repair, MissingContext: repair}
		_, err := client.saveContinuationScaffold(context.Background(), "thread-1", "continue", draft, "")
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		txn := mapValue(sliceValue(body["transactions"])[0])
		ops := sliceValue(txn["operations"])
		count := map[string]int{}
		for _, raw := range ops {
			op := mapValue(raw)
			step := mapValue(mapValue(op["args"])["step"])
			count[stringValue(step["type"])]++
		}
		want := 0
		if repair {
			want = 1
		}
		if count["config"] != want || count["context"] != want || count["user"] != 1 {
			t.Fatalf("repair=%v step counts=%v", repair, count)
		}
		if repair && (draft.ConfigID == "" || draft.ContextID == "") {
			t.Fatal("repair did not carry persisted IDs into inference")
		}
	}
}

func TestContinuationDraftReadsSingleAndDoubleWrappedRecords(t *testing.T) {
	for _, double := range []bool{false, true} {
		record := map[string]any{"step": map[string]any{"id": "config-1", "type": "config", "value": map[string]any{"model": "auto"}}}
		wrapped := map[string]any{"value": record}
		if double {
			wrapped = map[string]any{"value": wrapped}
		}
		draft := extractContinuationDraftFromThreadMessages(map[string]any{"config-1": wrapped}, []string{"config-1"})
		if draft == nil || draft.ConfigID != "config-1" {
			t.Fatalf("double=%v missing existing config", double)
		}
	}
}

func TestContinuationMetadataRequiresCompleteLiveRecords(t *testing.T) {
	for _, scenario := range []string{"missing-thread", "missing-message", "empty-thread", "missing-config", "missing-context", "complete"} {
		t.Run(scenario, func(t *testing.T) {
			ids := []string{"config", "context"}
			if scenario == "empty-thread" {
				ids = []string{}
			}
			if scenario == "missing-config" {
				ids = []string{"context"}
			}
			if scenario == "missing-context" {
				ids = []string{"config"}
			}
			records := map[string]any{"thread": map[string]any{"thread-1": map[string]any{"value": map[string]any{"messages": ids}}}, "thread_message": map[string]any{}}
			for _, id := range ids {
				mapValue(records["thread_message"])[id] = map[string]any{"value": map[string]any{"step": map[string]any{"id": id, "type": id, "value": map[string]any{}}}}
			}
			if scenario == "missing-thread" {
				delete(records, "thread")
			}
			if scenario == "missing-message" {
				delete(mapValue(records["thread_message"]), "config")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": records})
			}))
			defer server.Close()
			cfg := defaultConfig()
			cfg.UpstreamBaseURL = server.URL
			cfg.UpstreamOrigin = server.URL
			draft, err := newProtocolTestClient(cfg).prepareContinuationDraftFromThread(context.Background(), "thread-1")
			if scenario == "missing-thread" || scenario == "missing-message" {
				if err == nil || draft != nil {
					t.Fatal("incomplete read authorized metadata repair")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if draft.MissingConfig != (scenario == "missing-config" || scenario == "empty-thread") || draft.MissingContext != (scenario == "missing-context" || scenario == "empty-thread") {
				t.Fatalf("wrong repair flags: %+v", draft)
			}
		})
	}
}
