package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newConversationRequestTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.APIKey = "test-api-key"
	cfg.Admin.Password = "test-admin-password"
	cfg.Storage.SQLitePath = filepath.Join(dir, "state.db")
	cfg.Accounts = []NotionAccount{
		{PlanType: "business", Email: "primary@example.com", SpaceID: "space-primary", ProbeJSON: writeSyntheticProbeFile(t, dir, "primary@example.com", "space-primary")},
		{PlanType: "business", Email: "backup@example.com", SpaceID: "space-backup", ProbeJSON: writeSyntheticProbeFile(t, dir, "backup@example.com", "space-backup")},
	}
	cfg.ActiveAccount = "primary@example.com"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	state.AdminTokens["test-admin-token"] = time.Now().Add(time.Hour)
	app := &App{State: state}
	app.accountProtocolProbeOverride = func(context.Context, AppConfig, SessionInfo) error { return nil }
	return app
}

func conversationTestHTTPRequest(t *testing.T, path string, payload map[string]any) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, mustJSONBody(t, payload))
	r.Header.Set("Authorization", "Bearer test-api-key")
	r.Header.Set("X-Admin-Token", "test-admin-token")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestResponsesPartialStreamPreservesFailureAndExecutionTarget(t *testing.T) {
	app := newConversationRequestTestApp(t)
	threadID := "thread-partial-response"
	app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
		request.onThreadPrepared(threadID)
		if err := emit("partial answer"); err != nil {
			return InferenceResult{}, err
		}
		return InferenceResult{}, errors.New("upstream truncated")
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/responses", map[string]any{
		"model": "gpt-5.4", "input": "first question", "stream": true, "conversation_id": "partial-response",
	}))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "upstream truncated") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream did not report its failure: status=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("partial output was reported as completed: %s", body)
	}
	entry, ok := app.State.conversations().Get("partial-response")
	if !ok || entry.Status != "failed" || entry.ThreadID != threadID || entry.AccountEmail != "primary@example.com" || len(entry.Messages) != 2 {
		t.Fatalf("failed conversation lost its execution target or history: %+v", entry)
	}
	if entry.Messages[1].Status != "failed" || entry.Messages[1].Content != "partial answer" {
		t.Fatalf("partial answer was not preserved as failed: %+v", entry.Messages[1])
	}
	app.State.sqliteWriter.Close()
	responses, err := app.State.Store.LoadResponses(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := responses[entry.ResponseID]
	if !ok || stored.ThreadID != threadID || stored.AccountEmail != entry.AccountEmail || stored.ConversationID != entry.ID {
		t.Fatalf("failed response lost its execution target: %+v", stored)
	}
	response, ok := app.State.getResponse(entry.ResponseID)
	if !ok || response["status"] != "failed" {
		t.Fatalf("saved response is not failed: %+v", response)
	}
	if !strings.Contains(body, `"status":"incomplete"`) || !strings.Contains(body, "partial answer") {
		t.Fatalf("failed response does not expose its partial output: %s", body)
	}
}

func TestRefreshSessionUsesRealSaveWithoutDeadlock(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			cfg, _, _ := app.State.Snapshot()
			cfg.SessionRefresh.AutoSwitch = false
			if err := app.State.ApplyConfig(cfg); err != nil {
				t.Fatal(err)
			}
			previous := testHookTryRefreshAccount
			t.Cleanup(func() { testHookTryRefreshAccount = previous })
			refreshErr := errors.New("synthetic refresh failure")
			testHookTryRefreshAccount = func(_ context.Context, cfg AppConfig, account NotionAccount) (AppConfig, error) {
				cfg.Accounts = cloneAccounts(cfg.Accounts)
				account.LastRefreshAt = "2026-01-01T00:00:00Z"
				cfg.UpsertAccount(account)
				if failed {
					return cfg, refreshErr
				}
				return cfg, nil
			}
			done := make(chan error, 1)
			go func() { done <- app.State.RefreshSession(context.Background(), "test-real-save") }()
			select {
			case err := <-done:
				if failed != errors.Is(err, refreshErr) || (!failed && err != nil) {
					t.Fatalf("unexpected refresh result: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("refresh blocked while saving its configuration")
			}
			cfg, _, _ = app.State.Snapshot()
			account, _, _ := cfg.FindAccount("primary@example.com")
			if account.LastRefreshAt != "2026-01-01T00:00:00Z" {
				t.Fatal("refresh result was not applied by the real save path")
			}
			if _, started, err := app.State.beginAccountDispatch(account.Email, time.Now()); err != nil || !started {
				t.Fatalf("dispatch did not resume after refresh: started=%v err=%v", started, err)
			}
		})
	}
}

func TestNewHTTPConversationsPersistExecutionTargetBeforeInference(t *testing.T) {
	for _, surface := range []string{"chat", "responses", "sillytavern", "admin"} {
		for _, stream := range []bool{false, true} {
			if surface == "admin" && stream {
				continue
			}
			name := surface + map[bool]string{false: "/sync", true: "/stream"}[stream]
			t.Run(name, func(t *testing.T) {
				app := newConversationRequestTestApp(t)
				conversationID := ""
				assertTarget := func(threadID string) {
					t.Helper()
					items, err := app.State.Store.LoadConversations()
					if err != nil || len(items) != 1 {
						t.Fatalf("persisted conversations=%d err=%v", len(items), err)
					}
					if items[0].ID != conversationID || items[0].ThreadID != threadID || items[0].AccountEmail != "primary@example.com" || items[0].Status != "running" {
						t.Fatalf("execution target was not persisted before inference: %+v", items[0])
					}
				}
				app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
					conversationID = request.ConversationID
					if conversationID == "" || request.preparedThreadID == "" || request.onThreadPrepared == nil {
						t.Fatal("generated conversation ID did not reach execution-target preparation")
					}
					assertTarget(request.preparedThreadID)
					request.onThreadPrepared("thread-returned-by-upload")
					assertTarget("thread-returned-by-upload")
					if emit != nil {
						if err := emit("fresh answer"); err != nil {
							return InferenceResult{}, err
						}
					}
					return InferenceResult{Text: "fresh answer", ThreadID: "thread-returned-by-upload"}, nil
				}
				path := "/v1/chat/completions"
				payload := map[string]any{"model": "gpt-5.4", "stream": stream, "messages": []map[string]any{{"role": "user", "content": "first question"}}}
				switch surface {
				case "responses":
					path = "/v1/responses"
					payload = map[string]any{"model": "gpt-5.4", "stream": stream, "input": "first question"}
				case "sillytavern":
					payload["type"] = "normal"
				case "admin":
					path = "/admin/test"
					payload = map[string]any{"model": "gpt-5.4", "prompt": "first question"}
				}
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, conversationTestHTTPRequest(t, path, payload))
				if rec.Code != http.StatusOK || conversationID == "" || !strings.Contains(rec.Body.String(), "fresh answer") {
					t.Fatalf("request failed: conversation=%q status=%d body=%s", conversationID, rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestSillyTavernContinueGeneratesAndPersistsAnotherTurn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "tell a story"})
			result := InferenceResult{Text: "unfinished story", ThreadID: "thread-story", AccountEmail: "primary@example.com"}
			app.completeConversation(entry.ID, result)
			app.persistConversationSession(entry.ID, PromptRunRequest{RawMessageCount: 1}, result)
			dispatched := false
			app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
				dispatched = true
				if request.Prompt != sillyTavernContinuePrompt || request.SessionRepeatTurn || request.replayResult != nil {
					t.Fatalf("continue was treated as a cached repeat: %+v", request)
				}
				if emit != nil {
					if err := emit("new continuation"); err != nil {
						return InferenceResult{}, err
					}
				}
				return InferenceResult{Text: "new continuation", ThreadID: request.UpstreamThreadID}, nil
			}
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{
				"model": "gpt-5.4", "type": "continue", "stream": stream, "conversation_id": entry.ID,
				"messages": []map[string]any{{"role": "user", "content": "tell a story"}, {"role": "assistant", "content": "unfinished story"}},
			}))
			if rec.Code != http.StatusOK || !dispatched || !strings.Contains(rec.Body.String(), "new continuation") {
				t.Fatalf("continue failed: dispatched=%v status=%d body=%s", dispatched, rec.Code, rec.Body.String())
			}
			session, ok, err := app.State.Store.LoadConversationSessionByConversationID(entry.ID)
			if err != nil || !ok || session.TurnCount != 2 {
				t.Fatalf("continued generation was not recorded as a new turn: %+v err=%v", session, err)
			}
		})
	}
}

func TestSillyTavernAuxiliaryUsesAccountPool(t *testing.T) {
	for _, mode := range []string{"quiet", "impersonate"} {
		for _, stream := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/sync", true: "/stream"}[stream], func(t *testing.T) {
				app := newConversationRequestTestApp(t)
				dispatched := false
				app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
					dispatched = true
					if request.UpstreamThreadID != "" || request.continuationDraft != nil || !request.SuppressUpstreamThreadPersistence || !request.EphemeralConversation {
						t.Fatalf("auxiliary request did not use a fresh ephemeral thread: %+v", request)
					}
					if emit != nil {
						if err := emit("auxiliary answer"); err != nil {
							return InferenceResult{}, err
						}
					}
					return InferenceResult{Text: "auxiliary answer", ThreadID: request.preparedThreadID}, nil
				}
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{
					"model": "gpt-5.4", "type": mode, "stream": stream,
					"messages": []map[string]any{{"role": "user", "content": "summarize this"}},
				}))
				if rec.Code != http.StatusOK || !dispatched {
					t.Fatalf("auxiliary request rejected: status=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestSillyTavernContinueKeepsInstructionDuringQuotaFailover(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "tell a story"})
			result := InferenceResult{Text: "unfinished story", ThreadID: "thread-story", AccountEmail: "primary@example.com"}
			app.completeConversation(entry.ID, result)
			app.persistConversationSession(entry.ID, PromptRunRequest{RawMessageCount: 1}, result)
			if err := app.State.finishAccountDispatchFailure("primary@example.com", time.Now(), errors.New(syntheticQuotaExhaustedError), false); err != nil {
				t.Fatal(err)
			}
			dispatched := false
			app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, session SessionInfo, request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
				dispatched = true
				if session.UserEmail != "backup@example.com" || request.UpstreamThreadID != "" {
					t.Fatal("continuation did not move to a fresh backup thread")
				}
				if !strings.Contains(request.Prompt, "unfinished story") || strings.Count(request.Prompt, sillyTavernContinuePrompt) != 1 {
					t.Fatalf("failover lost or duplicated the continue instruction: %s", request.Prompt)
				}
				if emit != nil {
					if err := emit("new continuation"); err != nil {
						return InferenceResult{}, err
					}
				}
				return InferenceResult{Text: "new continuation", ThreadID: request.preparedThreadID}, nil
			}
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{
				"model": "gpt-5.4", "type": "continue", "stream": streaming, "conversation_id": entry.ID,
				"messages": []map[string]any{{"role": "user", "content": "tell a story"}, {"role": "assistant", "content": "unfinished story"}},
			}))
			if rec.Code != http.StatusOK || !dispatched || !strings.Contains(rec.Body.String(), "new continuation") {
				t.Fatalf("continue failover failed: dispatched=%v status=%d body=%s", dispatched, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHTTPAttachmentReplayChecksContent(t *testing.T) {
	for _, surface := range []string{"chat", "responses", "sillytavern"} {
		t.Run(surface, func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			calls := 0
			app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
				calls++
				if len(request.Attachments) != 1 {
					t.Fatal("attachment did not reach dispatch")
				}
				text := "answer for " + string(request.Attachments[0].Data)
				if emit != nil {
					if err := emit(text); err != nil {
						return InferenceResult{}, err
					}
				}
				return InferenceResult{Text: text, ThreadID: firstNonEmpty(request.UpstreamThreadID, request.preparedThreadID)}, nil
			}
			for index, content := range []string{"image-A", "image-A", "image-B"} {
				path := "/v1/chat/completions"
				payload := map[string]any{
					"model": "gpt-5.4", "stream": index > 0,
					"messages":    []map[string]any{{"role": "user", "content": "describe this"}},
					"attachments": []map[string]any{{"name": "image.png", "content_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte(content))}},
				}
				if surface == "responses" {
					path = "/v1/responses"
					delete(payload, "messages")
					payload["input"] = "describe this"
				} else if surface == "sillytavern" {
					payload["type"] = "normal"
				}
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, conversationTestHTTPRequest(t, path, payload))
				wantCalls := 1
				if index == 2 {
					wantCalls = 2
				}
				if rec.Code != http.StatusOK || calls != wantCalls || !strings.Contains(rec.Body.String(), "answer for "+content) {
					t.Fatalf("attachment request %d: calls=%d want=%d status=%d body=%s", index, calls, wantCalls, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
