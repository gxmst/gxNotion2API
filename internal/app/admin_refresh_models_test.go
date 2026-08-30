package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newRefreshModelsTestApp(t *testing.T, configure func(*AppConfig)) (*App, string) {
	t.Helper()
	cfg := defaultConfig()
	cfg.APIKey = "test-api-key"
	cfg.Storage.SQLitePath = ""
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-pw"
	if configure != nil {
		configure(&cfg)
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatalf("newServerState failed: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })

	token := "test-admin-token"
	state.mu.Lock()
	if state.AdminTokens == nil {
		state.AdminTokens = map[string]time.Time{}
	}
	state.AdminTokens[token] = time.Now().Add(time.Hour)
	state.mu.Unlock()
	return &App{State: state}, token
}

func postRefreshModels(t *testing.T, app *App, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/refresh-models", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	rec := httptest.NewRecorder()
	app.handleAdminAccountsRefreshModels(rec, req)
	return rec
}

func TestRefreshModelsRequiresAuth(t *testing.T) {
	app, _ := newRefreshModelsTestApp(t, nil)
	rec := postRefreshModels(t, app, "", map[string]any{"email": "a@b.c"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRefreshModelsRejectsWrongMethod(t *testing.T) {
	app, token := newRefreshModelsTestApp(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts/refresh-models", nil)
	req.Header.Set("X-Admin-Token", token)
	rec := httptest.NewRecorder()
	app.handleAdminAccountsRefreshModels(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestRefreshModelsUnknownAccount(t *testing.T) {
	app, token := newRefreshModelsTestApp(t, nil)
	rec := postRefreshModels(t, app, token, map[string]any{"email": "nobody@example.com"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestRefreshModelsWithoutEmailOrActiveAccount(t *testing.T) {
	app, token := newRefreshModelsTestApp(t, nil)
	rec := postRefreshModels(t, app, token, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestRefreshModelsMissingProbeFile(t *testing.T) {
	app, token := newRefreshModelsTestApp(t, func(cfg *AppConfig) {
		cfg.Accounts = []NotionAccount{{
			Email:     "user@example.com",
			ProbeJSON: filepath.Join(t.TempDir(), "does-not-exist.json"),
		}}
	})
	rec := postRefreshModels(t, app, token, map[string]any{"email": "user@example.com"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// refreshAccountModels must refuse to call upstream without a space_id, which is
// the parameter getAvailableModels requires.
func TestRefreshAccountModelsRequiresSpaceID(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	cookies := []ProbeCookie{{Name: "token_v2", Value: "x"}}
	_, err := refreshAccountModels(t.Context(), cfg, "a@b.c", cookies, "client-1", "user-1", "   ")
	if err == nil {
		t.Fatal("expected an error when space_id is blank")
	}
	if got := err.Error(); got != "space_id is required to refresh models" {
		t.Errorf("err = %q", got)
	}
}

func TestRefreshAccountModelsRequiresCookies(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	_, err := refreshAccountModels(t.Context(), cfg, "a@b.c", nil, "client-1", "user-1", "space-1")
	if err == nil {
		t.Fatal("expected an error when cookies are missing")
	}
	if got := err.Error(); got != "cookies are required to refresh models" {
		t.Errorf("err = %q", got)
	}
}

// The refresh path must let freshly discovered definitions win, so a model whose
// upstream codename changed is corrected rather than shadowed by config. This is
// the opposite precedence from import-time discovery.
func TestRefreshMergePrecedenceFavoursDiscovered(t *testing.T) {
	configured := []ModelDefinition{
		{ID: "opus-4.7", Name: "Opus 4.7", NotionModel: "stale-codename", Enabled: true},
	}
	discovered := []ModelDefinition{
		{ID: "opus-4.7", Name: "Opus 4.7", NotionModel: "fresh-codename", Enabled: true},
		{ID: "brand-new", Name: "Brand New", NotionModel: "new-codename", Enabled: true},
	}

	// What the refresh handler does.
	refreshed := mergeModelDefinitions(configured, discovered)
	byID := map[string]ModelDefinition{}
	for _, entry := range refreshed {
		byID[entry.ID] = entry
	}
	if got := byID["opus-4.7"].NotionModel; got != "fresh-codename" {
		t.Errorf("refresh: notion_model = %q, want fresh-codename", got)
	}
	if _, ok := byID["brand-new"]; !ok {
		t.Error("refresh: newly discovered model missing")
	}

	// What import-time discovery does, for contrast: config wins there.
	imported := mergeModelDefinitions(discovered, configured)
	for _, entry := range imported {
		if entry.ID == "opus-4.7" && entry.NotionModel != "stale-codename" {
			t.Errorf("import: expected config to win, got %q", entry.NotionModel)
		}
	}
}
