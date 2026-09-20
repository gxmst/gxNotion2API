package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommercialWorkspaceEntitlements(t *testing.T) {
	for _, tc := range []struct {
		name, tier, plan string
		disabled, want   bool
	}{
		{name: "business trial", tier: "business", plan: "trial", want: true},
		{name: "trial with business entitlement", tier: "trial", plan: "business", want: true},
		{name: "explicit trial variant", tier: "business_trial", want: true},
		{name: "enterprise", tier: "Enterprise", plan: "team", want: true},
		{name: "legacy business", plan: "business", want: true},
		{name: "free", tier: "free", plan: "business"},
		{name: "plus", tier: "plus", plan: "business"},
		{name: "generic trial", tier: "trial", plan: "team"},
		{name: "unknown team", plan: "team"},
		{name: "unknown personal", plan: "personal"},
		{name: "AI explicitly disabled", tier: "business", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := workspaceEligibility(NotionWorkspace{SubscriptionTier: tc.tier, PlanType: tc.plan, AIEnabled: true, AIDisabled: tc.disabled})
			if got != tc.want || reason == "" {
				t.Fatalf("eligibility = %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestDispatchOnlyAdmitsCommercialWorkspaces(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts[:1])
	cfg.Accounts[0].Workspaces = []NotionWorkspace{
		{ID: "free", SubscriptionTier: "free", Priority: 99},
		{ID: "plus", SubscriptionTier: "plus", Priority: 98},
		{ID: "unknown", PlanType: "team"},
		{ID: "business-trial", SubscriptionTier: "business", PlanType: "trial"},
	}
	cfg.Accounts[0].DefaultWorkspaceID = "free"
	cfg.Accounts[0].SpaceID = "free"
	cfg.ActiveWorkspaceID = "free"
	cfg = normalizeConfig(cfg)
	candidates, err := resolveDispatchCandidates(cfg, PromptRunRequest{}, time.Now())
	if err != nil || len(candidates) != 1 || accountWorkspaceID(candidates[0]) != "business-trial" {
		t.Fatalf("automatic dispatch = %+v, err=%v", candidates, err)
	}
	for _, workspace := range []string{"free", "plus", "unknown"} {
		for _, email := range []string{"", "primary@example.com"} {
			if candidates, err := resolveDispatchCandidates(cfg, PromptRunRequest{WorkspaceID: workspace, PinnedAccountEmail: email}, time.Now()); err == nil || len(candidates) != 0 {
				t.Fatalf("excluded workspace %s/%s became dispatchable", email, workspace)
			}
		}
	}
}

func TestWorkspaceRefreshRevokesStaleEntitlementsAndPreservesRuntime(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	account := &cfg.Accounts[0]
	account.Workspaces = []NotionWorkspace{
		{ID: "space-primary", SubscriptionTier: "business", TotalSuccesses: 7, HourlyQuota: 12},
		{ID: "removed", SubscriptionTier: "business"},
	}
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	before, _, _ := app.State.Snapshot()
	started, _, _ := before.FindAccount("primary@example.com")
	if err := app.State.applyWorkspaceDiscovery(started, []discoveredSpaceCandidate{
		{ID: "space-primary", SubscriptionTier: "free", PlanType: "personal", AIEnabled: true},
		{ID: "trial", SubscriptionTier: "business", PlanType: "trial", AIEnabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := app.State.Snapshot()
	current, _, _ := after.FindAccount(started.Email)
	prior, _ := accountWorkspace(started, "space-primary")
	free, _ := accountWorkspace(current, "space-primary")
	removed, _ := accountWorkspace(current, "removed")
	if prior.SubscriptionTier != "business" || free.SubscriptionTier != "free" || free.TotalSuccesses != 7 || free.HourlyQuota != 12 {
		t.Fatalf("refresh changed old snapshot or runtime: before=%+v after=%+v", prior, free)
	}
	if eligible, _ := workspaceEligibility(removed); eligible {
		t.Fatal("removed workspace retained its entitlement")
	}
	if current.DefaultWorkspaceID != "trial" || after.ActiveWorkspaceID != "trial" {
		t.Fatal("default target did not move to the eligible trial")
	}
	if current.ProbeJSON != started.ProbeJSON || current.UserID != started.UserID || current.StickyProxyAccount != started.StickyProxyAccount {
		t.Fatal("metadata refresh changed credentials")
	}
	if err := app.State.applyWorkspaceDiscovery(current, []discoveredSpaceCandidate{{ID: "trial", AIEnabled: true}}); err != nil {
		t.Fatal(err)
	}
	unknownConfig, _, _ := app.State.Snapshot()
	unknown, _, _ := unknownConfig.FindAccountWorkspace(started.Email, "trial")
	if eligible, _ := accountWorkspaceEligibility(unknown); eligible {
		t.Fatal("unknown refreshed tier inherited a stale account-level Business entitlement")
	}
}

func TestWorkspaceDiscoverySelectsAuthenticatedUser(t *testing.T) {
	records := spaceRecordMap("current", []discoveredSpaceCandidate{{ID: "trial", SubscriptionTier: "business"}})
	records["notion_user"].(map[string]any)["other"] = map[string]any{"value": map[string]any{"email": "other@example.com"}}
	payload := map[string]any{"recordMap": records}
	if meta := parseLoadUserContentMetadataForUser(payload, "current"); meta.UserID != "current" || meta.SpaceID != "trial" {
		t.Fatalf("wrong user selected: %+v", meta)
	}
	if meta := parseLoadUserContentMetadataForUser(payload, "missing"); meta.UserID != "" {
		t.Fatal("missing authenticated user was replaced by another user")
	}
}

func TestAdminWorkspaceRefreshUsesMetadataOnlyAndRequiresAuth(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	account, _, _ := cfg.FindAccount("primary@example.com")
	session, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v3/loadUserContent" {
			t.Errorf("metadata refresh made an unexpected upstream request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": spaceRecordMap(session.UserID, []discoveredSpaceCandidate{
			{ID: "space-primary", PlanType: "trial", SubscriptionTier: "business"},
			{ID: "personal", PlanType: "personal", SubscriptionTier: "free"},
		})})
	}))
	defer server.Close()
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/refresh-workspaces", nil))
	if rec.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthorized metadata refresh was accepted: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/admin/accounts/refresh-workspaces", map[string]any{"email": account.Email}))
	if rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("metadata refresh failed: %d %s", rec.Code, rec.Body.String())
	}
	live, _, _ := app.State.Snapshot()
	trial, _, _ := live.FindAccountWorkspace(account.Email, "space-primary")
	if eligible, _ := accountWorkspaceEligibility(trial); !eligible {
		t.Fatal("discovered Business trial did not become eligible")
	}
}
