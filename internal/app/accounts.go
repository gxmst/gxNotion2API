package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	accountPathSlugPattern     = regexp.MustCompile(`[^a-z0-9]+`)
	windowsAbsolutePathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/].*`)
)

type ResolvedLoginHelper struct {
	SessionsDir string `json:"sessions_dir"`
	TimeoutSec  int    `json:"timeout_sec"`
}

type ResolvedSessionRefresh struct {
	Enabled          bool `json:"enabled"`
	IntervalSec      int  `json:"interval_sec"`
	StartupCheck     bool `json:"startup_check"`
	RetryOnAuthError bool `json:"retry_on_auth_error"`
	AutoSwitch       bool `json:"auto_switch_account"`
}

type LoginStatusFile struct {
	Success          bool              `json:"success"`
	Status           string            `json:"status,omitempty"`
	Email            string            `json:"email,omitempty"`
	ProfileDir       string            `json:"profile_dir,omitempty"`
	PendingStatePath string            `json:"pending_state_path,omitempty"`
	StorageStatePath string            `json:"storage_state_path,omitempty"`
	ProbePath        string            `json:"probe_path,omitempty"`
	UserID           string            `json:"user_id,omitempty"`
	UserName         string            `json:"user_name,omitempty"`
	SpaceID          string            `json:"space_id,omitempty"`
	SpaceViewID      string            `json:"space_view_id,omitempty"`
	SpaceName        string            `json:"space_name,omitempty"`
	Workspaces       []NotionWorkspace `json:"workspaces,omitempty"`
	ClientVersion    string            `json:"client_version,omitempty"`
	CurrentURL       string            `json:"current_url,omitempty"`
	FinalURL         string            `json:"final_url,omitempty"`
	Title            string            `json:"title,omitempty"`
	Message          string            `json:"message,omitempty"`
	Error            string            `json:"error,omitempty"`
	UpdatedAt        string            `json:"updated_at,omitempty"`
	LastLoginAt      string            `json:"last_login_at,omitempty"`
}

func canonicalEmailKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func getAccountEmailKey(account NotionAccount) string {
	if account.emailKey != "" {
		return account.emailKey
	}
	return canonicalEmailKey(account.Email)
}

func accountPathSlug(email string) string {
	clean := canonicalEmailKey(email)
	if clean == "" {
		return "account"
	}
	clean = accountPathSlugPattern.ReplaceAllString(clean, "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		return "account"
	}
	return clean
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func resolveConfigRelativePath(configPath string, raw string, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = strings.TrimSpace(fallback)
	}
	if value == "" {
		return ""
	}
	if pathLooksAbsoluteAnyOS(value) {
		return filepath.Clean(value)
	}
	if strings.TrimSpace(configPath) != "" {
		return filepath.Clean(filepath.Join(filepath.Dir(configPath), value))
	}
	return filepath.Clean(value)
}

func pathLooksAbsoluteAnyOS(value string) bool {
	clean := strings.TrimSpace(value)
	if clean == "" {
		return false
	}
	if filepath.IsAbs(clean) {
		return true
	}
	if windowsAbsolutePathPattern.MatchString(clean) {
		return true
	}
	if strings.HasPrefix(clean, `\\`) {
		return true
	}
	if strings.HasPrefix(clean, "/") {
		return true
	}
	return false
}

func isForeignAbsolutePath(value string) bool {
	clean := strings.TrimSpace(value)
	if clean == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(clean, "/")
	}
	if windowsAbsolutePathPattern.MatchString(clean) {
		return true
	}
	if strings.HasPrefix(clean, `\\`) {
		return true
	}
	return false
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	stat, err := os.Stat(path)
	return err == nil && !stat.IsDir()
}

func dirExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	stat, err := os.Stat(path)
	return err == nil && stat.IsDir()
}

func (cfg AppConfig) FindAccount(email string) (NotionAccount, int, bool) {
	target := canonicalEmailKey(email)
	if target == "" {
		return NotionAccount{}, -1, false
	}
	for i, account := range cfg.Accounts {
		if getAccountEmailKey(account) == target {
			return normalizeAccountWorkspaces(account), i, true
		}
	}
	return NotionAccount{}, -1, false
}

func (cfg AppConfig) FindAccountWorkspace(email string, workspaceID string) (NotionAccount, int, bool) {
	account, index, ok := cfg.FindAccount(email)
	if !ok {
		return NotionAccount{}, -1, false
	}
	selected, ok := accountForWorkspace(account, workspaceID)
	if !ok {
		return NotionAccount{}, -1, false
	}
	return selected, index, true
}

func (cfg AppConfig) ResolveActiveWorkspace() (NotionAccount, int, bool) {
	account, index, ok := cfg.ResolveActiveAccount()
	if !ok {
		return NotionAccount{}, -1, false
	}
	workspaceID := firstNonEmpty(cfg.ActiveWorkspaceID, account.DefaultWorkspaceID, account.SpaceID)
	selected, ok := accountForWorkspace(account, workspaceID)
	if !ok {
		return account, index, true
	}
	return selected, index, true
}

func (cfg AppConfig) ResolveActiveAccount() (NotionAccount, int, bool) {
	if cfg.ActiveAccount == "" {
		return NotionAccount{}, -1, false
	}
	return cfg.FindAccount(cfg.ActiveAccount)
}

func (cfg AppConfig) ResolveSessionTarget() (probePath string, userName string, spaceName string, activeEmail string) {
	if account, _, ok := cfg.ResolveActiveWorkspace(); ok {
		account = ensureAccountPaths(cfg, account)
		return strings.TrimSpace(account.ProbeJSON), firstNonEmpty(account.UserName, cfg.UserName), firstNonEmpty(account.SpaceName, cfg.SpaceName), account.Email
	}
	return resolveConfigRelativePath(cfg.ConfigPath, cfg.ProbeJSON, cfg.ProbeJSON), strings.TrimSpace(cfg.UserName), strings.TrimSpace(cfg.SpaceName), ""
}

func (cfg AppConfig) SessionConfigured() bool {
	probePath, _, _, _ := cfg.ResolveSessionTarget()
	return strings.TrimSpace(probePath) != ""
}

func (cfg AppConfig) ResolveLoginHelper() ResolvedLoginHelper {
	return ResolvedLoginHelper{
		SessionsDir: resolveConfigRelativePath(cfg.ConfigPath, cfg.LoginHelper.SessionsDir, "probe_files/notion_accounts"),
		TimeoutSec:  maxInt(cfg.LoginHelper.TimeoutSec, 30),
	}
}

func (cfg AppConfig) ResolveSessionRefresh() ResolvedSessionRefresh {
	return ResolvedSessionRefresh{
		Enabled:          cfg.SessionRefresh.Enabled,
		IntervalSec:      maxInt(cfg.SessionRefresh.IntervalSec, 60),
		StartupCheck:     cfg.SessionRefresh.StartupCheck,
		RetryOnAuthError: cfg.SessionRefresh.RetryOnAuthError,
		AutoSwitch:       cfg.SessionRefresh.AutoSwitch,
	}
}

func (helper ResolvedLoginHelper) ProfileDirFor(email string) string {
	baseDir := strings.TrimSpace(helper.SessionsDir)
	if baseDir == "" {
		baseDir = filepath.Clean("probe_files/notion_accounts")
	}
	return filepath.Join(baseDir, accountPathSlug(email))
}

func (helper ResolvedLoginHelper) PendingStatePath(profileDir string) string {
	return filepath.Join(profileDir, "pending_login.json")
}

func (helper ResolvedLoginHelper) StorageStatePath(profileDir string) string {
	return filepath.Join(profileDir, "storage_state.json")
}

func (helper ResolvedLoginHelper) ProbePath(profileDir string) string {
	return filepath.Join(profileDir, "probe.json")
}

func ensureAccountPaths(cfg AppConfig, account NotionAccount) NotionAccount {
	account.emailKey = canonicalEmailKey(account.Email)
	helper := cfg.ResolveLoginHelper()
	if strings.TrimSpace(account.ProfileDir) == "" || isForeignAbsolutePath(account.ProfileDir) {
		account.ProfileDir = helper.ProfileDirFor(account.Email)
	} else {
		account.ProfileDir = resolveConfigRelativePath(cfg.ConfigPath, account.ProfileDir, account.ProfileDir)
	}
	if strings.TrimSpace(account.StorageStatePath) == "" || isForeignAbsolutePath(account.StorageStatePath) {
		account.StorageStatePath = helper.StorageStatePath(account.ProfileDir)
	} else {
		account.StorageStatePath = resolveConfigRelativePath(cfg.ConfigPath, account.StorageStatePath, account.StorageStatePath)
	}
	if strings.TrimSpace(account.PendingStatePath) == "" || isForeignAbsolutePath(account.PendingStatePath) {
		account.PendingStatePath = helper.PendingStatePath(account.ProfileDir)
	} else {
		account.PendingStatePath = resolveConfigRelativePath(cfg.ConfigPath, account.PendingStatePath, account.PendingStatePath)
	}
	if strings.TrimSpace(account.ProbeJSON) == "" || isForeignAbsolutePath(account.ProbeJSON) {
		account.ProbeJSON = helper.ProbePath(account.ProfileDir)
	} else {
		account.ProbeJSON = resolveConfigRelativePath(cfg.ConfigPath, account.ProbeJSON, account.ProbeJSON)
	}
	return account
}

// UpsertAccountRuntimeState stores an account whose runtime bookkeeping is
// authoritative, then restores the caller's values for the fields where a zero
// legitimately means "cleared".
//
// UpsertAccount treats every zero value as "not supplied" and copies the stored
// value back, which is right for partial edits from the admin API but wrong for
// the dispatch path: a success sets ConsecutiveFailures to 0 and LastError to
// "", and the merge promptly restored the stale failure. The counter therefore
// never dropped once it had risen, and computeAccountCooldown scales its wait
// with it, so a healthy account accumulated ever longer cooldowns and sorted
// worse in the pool forever.
func (cfg *AppConfig) UpsertAccountRuntimeState(account NotionAccount) (NotionAccount, int) {
	selectedWorkspaceID := accountWorkspaceID(account)
	runtime := struct {
		status               string
		lastError            string
		cooldownUntil        string
		windowStartedAt      string
		windowRequestCount   int
		consecutiveFailures  int
		totalSuccesses       int
		totalFailures        int
		lastQuotaExhaustedAt string
	}{
		status:               account.Status,
		lastError:            account.LastError,
		cooldownUntil:        account.CooldownUntil,
		windowStartedAt:      account.WindowStartedAt,
		windowRequestCount:   account.WindowRequestCount,
		consecutiveFailures:  account.ConsecutiveFailures,
		totalSuccesses:       account.TotalSuccesses,
		totalFailures:        account.TotalFailures,
		lastQuotaExhaustedAt: account.LastQuotaExhaustedAt,
	}
	account = syncSelectedWorkspaceFromAccount(account)
	stored, index := cfg.UpsertAccount(account)
	if index >= 0 && index < len(cfg.Accounts) && selectedWorkspaceID != "" {
		live := normalizeAccountWorkspaces(cfg.Accounts[index])
		workspace := workspaceFromAccountFields(account)
		workspace.ID = selectedWorkspaceID
		setAccountWorkspace(&live, workspace)
		live = normalizeAccountWorkspaces(live)
		cfg.Accounts[index] = live
		stored = live
	}
	stored.Status = runtime.status
	stored.LastError = runtime.lastError
	stored.CooldownUntil = runtime.cooldownUntil
	stored.WindowStartedAt = runtime.windowStartedAt
	stored.WindowRequestCount = runtime.windowRequestCount
	stored.ConsecutiveFailures = runtime.consecutiveFailures
	stored.LastQuotaExhaustedAt = firstNonEmpty(runtime.lastQuotaExhaustedAt, stored.LastQuotaExhaustedAt)
	// Cumulative totals only ever grow, so never let a merge walk them back.
	if stored.TotalSuccesses < runtime.totalSuccesses {
		stored.TotalSuccesses = runtime.totalSuccesses
	}
	if stored.TotalFailures < runtime.totalFailures {
		stored.TotalFailures = runtime.totalFailures
	}
	if index >= 0 && index < len(cfg.Accounts) {
		cfg.Accounts[index] = stored
	}
	return stored, index
}

func (cfg *AppConfig) UpsertAccount(account NotionAccount) (NotionAccount, int) {
	rawSpaceID := strings.TrimSpace(account.SpaceID)
	rawSpaceViewID := strings.TrimSpace(account.SpaceViewID)
	rawSpaceName := strings.TrimSpace(account.SpaceName)
	rawPlanType := strings.TrimSpace(account.PlanType)
	rawDefaultWorkspaceID := strings.TrimSpace(account.DefaultWorkspaceID)
	account = ensureAccountPaths(*cfg, account)
	if account.selectedWorkspaceID != "" {
		account = syncSelectedWorkspaceFromAccount(account)
	} else {
		account = normalizeAccountWorkspaces(account)
	}
	if existing, index, ok := cfg.FindAccount(account.Email); ok {
		existing = normalizeAccountWorkspaces(existing)
		if account.ProbeJSON == "" {
			account.ProbeJSON = existing.ProbeJSON
		}
		if account.ProfileDir == "" {
			account.ProfileDir = existing.ProfileDir
		}
		if account.StorageStatePath == "" {
			account.StorageStatePath = existing.StorageStatePath
		}
		if account.PendingStatePath == "" {
			account.PendingStatePath = existing.PendingStatePath
		}
		if account.UserID == "" {
			account.UserID = existing.UserID
		}
		if account.UserName == "" {
			account.UserName = existing.UserName
		}
		if account.DefaultWorkspaceID == "" {
			account.DefaultWorkspaceID = existing.DefaultWorkspaceID
		}
		if rawDefaultWorkspaceID == "" {
			account.DefaultWorkspaceID = existing.DefaultWorkspaceID
		}
		if account.ClientVersion == "" {
			account.ClientVersion = existing.ClientVersion
		}
		if account.Status == "" {
			account.Status = existing.Status
		}
		if account.LastError == "" {
			account.LastError = existing.LastError
		}
		if account.LastLoginAt == "" {
			account.LastLoginAt = existing.LastLoginAt
		}
		if account.Priority == 0 {
			account.Priority = existing.Priority
		}
		if account.LastRefreshAt == "" {
			account.LastRefreshAt = existing.LastRefreshAt
		}
		if account.LastReloginAt == "" {
			account.LastReloginAt = existing.LastReloginAt
		}
		if account.ConsecutiveFailures == 0 {
			account.ConsecutiveFailures = existing.ConsecutiveFailures
		}
		if account.TotalSuccesses == 0 {
			account.TotalSuccesses = existing.TotalSuccesses
		}
		if account.TotalFailures == 0 {
			account.TotalFailures = existing.TotalFailures
		}
		legacyWorkspace := NotionWorkspace{
			ID:       firstNonEmpty(rawSpaceID, account.DefaultWorkspaceID, existing.DefaultWorkspaceID),
			ViewID:   rawSpaceViewID,
			Name:     rawSpaceName,
			PlanType: rawPlanType,
		}
		if legacyWorkspace.ID != "" && (rawSpaceID != "" || rawSpaceViewID != "" || rawSpaceName != "" || rawPlanType != "") {
			account.Workspaces = append(account.Workspaces, legacyWorkspace)
		}
		merged := existing
		merged.Email = account.Email
		merged.emailKey = account.emailKey
		if account.ProbeJSON != "" {
			merged.ProbeJSON = account.ProbeJSON
		}
		if account.ProfileDir != "" {
			merged.ProfileDir = account.ProfileDir
		}
		if account.StorageStatePath != "" {
			merged.StorageStatePath = account.StorageStatePath
		}
		if account.PendingStatePath != "" {
			merged.PendingStatePath = account.PendingStatePath
		}
		if account.UserID != "" {
			merged.UserID = account.UserID
		}
		if account.UserName != "" {
			merged.UserName = account.UserName
		}
		if account.ClientVersion != "" {
			merged.ClientVersion = account.ClientVersion
		}
		if account.Status != "" {
			merged.Status = account.Status
		}
		if account.LastError != "" {
			merged.LastError = account.LastError
		}
		if account.LastLoginAt != "" {
			merged.LastLoginAt = account.LastLoginAt
		}
		if account.LastRefreshAt != "" {
			merged.LastRefreshAt = account.LastRefreshAt
		}
		if account.LastReloginAt != "" {
			merged.LastReloginAt = account.LastReloginAt
		}
		if account.LastUsedAt != "" {
			merged.LastUsedAt = account.LastUsedAt
		}
		if account.LastSuccessAt != "" {
			merged.LastSuccessAt = account.LastSuccessAt
		}
		if account.CooldownUntil != "" {
			merged.CooldownUntil = account.CooldownUntil
		}
		if account.WindowStartedAt != "" {
			merged.WindowStartedAt = account.WindowStartedAt
		}
		if account.WindowRequestCount != 0 {
			merged.WindowRequestCount = account.WindowRequestCount
		}
		if account.ConsecutiveFailures != 0 {
			merged.ConsecutiveFailures = account.ConsecutiveFailures
		}
		if account.TotalSuccesses != 0 {
			merged.TotalSuccesses = account.TotalSuccesses
		}
		if account.TotalFailures != 0 {
			merged.TotalFailures = account.TotalFailures
		}
		if account.Priority != 0 {
			merged.Priority = account.Priority
		}
		if account.StickyProxyAccount != "" {
			merged.StickyProxyAccount = account.StickyProxyAccount
		}
		if account.ProxyMode != "" {
			merged.ProxyMode = account.ProxyMode
		}
		if account.ProxyURL != "" {
			merged.ProxyURL = account.ProxyURL
		}
		if account.ProxyHTTPURL != "" {
			merged.ProxyHTTPURL = account.ProxyHTTPURL
		}
		if account.ProxyHTTPSURL != "" {
			merged.ProxyHTTPSURL = account.ProxyHTTPSURL
		}
		if account.ResinURL != "" {
			merged.ResinURL = account.ResinURL
		}
		if account.ResinPlatform != "" {
			merged.ResinPlatform = account.ResinPlatform
		}
		if account.ResinMode != "" {
			merged.ResinMode = account.ResinMode
		}
		merged.Disabled = account.Disabled || existing.Disabled
		if account.DefaultWorkspaceID != "" {
			merged.DefaultWorkspaceID = account.DefaultWorkspaceID
		}
		merged.Workspaces = append([]NotionWorkspace(nil), existing.Workspaces...)
		for _, incoming := range account.Workspaces {
			incoming = normalizeWorkspace(incoming)
			if incoming.ID == "" {
				continue
			}
			found := false
			for i := range merged.Workspaces {
				if merged.Workspaces[i].ID == incoming.ID {
					merged.Workspaces[i] = mergeWorkspaceValues(merged.Workspaces[i], incoming)
					found = true
					break
				}
			}
			if !found {
				merged.Workspaces = append(merged.Workspaces, incoming)
			}
		}
		merged = normalizeAccountWorkspaces(merged)
		cfg.Accounts[index] = merged
		return merged, index
	}
	account = normalizeAccountWorkspaces(account)
	cfg.Accounts = append(cfg.Accounts, account)
	return account, len(cfg.Accounts) - 1
}

func (cfg *AppConfig) DeleteAccount(email string) bool {
	target := canonicalEmailKey(email)
	_, index, ok := cfg.FindAccount(target)
	if !ok {
		return false
	}
	cfg.Accounts = append(cfg.Accounts[:index], cfg.Accounts[index+1:]...)
	if canonicalEmailKey(cfg.ActiveAccount) == target {
		cfg.ActiveAccount = ""
		cfg.ActiveWorkspaceID = ""
		cfg.ProbeJSON = ""
	}
	return true
}

func readLoginStatusFile(path string) (LoginStatusFile, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return LoginStatusFile{}, fmt.Errorf("empty login status path")
	}
	raw, err := os.ReadFile(clean)
	if err != nil {
		return LoginStatusFile{}, err
	}
	var payload LoginStatusFile
	if err := json.Unmarshal(raw, &payload); err != nil {
		return LoginStatusFile{}, err
	}
	return payload, nil
}
