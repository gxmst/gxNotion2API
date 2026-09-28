package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsesTopLevelInstructionsReachDispatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			var captured PromptRunRequest
			app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, emit func(string) error) (InferenceResult, error) {
				captured = req
				if emit != nil {
					_ = emit("answer")
				}
				return InferenceResult{Text: "answer", ThreadID: firstNonEmpty(req.UpstreamThreadID, req.preparedThreadID)}, nil
			}
			conversationID := ""
			for _, instruction := range []string{"Answer only in French.", "Answer only in Chinese."} {
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/responses", map[string]any{
					"model": "gpt-5.4", "input": "Hello", "instructions": instruction, "stream": stream, "conversation_id": conversationID,
				}))
				if rec.Code != http.StatusOK {
					t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
				}
				conversationID = rec.Header().Get("X-Conversation-ID")
				if captured.HiddenPrompt != instruction || captured.Prompt != "Hello" {
					t.Fatalf("instructions misplaced: hidden=%q prompt=%q", captured.HiddenPrompt, captured.Prompt)
				}
				cfg, session, _ := app.State.Snapshot()
				client := &NotionAIClient{Config: cfg, Session: session}
				payload, _ := client.buildInferencePayload(captured, "thread", nil)
				if !strings.Contains(payloadInstructions(t, payload), instruction) {
					t.Fatal("instructions missing upstream")
				}
			}
		})
	}
}

func TestResponsesInstructionsSurviveCompatibilityDecoder(t *testing.T) {
	// String booleans use the existing map-based compatibility decoder.
	typed, fallback, err := decodeResponsesRequestBodyFromRaw([]byte(`{"input":"hello","instructions":"Keep line\n    breaks.","use_web_search":"false"}`))
	if err != nil || fallback == nil {
		t.Fatalf("expected compatibility decode: %v", err)
	}
	normalized, err := normalizeResponsesInputWithInstructions(typed.Input, typed.Attachments, nil, typed.Instructions)
	if err != nil || normalized.HiddenPrompt != "Keep line\n    breaks." {
		t.Fatalf("instructions lost: %+v %v", normalized, err)
	}
}

func evictCompletedConversation(t *testing.T, app *App, id string) {
	t.Helper()
	entry, ok := app.State.conversations().Get(id)
	if !ok {
		t.Fatal("missing conversation to evict")
	}
	entries := make([]ConversationEntry, 0, maxConversationEntries+1)
	for i := 0; i < maxConversationEntries; i++ {
		entries = append(entries, ConversationEntry{ID: fmt.Sprintf("filler-%d", i), Status: "completed"})
	}
	entries = append(entries, entry)
	app.State.mu.Lock()
	app.State.Conversations = newConversationStoreFromEntries(entries)
	app.State.mu.Unlock()
	if _, ok := app.State.conversations().Get(id); ok {
		t.Fatal("cache limit did not evict old entry")
	}
}

func seedEvictedConversation(t *testing.T, app *App) ConversationEntry {
	t.Helper()
	entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "original question", HiddenPrompt: "original instruction"})
	result := InferenceResult{Text: "original answer", ThreadID: "existing-thread", MessageID: "original-assistant", AccountEmail: "primary@example.com", SpaceID: "space-primary"}
	app.completeConversation(entry.ID, result)
	app.persistConversationSession(entry.ID, PromptRunRequest{RawMessageCount: 1, SessionFingerprint: "evicted-fingerprint"}, result)
	entry, _ = app.State.conversations().Get(entry.ID)
	evictCompletedConversation(t, app, entry.ID)
	return entry
}

func TestEvictedConversationContinuationPreservesHistory(t *testing.T) {
	for _, surface := range []string{"chat", "responses", "admin", "sillytavern", "thread", "expired_response", "fresh"} {
		t.Run(surface, func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			entry := seedEvictedConversation(t, app)
			path := "/v1/chat/completions"
			payload := map[string]any{"model": "gpt-5.4", "conversation_id": entry.ID, "messages": []any{map[string]any{"role": "user", "content": "follow-up"}}}
			switch surface {
			case "responses", "expired_response":
				path = "/v1/responses"
				payload["input"] = "follow-up"
				if surface == "expired_response" {
					delete(payload, "conversation_id")
					payload["previous_response_id"] = "expired-response"
					app.State.mu.Lock()
					app.State.ResponseStore.ensureInitialized()
					app.State.ResponseStore.links["expired-response"] = StoredResponse{ConversationID: entry.ID, ThreadID: entry.ThreadID, AccountEmail: entry.AccountEmail, CreatedAt: time.Now().Add(-2 * time.Hour)}
					app.State.mu.Unlock()
				}
			case "admin":
				path = "/admin/test"
				payload["prompt"] = "follow-up"
			case "sillytavern":
				payload["type"] = "normal"
			case "thread":
				delete(payload, "conversation_id")
				payload["thread_id"] = entry.ThreadID
			case "fresh":
				cfg, _, _ := app.State.Snapshot()
				cfg.Features.ForceFreshThreadPerRequest = true
				if err := app.State.ApplyConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			var captured PromptRunRequest
			app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, req PromptRunRequest, _ func(string) error) (InferenceResult, error) {
				captured = req
				return InferenceResult{Text: "new answer", ThreadID: firstNonEmpty(req.UpstreamThreadID, req.preparedThreadID)}, nil
			}
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, conversationTestHTTPRequest(t, path, payload))
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
			}
			after, found, err := app.State.Store.LoadConversation(entry.ID)
			if err != nil || !found {
				t.Fatalf("read after: found=%v err=%v", found, err)
			}
			if len(after.Messages) != 4 || after.Messages[0].Content != "original question" || after.Messages[1].Content != "original answer" {
				t.Fatalf("history overwritten: %+v", after.Messages)
			}
			if captured.HiddenPrompt != entry.HiddenPrompt || captured.ConversationID != entry.ID || captured.PinnedSpaceID != entry.SpaceID {
				t.Fatalf("lost continuation metadata: %+v", captured)
			}
			if surface == "fresh" && (!strings.Contains(captured.Prompt, "original answer") || captured.UpstreamThreadID != "") {
				t.Fatal("fresh thread lost replay history")
			}
			if surface != "fresh" && captured.UpstreamThreadID != entry.ThreadID {
				t.Fatal("original thread lost")
			}
		})
	}
}

func TestEvictedConversationFingerprintAndAdminRead(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := seedEvictedConversation(t, app)
	segments := []conversationPromptSegment{{Role: "user", Text: "original question"}, {Role: "assistant", Text: "original answer"}, {Role: "user", Text: "follow-up"}}
	matched, ok := app.resolveContinuationConversationWithExplicit("", "evicted-fingerprint", "", segments, "", "")
	if !ok || matched.Err != nil || matched.Conversation.ID != entry.ID || len(matched.Conversation.Messages) != 2 {
		t.Fatalf("fingerprint failed: %+v", matched)
	}
	evictCompletedConversation(t, app, entry.ID)
	rec := httptest.NewRecorder()
	req := conversationTestHTTPRequest(t, "/admin/conversations/"+entry.ID+"?local=1", nil)
	req.Method = http.MethodGet
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "original answer") {
		t.Fatalf("admin read: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEvictedConversationReadFailureStopsDispatch(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := seedEvictedConversation(t, app)
	if err := app.State.Store.Close(); err != nil {
		t.Fatal(err)
	}
	app.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
		t.Fatal("dispatched after storage error")
		return InferenceResult{}, nil
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{"model": "gpt-5.4", "conversation_id": entry.ID, "messages": []any{map[string]any{"role": "user", "content": "follow-up"}}}))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "conversation_load_failed") {
		t.Fatalf("storage failure hidden: %d %s", rec.Code, rec.Body.String())
	}
	if app.State.conversations().Contains(entry.ID) {
		t.Fatal("recreated conversation after read failure")
	}
}

func TestConcurrentRestoresKeepOneActiveTurn(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := seedEvictedConversation(t, app)
	var successes atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := app.startConversationTurn(entry.ID, entry.ID, "api", "chat_completions", "follow-up", PromptRunRequest{UpstreamThreadID: entry.ThreadID})
			if err == nil {
				successes.Add(1)
			} else if !isConversationTurnConflict(err) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("started %d turns", successes.Load())
	}
}

func TestResolvedConversationDeletedBeforeTurnIsNotRecreated(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := seedEvictedConversation(t, app)
	if err := app.State.Store.DeleteConversation(entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.startConversationTurn(entry.ID, entry.ID, "api", "chat_completions", "follow-up", PromptRunRequest{UpstreamThreadID: entry.ThreadID}); err == nil {
		t.Fatal("recreated a conversation deleted after resolution")
	}
	if _, found, err := app.State.Store.LoadConversation(entry.ID); err != nil || found {
		t.Fatalf("deleted row reappeared: found=%v err=%v", found, err)
	}
}

func TestUnknownExplicitThreadStillAcceptsSuppliedHistory(t *testing.T) {
	app := newConversationRequestTestApp(t)
	segments := []conversationPromptSegment{{Role: "user", Text: "first"}, {Role: "assistant", Text: "answer"}, {Role: "user", Text: "next"}}
	matched, found := app.resolveContinuationConversationWithExplicit("", "", "", segments, "", "external-thread")
	if !found || matched.Err != nil || matched.Conversation.ThreadID != "external-thread" {
		t.Fatalf("unknown explicit thread was discarded: %+v", matched)
	}
}

func TestCachePressureKeepsRunningTurnsAndDeletionClaims(t *testing.T) {
	entries := make([]ConversationEntry, maxConversationEntries+2)
	for i := range entries {
		entries[i] = ConversationEntry{ID: fmt.Sprintf("entry-%d", i), Status: "completed"}
	}
	entries[len(entries)-2].Status = "running"
	entries[len(entries)-1].Status = "deleting"
	store := newConversationStoreFromEntries(entries)
	for _, entry := range entries[len(entries)-2:] {
		if !store.Contains(entry.ID) {
			t.Fatalf("evicted %s claim", entry.Status)
		}
	}
	if len(store.List()) != maxConversationEntries {
		t.Fatal("cache did not evict idle entries")
	}
}

func TestMergedUserEditSurvivesPersistenceAndRemoteSteps(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			store := app.State.conversations()
			entry := store.Create(ConversationCreateRequest{Prompt: "original question"})
			userID := "remote-user"
			if legacy {
				userID = ""
			}
			app.completeConversation(entry.ID, InferenceResult{Text: "answer", ThreadID: "thread", MessageID: "remote-assistant", UserMessageID: userID})
			if _, err := store.SetMessageContent(entry.ID, entry.Messages[0].ID, "corrected question"); err != nil {
				t.Fatal(err)
			}
			app.State.persistConversationSnapshot(entry.ID)
			local, found, err := app.State.Store.LoadConversation(entry.ID)
			if err != nil || !found {
				t.Fatalf("reload: %v", err)
			}
			remote := ConversationEntry{ThreadID: "thread", Messages: []ConversationMessage{
				{ID: "remote-file", Role: "user", StepType: "attachment"},
				{ID: "remote-user", Role: "user", Content: "original question"},
				{ID: "remote-tool", Role: "step", StepType: "agent-tool-result"},
				{ID: "remote-assistant", Role: "assistant", Content: "answer"},
			}}
			merged := mergeConversationEntry(local, remote)
			user := merged.Messages[1]
			if user.Content != "corrected question" || user.EditedAt == nil || user.ID != entry.Messages[0].ID {
				t.Fatalf("lost edit or local identity: %+v", user)
			}
			if _, err := store.SetMessageContent(entry.ID, user.ID, "second edit"); err != nil {
				t.Fatalf("merged message cannot be edited: %v", err)
			}
		})
	}
}

func TestLegacyMergeDoesNotGuessAcrossAmbiguousUserTurns(t *testing.T) {
	now := time.Now()
	local := ConversationEntry{Messages: []ConversationMessage{{ID: "local-user", Role: "user", Content: "edited", EditedAt: &now}, {ID: "assistant", Role: "assistant"}}}
	remote := ConversationEntry{Messages: []ConversationMessage{{ID: "user-1", Role: "user", Content: "first"}, {ID: "user-2", Role: "user", Content: "second"}, {ID: "assistant", Role: "assistant"}}}
	merged := mergeConversationEntry(local, remote)
	if merged.Messages[0].Content != "first" || merged.Messages[1].Content != "second" {
		t.Fatal("applied user edit to an ambiguous turn")
	}
}

func TestInferencePayloadReportsUserStepIdentity(t *testing.T) {
	client := &NotionAIClient{Config: normalizeConfig(defaultConfig()), Session: SessionInfo{SpaceID: "space", UserID: "user"}}
	for _, scaffold := range []*continuationTurnScaffold{nil, {UserStepID: "existing-user-step"}} {
		payload, meta := client.buildInferencePayload(PromptRunRequest{Prompt: "question", continuationScaffold: scaffold}, "thread", nil)
		var userID string
		for _, step := range payload["transcript"].([]map[string]any) {
			if step["type"] == "user" {
				userID = stringValue(step["id"])
			}
		}
		if userID == "" || meta.UserMessageID != userID {
			t.Fatalf("lost user step: %q %+v", userID, meta)
		}
		if scaffold != nil && userID != scaffold.UserStepID {
			t.Fatal("continuation user identity changed")
		}
	}
}

func TestPublicCORSAllowsContinuationHeadersWithoutOpeningAdmin(t *testing.T) {
	app := newConversationRequestTestApp(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/admin/settings"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "https://chat.example.test")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,x-client-id,x-workspace-id")
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		allowed := "," + strings.ReplaceAll(strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")), " ", "") + ","
		if strings.HasPrefix(path, "/admin") {
			if rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("admin exposed cross-origin")
			}
			continue
		}
		for _, name := range []string{"authorization", "content-type", "x-client-id", "x-session-id", "openai-organization", "x-workspace-id", "x-notion-workspace-id", "x-notion-space-id", "x-conversation-id", "x-notion-conversation-id", "x-thread-id", "x-notion-thread-id", "x-account-email", "x-notion-account-email"} {
			if !strings.Contains(allowed, ","+name+",") {
				t.Errorf("%s missing %s", path, name)
			}
		}
	}
}
