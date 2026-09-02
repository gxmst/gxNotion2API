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
	merged, err := app.mergeConfigFromBody(req)
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
