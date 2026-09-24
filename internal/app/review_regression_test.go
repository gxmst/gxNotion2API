package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// healthyRefreshConfigForTest mirrors what tryRefreshAccount publishes after a
// successful login: the credential is ready again and its failure bookkeeping
// is cleared. It is built from a starting snapshot so the test controls exactly
// which concurrent writes the commit has to notice.
func healthyRefreshConfigForTest(t *testing.T, base AppConfig, account NotionAccount) AppConfig {
	t.Helper()
	refreshedCfg := base
	refreshedCfg.Accounts = cloneAccounts(base.Accounts)
	refreshed := account
	refreshed.Status = "ready"
	refreshed.LastError = ""
	refreshed.CooldownUntil = ""
	refreshed.ConsecutiveFailures = 0
	refreshed.LastRefreshAt = formatRFC3339OrEmpty(time.Now())
	refreshedCfg.UpsertAccountRuntimeState(refreshed)
	return refreshedCfg
}

// A session refresh proves the credential authenticates again, not that the
// workspace's AI allowance came back. A quota-exhausted cooldown recorded by a
// concurrent dispatch while the refresh ran must survive the refresh commit.
func TestRefreshCommitPreservesConcurrentQuotaCooldown(t *testing.T) {
	app := newConversationRequestTestApp(t)
	const email = "primary@example.com"
	const workspaceID = "space-primary"

	startedCfg, _, _ := app.State.Snapshot()
	startedParent, _, ok := startedCfg.FindAccount(email)
	if !ok {
		t.Fatal("account missing from starting snapshot")
	}
	started, ok := accountForWorkspace(startedParent, workspaceID)
	if !ok {
		t.Fatal("workspace missing from starting snapshot")
	}

	// A concurrent dispatch confirms the workspace allowance is spent.
	if err := app.State.finishWorkspaceDispatchFailure(email, workspaceID, time.Now(), errors.New("upstream reported quota-exhausted"), false); err != nil {
		t.Fatal(err)
	}
	live, _, _ := app.State.Snapshot()
	exhausted, _, ok := live.FindAccountWorkspace(email, workspaceID)
	if !ok || exhausted.CooldownUntil == "" {
		t.Fatalf("precondition failed: quota cooldown not recorded (%+v)", exhausted)
	}

	refreshedCfg := healthyRefreshConfigForTest(t, startedCfg, started)
	if _, err := app.State.commitAccountRefresh(startedCfg, started, refreshedCfg); err != nil {
		t.Fatal(err)
	}

	after, _, _ := app.State.Snapshot()
	got, _, ok := after.FindAccountWorkspace(email, workspaceID)
	if !ok {
		t.Fatal("workspace vanished after commit")
	}
	if got.CooldownUntil == "" || !strings.EqualFold(got.Status, "quota_exhausted") {
		t.Fatalf("refresh wiped a concurrent quota cooldown: status=%q cooldown=%q", got.Status, got.CooldownUntil)
	}
	if eligible, _ := accountDispatchEligible(after, got, time.Now()); eligible {
		t.Fatal("workspace became dispatchable again despite an active quota cooldown")
	}
}

// The same commit must still clear a stale cooldown when nothing raced: that is
// the whole point of the "healthy again" merge.
func TestRefreshCommitClearsUncontestedFailureBookkeeping(t *testing.T) {
	app := newConversationRequestTestApp(t)
	const email = "primary@example.com"
	const workspaceID = "space-primary"

	// The refresh starts from an account that is already in a failure state.
	if err := app.State.finishWorkspaceDispatchFailure(email, workspaceID, time.Now(), &notionAPIError{StatusCode: http.StatusUnauthorized}, true); err != nil {
		t.Fatal(err)
	}
	startedCfg, _, _ := app.State.Snapshot()
	startedParent, _, ok := startedCfg.FindAccount(email)
	if !ok {
		t.Fatal("account missing from starting snapshot")
	}
	started, ok := accountForWorkspace(startedParent, workspaceID)
	if !ok {
		t.Fatal("workspace missing from starting snapshot")
	}
	if started.CooldownUntil == "" || started.ConsecutiveFailures == 0 {
		t.Fatalf("precondition failed: starting snapshot is not in a failure state (%+v)", started)
	}

	refreshedCfg := healthyRefreshConfigForTest(t, startedCfg, started)
	if _, err := app.State.commitAccountRefresh(startedCfg, started, refreshedCfg); err != nil {
		t.Fatal(err)
	}

	after, _, _ := app.State.Snapshot()
	got, _, ok := after.FindAccountWorkspace(email, workspaceID)
	if !ok {
		t.Fatal("workspace vanished after commit")
	}
	if got.CooldownUntil != "" || got.ConsecutiveFailures != 0 {
		t.Fatalf("healthy refresh did not clear failure bookkeeping: cooldown=%q failures=%d", got.CooldownUntil, got.ConsecutiveFailures)
	}
	if !strings.EqualFold(got.Status, "ready") {
		t.Fatalf("healthy refresh did not restore ready status: %q", got.Status)
	}
}

// A continuation session can outlive its conversation when a delete removes only
// one of the two rows. The resolver must treat the conversation as gone instead
// of reviving it against a thread nobody owns any more.
func TestDeletedConversationIsNotRevivedFromContinuationSession(t *testing.T) {
	app := newConversationRequestTestApp(t)
	store := app.State.Store
	if store == nil {
		t.Fatal("test app has no persistence store")
	}
	const conversationID = "deleted-conversation"
	const threadID = "deleted-thread"
	const fingerprint = "deleted-fingerprint"
	now := time.Now()
	session := ConversationSession{
		ID:             "orphan-session",
		ConversationID: conversationID,
		Fingerprint:    fingerprint,
		ThreadID:       threadID,
		AccountEmail:   "primary@example.com",
		Status:         conversationSessionStatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastUsedAt:     now,
	}
	if err := store.SaveConversationSession(session); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadConversation(conversationID); err != nil || found {
		t.Fatalf("precondition failed: conversation row present (found=%v err=%v)", found, err)
	}

	if target, ok := app.resolveContinuationConversationWithExplicit("", "", "", nil, conversationID, ""); ok {
		t.Fatalf("deleted conversation was revived by explicit id: %+v", target)
	}
	if target, ok := app.resolveContinuationConversationWithExplicit("", fingerprint, "", nil, "", ""); ok {
		t.Fatalf("deleted conversation was revived by fingerprint: %+v", target)
	}

	// Control: once the conversation row exists again (evicted from memory but
	// still persisted, which is the normal state past maxConversationEntries),
	// the same session continues it.
	entry := ConversationEntry{
		ID:           conversationID,
		ThreadID:     threadID,
		AccountEmail: "primary@example.com",
		Status:       "complete",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	target, ok := app.resolveContinuationConversationWithExplicit("", "", "", nil, conversationID, "")
	if !ok || strings.TrimSpace(target.Conversation.ThreadID) != threadID {
		t.Fatalf("live conversation was not continued from its session: ok=%v target=%+v", ok, target)
	}
}

// A failed continuation-session delete must not remove the conversation row:
// that is the state which lets a deleted conversation be revived.
func TestFailedSessionDeleteKeepsConversationRow(t *testing.T) {
	app := newConversationRequestTestApp(t)
	store := app.State.Store
	if store == nil {
		t.Fatal("test app has no persistence store")
	}
	const conversationID = "half-deleted-conversation"
	entry := app.State.conversations().Create(ConversationCreateRequest{Prompt: "question"})
	entry.ID = conversationID
	entry.ThreadID = "half-deleted-thread"
	entry.AccountEmail = "primary@example.com"
	entry.Status = "complete"
	entry.CreatedAt, entry.UpdatedAt = time.Now(), time.Now()
	if err := store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.SaveConversationSession(ConversationSession{
		ID: "half-deleted-session", ConversationID: conversationID, ThreadID: entry.ThreadID,
		AccountEmail: entry.AccountEmail, Status: conversationSessionStatusActive,
		CreatedAt: now, UpdatedAt: now, LastUsedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Make the session delete fail deterministically.
	if _, err := store.db.Exec(`CREATE TRIGGER block_session_delete BEFORE DELETE ON conversation_sessions BEGIN SELECT RAISE(ABORT, 'blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := app.deleteConversationLocalRecords(conversationID, entry.ThreadID); err == nil {
		t.Fatal("failed session delete was reported as success")
	}
	if _, found, err := store.LoadConversation(conversationID); err != nil || !found {
		t.Fatalf("conversation row was removed despite a failed session delete: found=%v err=%v", found, err)
	}
}

// A cleared response row is a continuation link, not history: it must not
// accumulate one dead row per turn forever. Links inside the retention window
// stay, older ones are dropped outright.
func TestExpiredResponseLinksAreBoundedByRetention(t *testing.T) {
	cfg := newSQLiteStoreTestConfig(filepath.Join(t.TempDir(), "state.sqlite"))
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	store := state.Store
	if store == nil {
		t.Fatal("no persistence store")
	}
	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "question"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "retention-thread"})
	entry, _ = state.conversations().Get(entry.ID)
	if err := store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}

	bodyTTL := time.Hour
	recent := time.Now().Add(-2 * bodyTTL)
	ancient := time.Now().Add(-responseLinkRetentionFloor - 24*time.Hour)
	for _, save := range []struct {
		id        string
		createdAt time.Time
	}{{"link-recent", recent}, {"link-ancient", ancient}} {
		if err := store.SaveResponse(save.id, map[string]any{"id": save.id}, save.createdAt, entry.ID, entry.ThreadID, ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteExpiredResponses(bodyTTL); err != nil {
		t.Fatal(err)
	}

	rowState := func(responseID string) (rows int, payload string) {
		t.Helper()
		if err := store.db.QueryRow(
			`SELECT COUNT(*), COALESCE(MAX(payload_json), '') FROM responses WHERE response_id = ?`, responseID,
		).Scan(&rows, &payload); err != nil {
			t.Fatal(err)
		}
		return rows, payload
	}

	if rows, payload := rowState("link-recent"); rows != 1 || payload != "{}" {
		t.Fatalf("in-window link was not kept as a cleared row: rows=%d payload=%q", rows, payload)
	}
	if rows, _ := rowState("link-ancient"); rows != 0 {
		t.Fatalf("out-of-window link was not deleted: rows=%d", rows)
	}
}

// The SillyTavern continue path resolves its own target before falling back to
// the shared resolver, so it needs the same deleted-conversation guard: a
// leftover session row must not let a deleted conversation be revived (and, via
// Create with the requested id, re-persisted) by an explicit id or thread id.
func TestDeletedConversationIsNotRevivedBySillyTavernContinue(t *testing.T) {
	app := newConversationRequestTestApp(t)
	store := app.State.Store
	if store == nil {
		t.Fatal("test app has no persistence store")
	}
	const conversationID = "st-deleted-conversation"
	const threadID = "st-deleted-thread"
	now := time.Now()
	if err := store.SaveConversationSession(ConversationSession{
		ID:             "st-orphan-session",
		ConversationID: conversationID,
		ThreadID:       threadID,
		AccountEmail:   "primary@example.com",
		Status:         conversationSessionStatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastUsedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadConversation(conversationID); err != nil || found {
		t.Fatalf("precondition failed: conversation row present (found=%v err=%v)", found, err)
	}

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"explicit conversation id", map[string]any{
			"type":            "continue",
			"conversation_id": conversationID,
			"messages":        []any{map[string]any{"role": "user", "content": "keep going"}},
		}},
		{"explicit thread id", map[string]any{
			"type":      "continue",
			"thread_id": threadID,
			"messages":  []any{map[string]any{"role": "user", "content": "keep going"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := buildSillyTavernContext(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if ctx.Mode != sillyTavernModeContinue {
				t.Fatalf("precondition failed: mode=%q", ctx.Mode)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if matched, ok := app.resolveSillyTavernContinuation(r, tc.payload, ctx, "st-missing-fingerprint", ""); ok {
				t.Fatalf("deleted conversation was revived: %+v", matched.Target.Conversation)
			}
		})
	}

	// Control: a live conversation with the same session still continues, so the
	// guard is not simply disabling the SillyTavern continue path.
	entry := ConversationEntry{
		ID:           conversationID,
		ThreadID:     threadID,
		AccountEmail: "primary@example.com",
		Status:       "complete",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"type":            "continue",
		"conversation_id": conversationID,
		"messages":        []any{map[string]any{"role": "user", "content": "keep going"}},
	}
	ctx, err := buildSillyTavernContext(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	matched, ok := app.resolveSillyTavernContinuation(r, payload, ctx, "st-missing-fingerprint", "")
	if !ok || strings.TrimSpace(matched.Target.Conversation.ThreadID) != threadID {
		t.Fatalf("live conversation was not continued: ok=%v target=%+v", ok, matched.Target.Conversation)
	}
}

// Resolution can adopt a conversation id from a leftover session row, so the
// deleted-conversation guard has to run on the RESOLVED id. An input-side check
// (the request's thread id, or a response link with no conversation id) misses
// the case where the session is what supplies the dead conversation.
func TestDeletedConversationIsNotRevivedThroughSessionAdoptedIDs(t *testing.T) {
	app := newConversationRequestTestApp(t)
	store := app.State.Store
	if store == nil {
		t.Fatal("test app has no persistence store")
	}
	const conversationID = "adopted-deleted-conversation"
	const threadID = "adopted-deleted-thread"
	now := time.Now()
	if err := store.SaveConversationSession(ConversationSession{
		ID:             "adopted-orphan-session",
		ConversationID: conversationID,
		ThreadID:       threadID,
		AccountEmail:   "primary@example.com",
		Status:         conversationSessionStatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastUsedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadConversation(conversationID); err != nil || found {
		t.Fatalf("precondition failed: conversation row present (found=%v err=%v)", found, err)
	}
	// A continuation link that carries the thread but no conversation id: the
	// session is then the only thing that names the (deleted) conversation.
	app.State.saveResponseWithAccount("adopted-link", map[string]any{"id": "adopted-link"}, "", threadID, "primary@example.com")

	for _, tc := range []struct {
		name             string
		previousResponse string
		explicitThread   string
	}{
		{"explicit thread id", "", threadID},
		{"previous response id", "adopted-link", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if target, ok := app.resolveContinuationConversationWithExplicit(tc.previousResponse, "", "", nil, "", tc.explicitThread); ok {
				t.Fatalf("deleted conversation was revived: %+v", target.Conversation)
			}
		})
	}

	// Control: with the row restored, both paths resolve it again.
	entry := ConversationEntry{
		ID:           conversationID,
		ThreadID:     threadID,
		AccountEmail: "primary@example.com",
		Status:       "complete",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	if target, ok := app.resolveContinuationConversationWithExplicit("", "", "", nil, "", threadID); !ok || target.Conversation.ID != conversationID {
		t.Fatalf("live conversation was not resolved by thread id: ok=%v target=%+v", ok, target.Conversation)
	}
	if target, ok := app.resolveContinuationConversationWithExplicit("adopted-link", "", "", nil, "", ""); !ok || target.Conversation.ID != conversationID {
		t.Fatalf("live conversation was not resolved by response link: ok=%v target=%+v", ok, target.Conversation)
	}
}

// A successful login proves the credential works; it says nothing about the
// workspace's AI allowance. If the refresh loop cleared an active quota
// cooldown, the account would be put back in the pool and re-dispatched into
// the same wall on every refresh interval.
func TestHealthyRefreshKeepsActiveQuotaCooldown(t *testing.T) {
	app := newConversationRequestTestApp(t)
	const email = "primary@example.com"
	const workspaceID = "space-primary"

	if err := app.State.finishWorkspaceDispatchFailure(email, workspaceID, time.Now(), errors.New("upstream AI quota-exhausted"), true); err != nil {
		t.Fatal(err)
	}
	startedCfg, _, _ := app.State.Snapshot()
	startedParent, _, ok := startedCfg.FindAccount(email)
	if !ok {
		t.Fatal("account missing from starting snapshot")
	}
	started, ok := accountForWorkspace(startedParent, workspaceID)
	if !ok {
		t.Fatal("workspace missing from starting snapshot")
	}
	if !strings.EqualFold(started.Status, "quota_exhausted") || started.CooldownUntil == "" {
		t.Fatalf("precondition failed: workspace is not in a quota cooldown (%+v)", started)
	}

	refreshedCfg := healthyRefreshConfigForTest(t, startedCfg, started)
	if _, err := app.State.commitAccountRefresh(startedCfg, started, refreshedCfg); err != nil {
		t.Fatal(err)
	}

	after, _, _ := app.State.Snapshot()
	got, _, ok := after.FindAccountWorkspace(email, workspaceID)
	if !ok {
		t.Fatal("workspace vanished after commit")
	}
	if got.CooldownUntil == "" || !strings.EqualFold(got.Status, "quota_exhausted") {
		t.Fatalf("healthy refresh cleared an active quota cooldown: status=%q cooldown=%q", got.Status, got.CooldownUntil)
	}
	if eligible, _ := accountDispatchEligible(after, got, time.Now()); eligible {
		t.Fatal("workspace became dispatchable despite an active quota cooldown")
	}
}

// The hold must be self-limiting: once the recorded deadline has passed, a
// healthy refresh clears the cooldown as before, so an account can never be
// parked forever by a stale quota state.
func TestHealthyRefreshClearsExpiredQuotaCooldown(t *testing.T) {
	app := newConversationRequestTestApp(t)
	const email = "primary@example.com"
	const workspaceID = "space-primary"

	if err := app.State.finishWorkspaceDispatchFailure(email, workspaceID, time.Now(), errors.New("upstream AI quota-exhausted"), true); err != nil {
		t.Fatal(err)
	}
	// Move the deadline into the past while keeping the recorded status.
	if _, err := app.State.Mutate(func(cfg *AppConfig) error {
		for i := range cfg.Accounts {
			if !strings.EqualFold(strings.TrimSpace(cfg.Accounts[i].Email), email) {
				continue
			}
			for j := range cfg.Accounts[i].Workspaces {
				if cfg.Accounts[i].Workspaces[j].ID == workspaceID {
					cfg.Accounts[i].Workspaces[j].CooldownUntil = formatRFC3339OrEmpty(time.Now().Add(-time.Minute))
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	startedCfg, _, _ := app.State.Snapshot()
	startedParent, _, ok := startedCfg.FindAccount(email)
	if !ok {
		t.Fatal("account missing from starting snapshot")
	}
	started, ok := accountForWorkspace(startedParent, workspaceID)
	if !ok {
		t.Fatal("workspace missing from starting snapshot")
	}
	if !strings.EqualFold(started.Status, "quota_exhausted") {
		t.Fatalf("precondition failed: status is %q", started.Status)
	}
	if activeQuotaCooldown(workspaceFromAccountFields(started), time.Now()) {
		t.Fatal("precondition failed: the cooldown is still active")
	}

	refreshedCfg := healthyRefreshConfigForTest(t, startedCfg, started)
	if _, err := app.State.commitAccountRefresh(startedCfg, started, refreshedCfg); err != nil {
		t.Fatal(err)
	}

	after, _, _ := app.State.Snapshot()
	got, _, ok := after.FindAccountWorkspace(email, workspaceID)
	if !ok {
		t.Fatal("workspace vanished after commit")
	}
	if got.CooldownUntil != "" || !strings.EqualFold(got.Status, "ready") {
		t.Fatalf("expired quota cooldown was not cleared: status=%q cooldown=%q", got.Status, got.CooldownUntil)
	}
}
