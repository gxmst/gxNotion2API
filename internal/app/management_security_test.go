package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminConfigExportRedactsSecrets(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "api-secret"
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-secret"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	state.mu.Lock()
	state.AdminTokens["valid-token"] = time.Now().Add(time.Hour)
	state.mu.Unlock()
	app := &App{State: state}
	req := httptest.NewRequest(http.MethodGet, "/admin/config/export", nil)
	req.Header.Set("X-Admin-Token", "valid-token")
	rec := httptest.NewRecorder()
	app.handleAdminConfigExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	exported := mapValue(payload["config"])
	if _, exists := exported["api_key"]; exists {
		t.Fatalf("api key leaked in export: %s", rec.Body.String())
	}
	admin := mapValue(exported["admin"])
	if _, exists := admin["password"]; exists {
		t.Fatalf("admin password leaked in export: %s", rec.Body.String())
	}
}

func TestImportOfRedactedExportPreservesCurrentSecrets(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "api-secret"
	cfg.Admin.Password = "admin-secret"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	body, err := json.Marshal(map[string]any{"config": configExportPayload(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/config/import", strings.NewReader(string(body)))
	patch, err := app.readConfigPatch(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatal(err)
	}
	current, _, _ := state.Snapshot()
	merged, err := mergeConfigPatch(current, patch, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.APIKey != "api-secret" || merged.Admin.Password != "admin-secret" {
		t.Fatalf("redacted import changed secrets: api=%q admin=%q", merged.APIKey, merged.Admin.Password)
	}
}

func TestNormalizeMetricsPathLabelBoundsUnknownPaths(t *testing.T) {
	for _, path := range []string{"/foo", "/bar123", "/random/a/b/c"} {
		if got := normalizeMetricsPathLabel(path); got != "/other" {
			t.Fatalf("path %q label = %q, want /other", path, got)
		}
	}
}

func TestAdminResponsesDoNotAdvertiseWildcardCORS(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "api-secret"
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-secret"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	rec := httptest.NewRecorder()
	(&App{State: state}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/verify", nil))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("admin response advertised wildcard CORS: %q", got)
	}
}

func TestInlineAttachmentSupportsBase64URLAndEnforcesLimit(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xef})
	decoded, _, err := decodeInlineAttachment(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 || decoded[0] != 0xfb {
		t.Fatalf("decoded = %v", decoded)
	}
	oversized := base64.RawStdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1))
	_, _, err = buildAttachmentFromInlineData(oversized, "large.bin", "application/octet-stream")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected attachment size error, got %v", err)
	}
}

// A settings save must neither write into the live snapshot nor replace the
// runtime state dispatch keeps on accounts.
func TestConfigPatchDoesNotAliasLiveConfigOrTouchAccounts(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "api-secret"
	cfg.Admin.TrustedProxies = []string{"10.0.0.1"}
	cfg.ModelAliases = map[string]string{"old": "auto"}
	cfg.Accounts = []NotionAccount{
		{Email: "a@example.com", CredentialCooldownUntil: "2099-01-01T00:00:00Z", ConsecutiveFailures: 3},
		{Email: "b@example.com"},
	}
	live := normalizeConfig(cfg)
	patch := map[string]any{
		"admin":         map[string]any{"trusted_proxies": []any{"10.0.0.2"}},
		"model_aliases": map[string]any{"new": "auto"},
		"accounts":      []any{map[string]any{"email": "b@example.com"}, map[string]any{"email": "a@example.com"}},
	}
	merged, err := mergeConfigPatch(live, patch, false)
	if err != nil {
		t.Fatal(err)
	}
	if live.Admin.TrustedProxies[0] != "10.0.0.1" || live.ModelAliases["new"] != "" {
		t.Fatalf("patch wrote into the live config: %+v %+v", live.Admin.TrustedProxies, live.ModelAliases)
	}
	if merged.Admin.TrustedProxies[0] != "10.0.0.2" || merged.ModelAliases["old"] != "" || merged.ModelAliases["new"] != "auto" {
		t.Fatalf("patch not applied as a replacement: %+v %+v", merged.Admin.TrustedProxies, merged.ModelAliases)
	}
	first, _, _ := merged.FindAccount("a@example.com")
	if len(merged.Accounts) != 2 || merged.Accounts[0].Email != "a@example.com" || first.ConsecutiveFailures != 3 || first.CredentialCooldownUntil == "" {
		t.Fatalf("settings save changed accounts: %+v", merged.Accounts)
	}
	// Import may replace accounts, but decodes them fresh rather than on top of
	// whichever account used to sit at the same index.
	imported, err := mergeConfigPatch(live, patch, true)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Accounts[0].Email != "b@example.com" || imported.Accounts[0].ConsecutiveFailures != 0 || imported.Accounts[0].CredentialCooldownUntil != "" {
		t.Fatalf("import leaked another account's state: %+v", imported.Accounts[0])
	}
}

func TestConfigPatchRejectsForeignUpstream(t *testing.T) {
	live := normalizeConfig(defaultConfig())
	for _, target := range []string{"https://evil.example", "http://www.notion.so", "https://notion.so.evil.example"} {
		if _, err := mergeConfigPatch(live, map[string]any{"upstream_base_url": target}, false); err == nil {
			t.Fatalf("upstream %q accepted", target)
		}
	}
	for _, target := range []string{"https://www.notion.so", "https://app.notion.com", "http://127.0.0.1:9999"} {
		if _, err := mergeConfigPatch(live, map[string]any{"upstream_base_url": target}, false); err != nil {
			t.Fatalf("upstream %q rejected: %v", target, err)
		}
	}
}

func TestProxyCredentialsRedactedAndRestoredOnSave(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProxyURL = "socks5://user:hunter2@127.0.0.1:1080"
	cfg.Accounts = []NotionAccount{{Email: "a@example.com", ProxyURL: "http://u:p4ss@10.0.0.1:8080"}}
	live := normalizeConfig(cfg)
	redacted := redactConfigSecrets(live)
	body, _ := json.Marshal(redacted)
	if strings.Contains(string(body), "hunter2") || strings.Contains(string(body), "p4ss") {
		t.Fatalf("proxy password leaked: %s", body)
	}
	if live.Accounts[0].ProxyURL != "http://u:p4ss@10.0.0.1:8080" {
		t.Fatal("redaction modified the live accounts")
	}
	var patch map[string]any
	_ = json.Unmarshal(body, &patch)
	merged, err := mergeConfigPatch(live, patch, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.ProxyURL != live.ProxyURL || merged.Accounts[0].ProxyURL != live.Accounts[0].ProxyURL {
		t.Fatalf("round trip lost credentials: %q %q", merged.ProxyURL, merged.Accounts[0].ProxyURL)
	}
}

func TestAdminRejectsForgedCrossSiteWrites(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "api-secret"
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-secret"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	state.mu.Lock()
	state.AdminTokens["valid-token"] = time.Now().Add(time.Hour)
	state.mu.Unlock()
	app := &App{State: state}
	send := func(contentType, fetchSite string) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/config/import", strings.NewReader(`{"upstream_base_url":"https://www.notion.so"}`))
		req.AddCookie(&http.Cookie{Name: "notion2api_admin", Value: "valid-token"})
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("text/plain", ""); code != http.StatusForbidden {
		t.Fatalf("text/plain cookie POST = %d, want 403", code)
	}
	if code := send("application/json", "same-site"); code != http.StatusForbidden {
		t.Fatalf("same-site POST = %d, want 403", code)
	}
	if code := send("application/json; charset=utf-8", "same-origin"); code != http.StatusOK {
		t.Fatalf("console POST = %d, want 200", code)
	}
}

func TestAdminLoginReservesAttemptsBeforeCheckingPassword(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "api-secret"
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-secret"
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	allowed := 0
	for i := 0; i < adminLoginMaxFailures*3; i++ {
		if _, locked := app.reserveAdminLoginAttempt("203.0.113.9"); !locked {
			allowed++
		}
	}
	if allowed != adminLoginMaxFailures {
		t.Fatalf("reserved %d attempts before lockout, want %d", allowed, adminLoginMaxFailures)
	}
}

func TestPlaceholderSecretsRejected(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "change-me-openai-key"
	if err := validateConfiguredAPIKey(normalizeConfig(cfg)); err == nil {
		t.Fatal("placeholder api key accepted")
	}
	cfg.APIKey = "real"
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "change-me-admin-password"
	if err := validateConfiguredAPIKey(normalizeConfig(cfg)); err == nil {
		t.Fatal("placeholder admin password accepted")
	}
}
