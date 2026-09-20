package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestExpiredResponseBodyStaysUnavailableAfterTTLIncrease(t *testing.T) {
	cfg := newSQLiteStoreTestConfig(filepath.Join(t.TempDir(), "state.sqlite"))
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "question"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "retained-thread"})
	entry, _ = state.conversations().Get(entry.ID)
	if err := state.Store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	if err := state.Store.SaveResponse("expired", map[string]any{"id": "expired"}, time.Now().Add(-2*time.Hour), entry.ID, entry.ThreadID, ""); err != nil {
		t.Fatal(err)
	}
	if err := state.Store.DeleteExpiredResponses(time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Responses.StoreTTLSeconds = 3 * 60 * 60
	state, err = newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.getResponse("expired"); ok {
		t.Error("increasing TTL resurrected a cleared response body")
	}
	if _, ok := state.getContinuationResponse("expired"); !ok {
		t.Error("cleared response body lost its continuation link")
	}
}

func TestWorkspaceRefreshDistinguishesEmptyFromIncompleteMetadata(t *testing.T) {
	for _, kind := range []string{"empty", "missing_root", "missing_spaces"} {
		t.Run(kind, func(t *testing.T) {
			app := newConversationRequestTestApp(t)
			cfg, _, _ := app.State.Snapshot()
			account, _, _ := cfg.FindAccount("primary@example.com")
			session, err := loadSessionInfoForAccountRefresh(cfg, account)
			if err != nil {
				t.Fatal(err)
			}
			records := spaceRecordMap(session.UserID, nil)
			if kind == "missing_root" {
				delete(records, "user_root")
			} else if kind == "missing_spaces" {
				records = spaceRecordMap(session.UserID, []discoveredSpaceCandidate{{ID: account.SpaceID}})
				delete(records, "space")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v3/loadUserContent" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": records})
			}))
			defer server.Close()
			cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
			if err := app.State.SaveAndApply(cfg); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/admin/accounts/refresh-workspaces", map[string]any{"email": account.Email}))
			live, _, _ := app.State.Snapshot()
			current, _, _ := live.FindAccount(account.Email)
			eligible, _ := accountWorkspaceEligibility(current)
			if kind == "empty" {
				if rec.Code != http.StatusOK || eligible {
					t.Fatalf("empty membership did not revoke old entitlement: status=%d eligible=%v", rec.Code, eligible)
				}
			} else if rec.Code != http.StatusBadGateway || !eligible {
				t.Fatalf("incomplete metadata changed entitlements: status=%d eligible=%v", rec.Code, eligible)
			}
		})
	}
}

func TestSessionRefreshDoesNotRestoreRevokedWorkspaceEntitlement(t *testing.T) {
	app := newConversationRequestTestApp(t)
	startedCfg, _, _ := app.State.Snapshot()
	started, _, _ := startedCfg.FindAccount("primary@example.com")
	if err := app.State.applyWorkspaceDiscovery(started, []discoveredSpaceCandidate{{ID: started.SpaceID, PlanType: started.PlanType, SubscriptionTier: "free"}}); err != nil {
		t.Fatal(err)
	}
	committed, err := app.State.commitAccountRefresh(startedCfg, started, startedCfg)
	if err != nil {
		t.Fatal(err)
	}
	current, _, _ := committed.FindAccount(started.Email)
	if eligible, _ := accountWorkspaceEligibility(current); eligible {
		t.Fatal("in-flight session refresh restored a revoked workspace entitlement")
	}
}

func TestCredentialCooldownSurvivesWorkspaceRemoval(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	account, _, _ := cfg.FindAccount("primary@example.com")
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].Workspaces = []NotionWorkspace{{ID: "remaining", PlanType: "business"}}
	cfg.Accounts[0].DefaultWorkspaceID, cfg.Accounts[0].SpaceID = "remaining", "remaining"
	cfg.ActiveWorkspaceID = "remaining"
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	err := app.State.finishWorkspaceDispatchFailure(account.Email, account.SpaceID, time.Now(), &notionAPIError{StatusCode: http.StatusTooManyRequests}, false)
	if err != nil {
		t.Fatal(err)
	}
	live, _, _ := app.State.Snapshot()
	remaining, _, _ := live.FindAccountWorkspace(account.Email, "remaining")
	if eligible, reason := accountDispatchEligible(live, remaining, time.Now()); eligible || reason != "credential_cooldown" {
		t.Fatalf("removed workspace lost account-wide cooldown: eligible=%v reason=%s", eligible, reason)
	}
}

func TestRefreshRetryHonorsCredentialAndWorkspaceState(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, scenario := range []string{"rate_limit", "trust_denied", "paused_during_refresh", "revoked_during_refresh"} {
			t.Run(fmt.Sprintf("%s/stream=%v", scenario, streaming), func(t *testing.T) {
				app := newConversationRequestTestApp(t)
				cfg, _, _ := app.State.Snapshot()
				cfg.Accounts = cloneAccounts(cfg.Accounts[:1])
				account := &cfg.Accounts[0]
				dir := t.TempDir()
				account.ProfileDir = dir
				account.StorageStatePath = filepath.Join(dir, "storage.json")
				account.PendingStatePath = filepath.Join(dir, "pending.json")
				session, err := loadSessionInfoForAccountRefresh(cfg, *account)
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/login":
						_, _ = fmt.Fprint(w, `<html data-notion-version="test-refresh-version"></html>`)
					case "/api/v3/getSpacesInitial":
						if scenario == "paused_during_refresh" {
							if err := app.State.finishWorkspaceDispatchFailure(account.Email, account.SpaceID, time.Now(), &notionAPIError{StatusCode: http.StatusTooManyRequests}, false); err != nil {
								t.Error(err)
							}
						} else if scenario == "revoked_during_refresh" {
							if err := app.State.applyWorkspaceDiscovery(*account, []discoveredSpaceCandidate{{ID: account.SpaceID, PlanType: account.PlanType, SubscriptionTier: "free"}}); err != nil {
								t.Error(err)
							}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"users": map[string]any{session.UserID: map[string]any{
							"user_root": map[string]any{session.UserID: map[string]any{"value": map[string]any{"value": map[string]any{
								"space_view_pointers": []any{map[string]any{"spaceId": session.SpaceID, "id": "view"}},
							}}}},
						}}})
					default:
						t.Errorf("unexpected refresh request: %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
				cfg.SessionRefresh.Enabled = true
				if err := app.State.SaveAndApply(cfg); err != nil {
					t.Fatal(err)
				}
				failure := &notionAPIError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Now().Add(time.Hour)}
				if scenario == "trust_denied" {
					failure.StatusCode, failure.Message = http.StatusForbidden, "trust-rule-denied"
				}
				calls, cooldownSaves := 0, 0
				previousSave := testHookSaveAndApply
				t.Cleanup(func() { testHookSaveAndApply = previousSave })
				testHookSaveAndApply = func(state *ServerState, next AppConfig) error {
					if (scenario == "rate_limit" || scenario == "trust_denied") && next.Accounts[0].CredentialCooldownUntil != "" {
						cooldownSaves++
						if got := state.loadAccountSlots()[credentialSlotKey(account.Email)].inflight.Load(); got != 1 {
							t.Errorf("retry released credential capacity before recording cooldown: inflight=%d", got)
						}
					}
					return state.saveAndApplyLocked(next)
				}
				app.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
					calls++
					if calls == 1 {
						return InferenceResult{}, &notionAPIError{StatusCode: http.StatusUnauthorized}
					}
					return InferenceResult{}, failure
				}
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				if streaming {
					_, err = app.runPromptWithAccountPoolWithSink(r, PromptRunRequest{Prompt: "hello"}, InferenceStreamSink{})
				} else {
					_, err = app.runPromptWithAccountPool(r, PromptRunRequest{Prompt: "hello"}, nil)
				}
				if scenario == "rate_limit" || scenario == "trust_denied" {
					if !errors.Is(err, failure) || calls != 2 || cooldownSaves != 1 {
						t.Fatalf("unexpected refresh retry: calls=%d cooldown_saves=%d err=%v", calls, cooldownSaves, err)
					}
				} else if calls != 1 || err == nil {
					t.Fatalf("retry ignored concurrent account state: calls=%d err=%v", calls, err)
				}
				if got := app.State.loadAccountSlots()[credentialSlotKey(account.Email)].inflight.Load(); got != 0 {
					t.Fatalf("retry leaked credential capacity: %d", got)
				}
			})
		}
	}
}
