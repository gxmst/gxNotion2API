package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Files holding Notion cookies or the service's own credentials must not be
// world-readable. os.WriteFile only applies its mode when creating a file, so
// the helper also has to chmod: a probe.json that already exists at 0644 would
// otherwise keep those bits forever.

func TestWritePrivatePrettyJSONFileUsesRestrictiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes are not meaningful on windows")
	}
	path := filepath.Join(t.TempDir(), "probe.json")
	if err := writePrivatePrettyJSONFile(path, map[string]any{"cookies": []string{"token_v2"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestWritePrivatePrettyJSONFileTightensExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes are not meaningful on windows")
	}
	path := filepath.Join(t.TempDir(), "probe.json")
	// Simulate a file left behind by an older build that wrote 0644.
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writePrivatePrettyJSONFile(path, map[string]any{"cookies": []string{"token_v2"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600 (an existing 0644 file must be tightened)", got)
	}
}

func TestRedactConfigSecretsClearsBothSecrets(t *testing.T) {
	cfg := AppConfig{APIKey: "sk-n2a-secret"}
	cfg.Admin.Password = "admin-secret"

	safe := redactConfigSecrets(cfg)
	if safe.APIKey != "" {
		t.Errorf("APIKey survived redaction: %q", safe.APIKey)
	}
	if safe.Admin.Password != "" {
		t.Errorf("Admin.Password survived redaction: %q", safe.Admin.Password)
	}
	// The caller's copy must be untouched, since it is the live config.
	if cfg.APIKey == "" || cfg.Admin.Password == "" {
		t.Error("redaction mutated the source config")
	}
}

// A config snapshot lands on disk and stays there, so it must be redacted and
// mode 0600. This asserts the shape a snapshot is written with, mirroring
// handleAdminConfigSnapshot's POST branch.
func TestConfigSnapshotPayloadCarriesNoSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes are not meaningful on windows")
	}
	cfg := AppConfig{APIKey: "sk-n2a-secret"}
	cfg.Admin.Password = "admin-secret"

	exported := normalizeConfig(cfg)
	exported.ConfigPath = ""
	exported = redactConfigSecrets(exported)

	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := writePrivatePrettyJSONFile(path, exported); err != nil {
		t.Fatalf("write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("snapshot mode = %o, want 600", got)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, _ := parsed["api_key"].(string); v != "" {
		t.Errorf("snapshot leaked api_key: %q", v)
	}
	if admin, ok := parsed["admin"].(map[string]any); ok {
		if v, _ := admin["password"].(string); v != "" {
			t.Errorf("snapshot leaked admin.password: %q", v)
		}
	}
}
