package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Synthetic identifiers with the structure captured in the September HAR.
const manualModelFixture = `{"modelSelectionRestricted":false,"models":[{"model":"test-model","modelMessage":"Test Model","modelFamily":"provider","modelConfiguration":{"defaultReasoningEffort":"medium","supportedReasoningEfforts":["low","medium","high"]},"workflow":{"finalModelName":"test-codename"}},{"model":"blocked","modelMessage":"Blocked Model","workflow":{"finalModelName":"blocked","isDisabled":true,"disabledReason":"trial_not_allowed"}}]}`
const autoModelFixture = `{"modelSelectionRestricted":true,"models":[]}`

func TestModelCapabilitiesDistinguishRestrictionAndUnknown(t *testing.T) {
	capability, err := parseWorkspaceModelCapabilities([]byte(autoModelFixture))
	if err != nil || capability.Mode != "auto_only" || len(capability.Models) != 0 {
		t.Fatalf("restricted empty list: %+v %v", capability, err)
	}
	for _, raw := range []string{`{}`, `{"models":[]}`, `{"modelSelectionRestricted":false}`, `{"modelSelectionRestricted":false,"models":null}`} {
		if _, err := parseWorkspaceModelCapabilities([]byte(raw)); err == nil {
			t.Fatalf("incomplete response was accepted: %s", raw)
		}
	}
	capability, err = parseWorkspaceModelCapabilities([]byte(manualModelFixture))
	if err != nil {
		t.Fatal(err)
	}
	if capability.Mode != "manual" || len(capability.Models) != 2 || capability.Models[1].Enabled || capability.Models[1].DisabledReason != "trial_not_allowed" {
		t.Fatalf("workflow disabled flag lost: %+v", capability)
	}
	if capability.Models[0].DefaultReasoningEffort != "medium" || len(capability.Models[0].SupportedReasoningEfforts) != 3 {
		t.Fatal("reasoning catalog metadata lost")
	}
}

func TestRefreshModelCapabilitiesUsesSelectedWorkspaceAndSeparatesCatalog(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].Workspaces = append(cfg.Accounts[0].Workspaces, NotionWorkspace{ID: "selected-space", PlanType: "business"})
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v3/getAvailableModels" {
			t.Errorf("unexpected upstream call %s", r.URL.Path)
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["spaceId"] != "selected-space" {
			t.Errorf("used probe workspace: %+v", payload)
		}
		if calls == 1 && payload["surface"] != nil {
			t.Error("chat discovery used settings surface")
		}
		if calls == 2 && payload["surface"] != "workspace_model_settings" {
			t.Error("catalog did not use settings surface")
		}
		if calls == 1 {
			_, _ = w.Write([]byte(autoModelFixture))
		} else {
			_, _ = w.Write([]byte(manualModelFixture))
		}
	}))
	defer server.Close()
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	before, _, _ := app.State.Snapshot()
	rec := postRefreshModels(t, app, "test-admin-token", map[string]any{"email": "primary@example.com", "workspace_id": "selected-space"})
	if rec.Code != 200 || calls != 2 {
		t.Fatalf("refresh status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	live, _, registry := app.State.Snapshot()
	selected, _, _ := live.FindAccountWorkspace("primary@example.com", "selected-space")
	workspace, _ := accountWorkspace(selected, "selected-space")
	if workspace.ModelCapabilities.Mode != "auto_only" || len(workspace.ModelCapabilities.Catalog) != 2 {
		t.Fatalf("settings response changed chat capability: %+v", workspace)
	}
	original, _, _ := before.FindAccountWorkspace("primary@example.com", "selected-space")
	oldWorkspace, _ := accountWorkspace(original, "selected-space")
	if oldWorkspace.ModelCapabilities != nil {
		t.Fatal("refresh mutated previous snapshot")
	}
	if len(live.Models) != len(before.Models) {
		t.Fatal("refresh changed global configured models")
	}
	if _, err := selectWorkspaceModel(live, selected, PromptRunRequest{PublicModel: "test-model", NotionModel: "test-codename"}); err == nil {
		t.Fatal("catalog granted manual selection")
	}
	if _, err := registry.Resolve("test-model", ""); err != nil {
		t.Fatal("catalog name unavailable for alias resolution")
	}
	rec = postRefreshModels(t, app, "test-admin-token", map[string]any{"email": "primary@example.com", "workspace_id": "missing"})
	if rec.Code != 404 || calls != 2 {
		t.Fatal("invalid workspace silently fell back")
	}
}

func TestWorkspaceModelRoutingAndExplicitAutoFallback(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	manual, _ := parseWorkspaceModelCapabilities([]byte(manualModelFixture))
	auto, _ := parseWorkspaceModelCapabilities([]byte(autoModelFixture))
	for i := range cfg.Accounts {
		cfg.Accounts[i].Workspaces[0].ModelCapabilities = auto
	}
	cfg.Accounts[1].Workspaces[0].ModelCapabilities = manual
	request := PromptRunRequest{PublicModel: "test-model", NotionModel: "old-codename"}
	candidates, err := resolveDispatchCandidates(cfg, request, time.Now())
	if err != nil || len(candidates) != 1 || candidates[0].Email != "backup@example.com" {
		t.Fatalf("manual request routed incorrectly: %+v %v", candidates, err)
	}
	resolved, err := selectWorkspaceModel(cfg, candidates[0], request)
	if err != nil || resolved.NotionModel != "test-codename" || resolved.ModelSelectionMode != "manual" {
		t.Fatal("workspace model mapping not applied")
	}
	request.PinnedAccountEmail, request.PinnedSpaceID = "primary@example.com", "space-primary"
	if _, err := resolveDispatchCandidates(cfg, request, time.Now()); !isModelSelectionError(err) {
		t.Fatalf("pinned restricted request escaped owner: %v", err)
	}
	cfg.Dispatch.RestrictedModelFallback = true
	unpinned := request
	unpinned.PinnedAccountEmail, unpinned.PinnedSpaceID = "", ""
	preferred, preferredErr := resolveDispatchCandidates(cfg, unpinned, time.Now())
	if preferredErr != nil || len(preferred) != 2 || preferred[0].Email != "backup@example.com" {
		t.Fatal("Auto fallback displaced a workspace supporting the requested model")
	}
	candidates, err = resolveDispatchCandidates(cfg, request, time.Now())
	if err != nil || len(candidates) != 1 {
		t.Fatal(err)
	}
	resolved, err = selectWorkspaceModel(cfg, candidates[0], request)
	if err != nil || resolved.NotionModel != "" || resolved.PublicModel != request.PublicModel || resolved.ModelSelectionMode != "auto_fallback" {
		t.Fatal("fallback lost requested identity or did not use Auto")
	}
	cfg.Accounts[0].Workspaces[0].ModelCapabilities = nil
	candidates, err = resolveDispatchCandidates(cfg, request, time.Now())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("unknown capability with restricted fallback enabled must fall back to Auto: %v", err)
	}
	if resolved, err = selectWorkspaceModel(cfg, candidates[0], request); err != nil || resolved.NotionModel != "" || resolved.ModelSelectionMode != "auto_fallback" {
		t.Fatalf("unknown capability did not fall back to Auto: %+v %v", resolved, err)
	}
	cfg.Dispatch.RestrictedModelFallback = false
	if _, err := resolveDispatchCandidates(cfg, request, time.Now()); !isModelSelectionError(err) {
		t.Fatal("unknown capability without fallback must be rejected")
	}
	request.NotionModel = ""
	if _, err := resolveDispatchCandidates(cfg, request, time.Now()); err != nil {
		t.Fatal("unknown workspace cannot use Auto", err)
	}
}

func TestWorkspaceCatalogDoesNotReenableLocallyDisabledModel(t *testing.T) {
	cfg := defaultConfig()
	capability, _ := parseWorkspaceModelCapabilities([]byte(manualModelFixture))
	cfg.Accounts = []NotionAccount{{Email: "local@example.com", Workspaces: []NotionWorkspace{{ID: "space", PlanType: "business", ModelCapabilities: capability}}}}
	cfg.Models = []ModelDefinition{{ID: "test-model", NotionModel: "test-codename", Enabled: false}}
	registry := buildModelRegistry(cfg)
	if _, err := registry.Resolve("test-model", ""); err == nil {
		t.Fatal("workspace catalog re-enabled a local disabled model")
	}
	for _, model := range buildPublicModelsListPayload(publicWorkspaceModelRegistry(cfg, registry)).Data {
		if model.ID == "test-model" {
			t.Fatal("disabled model was advertised")
		}
	}
}

func TestModelCapabilityPersistsAndSurvivesSessionRefresh(t *testing.T) {
	app := newConversationRequestTestApp(t)
	startedCfg, _, _ := app.State.Snapshot()
	started, _, _ := startedCfg.FindAccount("primary@example.com")
	capability, _ := parseWorkspaceModelCapabilities([]byte(autoModelFixture))
	if err := app.State.applyWorkspaceModelCapabilities(started, started.SpaceID, capability); err != nil {
		t.Fatal(err)
	}
	if _, err := app.State.commitAccountRefresh(startedCfg, started, startedCfg); err != nil {
		t.Fatal(err)
	}
	live, _, _ := app.State.Snapshot()
	account, _, _ := live.FindAccount(started.Email)
	workspace, _ := accountWorkspace(account, started.SpaceID)
	if workspace.ModelCapabilities.Mode != "auto_only" {
		t.Fatal("login refresh restored old model permission")
	}
	app.State.sqliteWriter.Close()
	accounts, _, _, _, err := app.State.Store.LoadAccountsWithWorkspace()
	if err != nil || accounts[0].Workspaces[0].ModelCapabilities.Mode != "auto_only" {
		t.Fatal("capabilities did not persist", err)
	}
}

func TestModelSelectionErrorsDoNotConsumeQuotaOrCallUpstream(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	for i := range cfg.Accounts {
		cfg.Accounts[i].Workspaces[0].ModelCapabilities = nil
	}
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	app.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
		t.Fatal("unsupported selection dispatched")
		return InferenceResult{}, nil
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, conversationTestHTTPRequest(t, path, map[string]any{"model": "gpt-5.4", "input": "hi", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_selection_unavailable") {
			t.Fatalf("wrong error: %d %s", rec.Code, rec.Body.String())
		}
	}
	live, _, _ := app.State.Snapshot()
	if live.Accounts[0].WindowRequestCount != 0 || live.Accounts[0].TotalFailures != 0 {
		t.Fatal("local capability rejection charged account")
	}
	list := httptest.NewRecorder()
	app.serveModels(list)
	if !strings.Contains(list.Body.String(), `"id":"auto"`) || strings.Contains(list.Body.String(), "gpt-5.4") {
		t.Fatalf("unknown workspaces advertised manual models: %s", list.Body.String())
	}
}
