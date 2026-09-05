package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistoryEditsForkAndExactReplaysDoNotDuplicateMessages(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/st/chat/completions"} {
		for _, streaming := range []bool{false, true} {
			t.Run(endpoint+map[bool]string{false: "/sync", true: "/stream"}[streaming], func(t *testing.T) {
				app := newConversationRequestTestApp(t)
				calls := 0
				var captured PromptRunRequest
				app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, emit func(string) error) (InferenceResult, error) {
					calls++
					captured = req
					if emit != nil {
						if err := emit("answer"); err != nil {
							return InferenceResult{}, err
						}
					}
					return InferenceResult{Text: "answer", ThreadID: req.preparedThreadID}, nil
				}
				messages := []map[string]any{{"role": "user", "content": "A"}, {"role": "assistant", "content": "X"}, {"role": "user", "content": "B"}, {"role": "assistant", "content": "Y"}, {"role": "user", "content": "C"}}
				payload := map[string]any{"model": "gpt-5.4", "messages": messages, "stream": streaming}
				if endpoint == "/v1/responses" {
					delete(payload, "messages")
					payload["input"] = messages
				}
				send := func() string {
					t.Helper()
					rec := httptest.NewRecorder()
					app.ServeHTTP(rec, conversationTestHTTPRequest(t, endpoint, payload))
					if rec.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					return rec.Header().Get("X-Conversation-ID")
				}
				id := send()
				if repeated := send(); repeated != id || calls != 1 {
					t.Fatalf("exact replay changed target or dispatched: id=%s calls=%d", repeated, calls)
				}
				entry, _ := app.State.conversations().Get(id)
				if len(entry.Messages) != 6 {
					t.Fatalf("replay duplicated history: messages=%d", len(entry.Messages))
				}
				messages[2]["content"] = "edited B"
				if fork := send(); fork == id || calls != 2 || captured.UpstreamThreadID != "" {
					t.Fatalf("edited middle turn reused the old branch: fork=%s calls=%d thread=%s", fork, calls, captured.UpstreamThreadID)
				}
				if !strings.Contains(captured.Prompt, "edited B") {
					t.Fatal("fork lost edited history")
				}
				forkID := captured.ConversationID
				messages[2]["content"] = "edited\n    B"
				if fork := send(); fork == forkID || calls != 3 || !strings.Contains(captured.Prompt, "edited\n    B") {
					t.Fatalf("whitespace edit reused history or lost formatting: calls=%d", calls)
				}
			})
		}
	}
}

func TestQuotaFailoverKeepsImportedHistoryAndSystemPrompt(t *testing.T) {
	app := newConversationRequestTestApp(t)
	calls := 0
	var captured PromptRunRequest
	app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, session SessionInfo, req PromptRunRequest, emit func(string) error) (InferenceResult, error) {
		calls++
		captured = req
		if calls == 2 && session.UserEmail != "backup@example.com" {
			t.Fatal("failover did not use backup account")
		}
		return InferenceResult{Text: "answer", ThreadID: req.preparedThreadID}, nil
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-5.4", "messages": []map[string]any{
			{"role": "system", "content": "Preserve line breaks."},
			{"role": "user", "content": "MEMO_7391\n    indented context"}, {"role": "assistant", "content": "remembered"}, {"role": "user", "content": "first local question"},
		},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get("X-Conversation-ID")
	persisted, err := app.State.Store.LoadConversations()
	if err != nil || len(persisted) != 1 || len(persisted[0].Messages) != 4 {
		t.Fatalf("imported history was not persisted: err=%v", err)
	}
	if err := app.State.finishAccountDispatchFailure("primary@example.com", time.Now(), errors.New(syntheticQuotaExhaustedError), false); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-5.4", "conversation_id": id,
		"messages": []map[string]any{{"role": "user", "content": "what is the memo"}},
	}))
	if rec.Code != http.StatusOK || calls != 2 {
		t.Fatalf("failover failed: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	if !strings.Contains(captured.Prompt, "MEMO_7391\n    indented context") || captured.HiddenPrompt != "Preserve line breaks." {
		t.Fatalf("failover lost original context: %+v", captured)
	}
	session, ok, err := app.State.Store.LoadConversationSessionByConversationID(id)
	if err != nil || !ok || session.SpaceID != "space-backup" || session.TurnCount != 1 {
		t.Fatalf("failover retained old session metadata: %+v err=%v", session, err)
	}
}

func TestConfiguredWorkspaceOverridesProbeAndRejectsOldConversation(t *testing.T) {
	app := newConversationRequestTestApp(t)
	calls := 0
	app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, _ func(string) error) (InferenceResult, error) {
		calls++
		return InferenceResult{Text: "answer", ThreadID: req.preparedThreadID}, nil
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/admin/test", map[string]any{"prompt": "first", "conversation_id": "workspace-bound"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("first status=%d", rec.Code)
	}
	entry, _ := app.State.conversations().Get("workspace-bound")
	if entry.SpaceID != "space-primary" {
		t.Fatalf("conversation missing workspace: %q", entry.SpaceID)
	}
	stored, ok, err := app.State.Store.LoadConversationSessionByConversationID(entry.ID)
	if err != nil || !ok || stored.SpaceID != entry.SpaceID {
		t.Fatal("session workspace did not round-trip through SQLite")
	}
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].SpaceID = "selected-business-space"
	cfg.Accounts[0].SpaceViewID = "selected-view"
	if err := app.State.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	session, err := loadSessionInfoForAccountRefresh(cfg, cfg.Accounts[0])
	if err != nil || session.SpaceID != "selected-business-space" || session.SpaceViewID != "selected-view" {
		t.Fatalf("configured workspace ignored: %+v err=%v", session, err)
	}
	_, snapshot, _ := app.State.Snapshot()
	if snapshot.SpaceID != session.SpaceID {
		t.Fatal("active fallback uses a different workspace")
	}
	for _, endpoint := range []string{"/admin/test", "/v1/chat/completions"} {
		rec = httptest.NewRecorder()
		app.ServeHTTP(rec, conversationTestHTTPRequest(t, endpoint, map[string]any{"model": "gpt-5.4", "conversation_id": entry.ID, "prompt": "next", "messages": []map[string]any{{"role": "user", "content": "next"}}}))
		if rec.Code == http.StatusOK || calls != 1 {
			t.Fatalf("old thread sent in new workspace: endpoint=%s status=%d calls=%d", endpoint, rec.Code, calls)
		}
	}
}

func TestAccountProxyOverridesBothSchemes(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProxyMode = "http"
	cfg.ProxyURL = "http://global.invalid:8080"
	cfg.Accounts = []NotionAccount{{Email: "account@example.com", ProxyMode: "http", ProxyURL: "http://account.invalid:8081", ProxyHTTPSURL: "http://secure.invalid:8082"}}
	for _, scheme := range []string{"http", "https"} {
		target, _ := url.Parse(scheme + "://www.notion.so/api/v3/test")
		proxy, _, err := NewProxyResolver(cfg).ResolveProxyForRequest("account@example.com", target)
		want := map[string]string{"http": "account.invalid:8081", "https": "secure.invalid:8082"}[scheme]
		if err != nil || proxy == nil || proxy.Host != want {
			t.Fatalf("%s proxy=%v err=%v", scheme, proxy, err)
		}
	}
}

func TestHealthProbeUsesAccountProxy(t *testing.T) {
	var direct, proxied atomic.Int32
	handler := func(counter *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[]}`))
		}
	}
	upstream := httptest.NewServer(handler(&direct))
	defer upstream.Close()
	proxy := httptest.NewServer(handler(&proxied))
	defer proxy.Close()
	cfg := defaultConfig()
	cfg.ProxyMode = "off"
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin = upstream.URL, upstream.URL
	cfg.Accounts = []NotionAccount{{Email: "account@example.com", ProxyMode: "http", ProxyURL: proxy.URL}}
	app := &App{}
	err := app.probeAccountProtocolHealth(context.Background(), cfg, SessionInfo{UserEmail: "account@example.com", UserID: "user", SpaceID: "space", Cookies: []ProbeCookie{{Name: "token_v2", Value: "synthetic-token"}}}, "account@example.com")
	if err != nil || direct.Load() != 0 || proxied.Load() == 0 {
		t.Fatalf("probe route: direct=%d proxied=%d err=%v", direct.Load(), proxied.Load(), err)
	}
}

func TestNativeTransportFallbackRequiresOptIn(t *testing.T) {
	cfg := defaultConfig()
	client := newNotionAIClient(SessionInfo{}, cfg, "")
	if client.FallbackHTTPClient != nil {
		t.Fatal("native fallback enabled without opt-in")
	}
	cfg.Features.AllowNativeTransportFallback = true
	client = newNotionAIClient(SessionInfo{}, cfg, "")
	if client.FallbackHTTPClient == nil {
		t.Fatal("native fallback opt-in ignored")
	}
}

func TestSillyTavernHistoryKeepsInteriorWhitespace(t *testing.T) {
	ctx, err := buildSillyTavernContext(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "memo\n    indented line"},
			map[string]any{"role": "assistant", "content": "answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.RequestSegments) != 2 || ctx.RequestSegments[0].Text != "memo\n    indented line" {
		t.Fatalf("SillyTavern history was normalized too aggressively: %+v", ctx.RequestSegments)
	}
}

func TestLegacyContinuationChecksWorkspaceBeforeWriting(t *testing.T) {
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/syncRecordValuesSpaceInitial" {
			writes.Add(1)
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		reads.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"recordMap": map[string]any{"thread": map[string]any{"legacy-thread": map[string]any{"value": map[string]any{"value": map[string]any{"space_id": "original-space"}}}}}})
	}))
	defer server.Close()
	client := newBestEffortTestClient(server.URL)
	request := PromptRunRequest{Prompt: "next", UpstreamThreadID: "legacy-thread", Attachments: []InputAttachment{{Name: "note.txt", ContentType: "text/plain", Data: []byte("note")}}}
	if _, err := client.RunPrompt(context.Background(), request); !errors.Is(err, errConversationWorkspaceMismatch) {
		t.Fatalf("workspace mismatch was not rejected: %v", err)
	}
	if reads.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("workspace check occurred after a write: reads=%d writes=%d", reads.Load(), writes.Load())
	}
}

func TestExplicitAccountClientNeverFallsBackToActiveClient(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[1].ProbeJSON = t.TempDir() + "/missing-probe.json"
	cfg.Accounts[1].StorageStatePath = t.TempDir() + "/missing-storage.json"
	if err := app.State.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := app.notionClientForAccount(context.Background(), "backup@example.com"); err == nil || !strings.Contains(err.Error(), "load account session") {
		t.Fatalf("missing explicit account session silently fell back: %v", err)
	}
	if _, err := app.notionClientForAccount(context.Background(), "removed@example.com"); err == nil {
		t.Fatal("removed account fell back to active account")
	}
}

func TestWorkspaceColumnsMigrateExistingSession(t *testing.T) {
	app := newConversationRequestTestApp(t)
	store := app.State.Store
	session := ConversationSession{ID: "legacy-session", ConversationID: "legacy-conversation", ThreadID: "legacy-thread", AccountEmail: "primary@example.com", Status: conversationSessionStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), LastUsedAt: time.Now()}
	if err := store.SaveConversationSession(session); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"space_id", "space_view_id"} {
		if _, err := store.db.Exec("ALTER TABLE conversation_sessions DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.init(); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.LoadConversationSessionByConversationID(session.ConversationID)
	if err != nil || !ok || loaded.ThreadID != session.ThreadID || loaded.SpaceID != "" || loaded.SpaceViewID != "" {
		t.Fatalf("migration lost old session: %+v err=%v", loaded, err)
	}
	loaded.SpaceID, loaded.SpaceViewID = "verified-space", "verified-view"
	if err := store.SaveConversationSession(loaded); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err = store.LoadConversationSessionByThreadID(session.ThreadID)
	if err != nil || !ok || loaded.SpaceID != "verified-space" || loaded.SpaceViewID != "verified-view" {
		t.Fatal("migrated workspace columns did not round-trip")
	}
}
