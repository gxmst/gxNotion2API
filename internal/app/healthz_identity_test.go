package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// /healthz is answered before authOK so a liveness probe needs no credentials.
// That makes it the wrong place for account identity: exposing the panel over a
// domain would otherwise publish the Notion account email to anyone scanning.

func healthzTestApp(t *testing.T) *App {
	t.Helper()
	cfg := AppConfig{APIKey: "test-api-key", ActiveAccount: "operator@example.com"}
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-pw"
	state := &ServerState{
		Config:      cfg,
		Session:     SessionInfo{UserEmail: "operator@example.com", SpaceID: "space-42"},
		AdminTokens: map[string]time.Time{},
	}
	state.snap.Store(&snapshotBundle{Config: cfg, Session: state.Session})
	return &App{State: state}
}

func healthzBody(t *testing.T, app *App, headers map[string]string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	app.serveHealthz(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return payload
}

func assertNoIdentity(t *testing.T, payload map[string]any, context string) {
	t.Helper()
	for _, field := range []string{"user_email", "space_id", "active_account"} {
		if v, present := payload[field]; present {
			t.Errorf("%s: %s leaked to an unauthenticated caller: %#v", context, field, v)
		}
	}
}

func TestHealthzHidesIdentityFromAnonymousCaller(t *testing.T) {
	payload := healthzBody(t, healthzTestApp(t), nil)
	assertNoIdentity(t, payload, "anonymous")
	// Liveness information must still be there, or a probe cannot use it.
	if ok, _ := payload["ok"].(bool); !ok {
		t.Error("ok missing for anonymous caller")
	}
	if _, present := payload["session_ready"]; !present {
		t.Error("session_ready missing for anonymous caller")
	}
}

func TestHealthzHidesIdentityFromWrongAPIKey(t *testing.T) {
	payload := healthzBody(t, healthzTestApp(t), map[string]string{
		"Authorization": "Bearer not-the-key",
	})
	assertNoIdentity(t, payload, "wrong api key")
}

func TestHealthzHidesIdentityFromStaleAdminCookie(t *testing.T) {
	payload := healthzBody(t, healthzTestApp(t), map[string]string{
		"X-Admin-Token": "never-issued",
	})
	assertNoIdentity(t, payload, "forged admin token")
}

func TestHealthzShowsIdentityToAPIKeyHolder(t *testing.T) {
	payload := healthzBody(t, healthzTestApp(t), map[string]string{
		"Authorization": "Bearer test-api-key",
	})
	if got, _ := payload["user_email"].(string); got != "operator@example.com" {
		t.Errorf("user_email = %q, want the account address", got)
	}
	if got, _ := payload["space_id"].(string); got != "space-42" {
		t.Errorf("space_id = %q", got)
	}
	if got, _ := payload["active_account"].(string); got != "operator@example.com" {
		t.Errorf("active_account = %q", got)
	}
}

func TestHealthzShowsIdentityToAdminSession(t *testing.T) {
	app := healthzTestApp(t)
	// Mint a real token the way /admin/login does.
	token := app.issueAdminToken()
	payload := healthzBody(t, app, map[string]string{"X-Admin-Token": token})
	if got, _ := payload["user_email"].(string); got != "operator@example.com" {
		t.Errorf("admin session did not receive identity: user_email = %q", got)
	}
}

// The static cache is shared across callers, so identity must never enter it --
// otherwise one authenticated request would poison the anonymous response.
func TestHealthzStaticCacheCarriesNoIdentity(t *testing.T) {
	app := healthzTestApp(t)
	app.State.mu.Lock()
	app.State.rebuildStaticJSONCachesLocked()
	app.State.mu.Unlock()

	cached := app.State.cachedHealthzStaticJSON.Load()
	if cached == nil {
		t.Fatal("static cache was not built")
	}
	var payload map[string]any
	if err := json.Unmarshal(*cached, &payload); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}
	assertNoIdentity(t, payload, "static cache")

	// And an anonymous request served from that cache stays clean even after an
	// operator request has been served.
	_ = healthzBody(t, app, map[string]string{"Authorization": "Bearer test-api-key"})
	assertNoIdentity(t, healthzBody(t, app, nil), "anonymous after operator request")
}

func TestHealthzAnonymousStillReportsRuntimeFields(t *testing.T) {
	app := healthzTestApp(t)
	app.State.mu.Lock()
	app.State.rebuildStaticJSONCachesLocked()
	app.State.LastSessionRefresh = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	app.State.LastSessionRefreshError = "refresh failed"
	app.State.mu.Unlock()

	payload := healthzBody(t, app, nil)
	if got, _ := payload["last_session_refresh"].(string); got != "2026-01-02T03:04:05Z" {
		t.Errorf("last_session_refresh = %q", got)
	}
	if got, _ := payload["last_session_refresh_error"].(string); got != "refresh failed" {
		t.Errorf("last_session_refresh_error = %q", got)
	}
	assertNoIdentity(t, payload, "cached runtime path")
}
