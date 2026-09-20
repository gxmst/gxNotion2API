package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newWorkspaceRegressionState(t *testing.T) *ServerState {
	t.Helper()
	dir := t.TempDir()
	cfg := workspaceSlotTestConfig([]NotionWorkspace{
		{ID: "a-free", Status: "ready", SubscriptionTier: "free", MaxConcurrency: 2},
		{ID: "z-paid", Status: "ready", SubscriptionTier: "business", AIEnabled: true, MaxConcurrency: 2, HourlyQuota: 1000},
	})
	cfg.Storage.SQLitePath = filepath.Join(dir, "state.sqlite")
	cfg.Admin.Password = "test-admin-password"
	cfg.Accounts[0].DefaultWorkspaceID = "z-paid"
	cfg.Accounts[0].ProbeJSON = writeSyntheticProbeFile(t, dir, "user@example.com", "z-paid")
	cfg.ActiveAccount = "user@example.com"
	cfg.ActiveWorkspaceID = "z-paid"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func TestWorkspaceRuntimeUpdatesPreserveSnapshotsAndMetadata(t *testing.T) {
	state := newWorkspaceRegressionState(t)
	for _, operation := range []string{"begin", "failure", "success"} {
		t.Run(operation, func(t *testing.T) {
			before, _, _ := state.Snapshot()
			original, _ := json.Marshal(before)
			var err error
			switch operation {
			case "begin":
				var started bool
				_, started, err = state.beginWorkspaceDispatch("user@example.com", "z-paid", time.Now())
				if !started {
					t.Fatal("dispatch did not begin")
				}
			case "failure":
				err = state.finishWorkspaceDispatchFailure("user@example.com", "z-paid", time.Now(), errors.New("synthetic failure"), false)
			case "success":
				err = state.finishWorkspaceDispatchSuccess("user@example.com", "z-paid", SessionInfo{}, time.Now(), false)
			}
			if err != nil {
				t.Fatal(err)
			}
			unchanged, _ := json.Marshal(before)
			if string(original) != string(unchanged) {
				t.Fatal("published configuration snapshot was mutated")
			}
			after, _, _ := state.Snapshot()
			workspace, _ := accountWorkspace(after.Accounts[0], "z-paid")
			if workspace.SubscriptionTier != "business" || !workspace.AIEnabled {
				t.Fatalf("discovery metadata lost: %+v", workspace)
			}
		})
	}
	accounts, _, _, _, err := state.Store.LoadAccountsWithWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	w, _ := accountWorkspace(accounts[0], "z-paid")
	if w.SubscriptionTier != "business" || !w.AIEnabled {
		t.Fatal("persisted workspace metadata lost")
	}
}

func TestConfigMutatorsDoNotChangePublishedAccounts(t *testing.T) {
	for _, operation := range []string{"normalize", "upsert", "delete", "admin_edit"} {
		t.Run(operation, func(t *testing.T) {
			state := newWorkspaceRegressionState(t)
			before, _, _ := state.Snapshot()
			original, _ := json.Marshal(before)
			cfg := before
			switch operation {
			case "normalize":
				_ = normalizeConfig(cfg)
			case "upsert":
				cfg.UpsertAccount(NotionAccount{Email: "user@example.com", SpaceName: "renamed"})
			case "delete":
				cfg.DeleteAccount("user@example.com")
			case "admin_edit":
				state.AdminTokens["test-admin-token"] = time.Now().Add(time.Hour)
				req := httptest.NewRequest(http.MethodPut, "/admin/accounts", strings.NewReader(`{"email":"user@example.com","workspace_id":"z-paid","max_concurrency":3}`))
				req.Header.Set("X-Admin-Token", "test-admin-token")
				rec := httptest.NewRecorder()
				(&App{State: state}).ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("admin update: %d %s", rec.Code, rec.Body.String())
				}
			}
			unchanged, _ := json.Marshal(before)
			if string(original) != string(unchanged) {
				t.Fatal("mutation changed published accounts")
			}
		})
	}
}

func TestWorkspaceDispatchConcurrentSnapshotReaders(t *testing.T) {
	state := newWorkspaceRegressionState(t)
	before, _, _ := state.Snapshot()
	frozen := cloneAccounts(before.Accounts)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			cfg, _, _ := state.Snapshot()
			_, _ = json.Marshal(cfg)
			_, _ = json.Marshal(before)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, started, err := state.beginWorkspaceDispatch("user@example.com", "z-paid", time.Now()); err != nil || !started {
				t.Errorf("begin: %v %v", started, err)
				return
			}
			if err := state.finishWorkspaceDispatchSuccess("user@example.com", "z-paid", SessionInfo{}, time.Now(), false); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	if !reflect.DeepEqual(before.Accounts, frozen) {
		t.Fatal("concurrent writer changed frozen snapshot")
	}
}

func TestDispatchPrefersActiveAndDefaultWorkspace(t *testing.T) {
	state := newWorkspaceRegressionState(t)
	cfg, _, _ := state.Snapshot()
	// Both options are eligible here; excluded tiers are covered separately.
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].Workspaces[0].SubscriptionTier = "business"
	// Active differs from default and has lower priority: selection still wins.
	cfg.Accounts[0].DefaultWorkspaceID = "a-free"
	cfg = normalizeConfig(cfg)
	for _, request := range []PromptRunRequest{{}, {PinnedAccountEmail: "user@example.com"}, {PinnedAccountEmail: "user@example.com", AllowPinnedAccountFallback: true}} {
		candidates, err := resolveDispatchCandidates(cfg, request, time.Now())
		if err != nil || len(candidates) == 0 || accountWorkspaceID(candidates[0]) != "z-paid" {
			t.Fatalf("active workspace ignored: %v %v", candidates, err)
		}
	}
	request := PromptRunRequest{WorkspaceID: "a-free"}
	candidates, err := resolveDispatchCandidates(cfg, request, time.Now())
	if err != nil || len(candidates) != 1 || accountWorkspaceID(candidates[0]) != "a-free" {
		t.Fatalf("explicit workspace ignored: %v %v", candidates, err)
	}
	cfg.ActiveWorkspaceID = ""
	candidates, err = resolveDispatchCandidates(cfg, PromptRunRequest{}, time.Now())
	if err != nil || accountWorkspaceID(candidates[0]) != "a-free" {
		t.Fatal("default workspace ignored")
	}
	account, _, _ := cfg.FindAccountWorkspace("user@example.com", "a-free")
	account = markAccountDispatchFailure(account, time.Now(), errors.New("quota-exhausted"), false)
	cfg.UpsertAccountRuntimeState(account)
	candidates, err = resolveDispatchCandidates(cfg, PromptRunRequest{}, time.Now())
	if err != nil || len(candidates) == 0 || accountWorkspaceID(candidates[0]) != "z-paid" {
		t.Fatalf("unavailable default prevents fallback: %v %v", candidates, err)
	}
}

func TestWorkspaceScopesSeparateImplicitConversations(t *testing.T) {
	for _, surface := range []string{"chat", "responses", "sillytavern"} {
		t.Run(surface, func(t *testing.T) {
			state := newWorkspaceRegressionState(t)
			var requests []PromptRunRequest
			app := &App{State: state, runPromptOverride: func(_ *http.Request, request PromptRunRequest) (InferenceResult, error) {
				requests = append(requests, request)
				return InferenceResult{Text: "answer", ThreadID: firstNonEmpty(request.UpstreamThreadID, "thread-"+request.WorkspaceID), AccountEmail: "user@example.com", SpaceID: request.WorkspaceID, ConfigID: "config", ContextID: "context", OriginalDatetime: "2026-01-01T00:00:00Z"}, nil
			}}
			path := "/v1/chat/completions"
			if surface == "responses" {
				path = "/v1/responses"
			}
			if surface == "sillytavern" {
				path = "/v1/st/chat/completions"
			}
			messages := []any{map[string]any{"role": "user", "content": "hello"}}
			var lastID string
			for i, workspace := range []string{"a-free", "z-paid", "z-paid"} {
				payload := map[string]any{"workspace_id": workspace, "model": "gpt-5.4", "messages": messages}
				if surface == "responses" {
					payload["input"] = messages
					delete(payload, "messages")
				}
				if surface == "sillytavern" {
					payload["type"] = "normal"
				}
				req := httptest.NewRequest(http.MethodPost, path, mustJSONBody(t, payload))
				req.Header.Set("Authorization", "Bearer test-key")
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("workspace %s request: %d %s", workspace, rec.Code, rec.Body.String())
				}
				if len(requests) != i+1 {
					t.Fatal("unexpected replay or missing dispatch")
				}
				if i == 1 && requests[i].UpstreamThreadID != "" {
					t.Fatal("new workspace reused another workspace's thread")
				}
				if i == 2 && requests[i].UpstreamThreadID != "thread-z-paid" {
					t.Fatalf("same workspace did not continue: %+v", requests[i])
				}
				lastID = rec.Header().Get("X-Conversation-ID")
				messages = append(messages, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": fmt.Sprintf("next %d", i)})
			}
			payload := map[string]any{"workspace_id": "a-free", "conversation_id": lastID, "messages": messages, "model": "gpt-5.4"}
			if surface == "responses" {
				payload["input"] = messages
				delete(payload, "messages")
			}
			if surface == "sillytavern" {
				payload["type"] = "normal"
			}
			req := httptest.NewRequest(http.MethodPost, path, mustJSONBody(t, payload))
			req.Header.Set("Authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "conversation_workspace_mismatch") {
				t.Fatalf("explicit cross-workspace continuation accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
