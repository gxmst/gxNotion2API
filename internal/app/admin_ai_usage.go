package app

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

// aiUsageCacheTTL bounds how long a workspace AI usage report is reused. The
// upstream counters update lazily, so polling more often than this only adds
// requests; the admin endpoint accepts refresh=1 to bypass it deliberately.
const aiUsageCacheTTL = 5 * time.Minute

// aiUsageForceMinInterval caps refresh=1: each forced report costs one upstream
// call per workspace with that account's cookies, so repeated clicks (or a
// script) must not turn into a request stream against Notion.
const aiUsageForceMinInterval = 30 * time.Second

// workspaceAIUsageReport is what the admin API shows for one account's
// workspace allowance. Status "unknown" means there is nothing to report (no
// workspace recorded, account disabled); "error" means the upstream query
// itself failed and Detail says why. A report never invents numbers.
type workspaceAIUsageReport struct {
	Email         string            `json:"email"`
	SpaceID       string            `json:"space_id,omitempty"`
	WorkspaceName string            `json:"workspace_name,omitempty"`
	Status        string            `json:"status"`
	Detail        string            `json:"detail,omitempty"`
	Cached        bool              `json:"cached"`
	FetchedAt     time.Time         `json:"fetched_at,omitempty"`
	Usage         *workspaceAIUsage `json:"usage,omitempty"`
}

func (a *App) workspaceAIUsageReport(ctx context.Context, cfg AppConfig, account NotionAccount, force bool) workspaceAIUsageReport {
	account = normalizeAccountWorkspaces(account)
	return a.workspaceAIUsageReportForWorkspace(ctx, cfg, account, accountWorkspaceID(account), force)
}

func (a *App) workspaceAIUsageReportForWorkspace(ctx context.Context, cfg AppConfig, account NotionAccount, workspaceID string, force bool) workspaceAIUsageReport {
	account = normalizeAccountWorkspaces(account)
	if selected, ok := accountForWorkspace(account, workspaceID); ok {
		account = selected
	}
	email := strings.TrimSpace(account.Email)
	spaceID := strings.TrimSpace(account.SpaceID)
	report := workspaceAIUsageReport{Email: email, SpaceID: spaceID, WorkspaceName: account.SpaceName, Status: "unknown"}
	if email == "" {
		report.Detail = "account has no email"
		return report
	}
	if account.Disabled {
		report.Detail = "account is disabled"
		return report
	}
	if spaceID == "" {
		report.Detail = "account has no recorded workspace"
		return report
	}

	key := canonicalEmailKey(email) + "\x00" + spaceID
	now := time.Now()
	a.State.aiUsageMu.Lock()
	if !force && a.State.aiUsageCache != nil {
		if cached, ok := a.State.aiUsageCache[key]; ok && now.Sub(cached.FetchedAt) < aiUsageCacheTTL {
			a.State.aiUsageMu.Unlock()
			report = cached
			report.Cached = true
			return report
		}
	}
	a.State.aiUsageMu.Unlock()

	usage, err := a.fetchWorkspaceAIUsage(ctx, cfg, account, spaceID)
	report.FetchedAt = now
	if err != nil {
		// Errors are cached too, so a failing upstream is retried at most once
		// per TTL window; refresh=1 still forces a fresh attempt.
		report.Status = "error"
		report.Detail = strings.TrimSpace(err.Error())
	} else {
		usage.SpaceID = spaceID
		report.Status = "ok"
		report.Detail = ""
		report.Usage = &usage
	}
	a.State.aiUsageMu.Lock()
	if a.State.aiUsageCache == nil {
		a.State.aiUsageCache = make(map[string]workspaceAIUsageReport)
	}
	a.State.aiUsageCache[key] = report
	a.State.aiUsageMu.Unlock()
	return report
}

func (a *App) fetchWorkspaceAIUsage(ctx context.Context, cfg AppConfig, account NotionAccount, spaceID string) (workspaceAIUsage, error) {
	if a.workspaceAIUsageFetchOverride != nil {
		return a.workspaceAIUsageFetchOverride(ctx, cfg, account)
	}
	session, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		return workspaceAIUsage{}, err
	}
	client := newNotionAIClient(session, cfg, account.Email)
	usage, err := client.getAIUsageEligibility(ctx, spaceID)
	if err != nil {
		// Eligibility is the primary gate: the report is built around its
		// allowance counters, so a failure here fails the whole read rather
		// than returning a report with only the rolling windows attached.
		return workspaceAIUsage{}, err
	}
	// The rolling windows live behind a separate endpoint. Fetching them is
	// best effort: losing them must not hide the allowance counters that did
	// come back.
	if limits, limitErr := client.getCreditRateLimitStatus(ctx, spaceID); limitErr != nil {
		log.Printf("[ai-usage] credit rate limit for space %s failed: %v", spaceID, limitErr)
	} else {
		usage.RateLimit = limits
	}
	return usage, nil
}

// handleAdminWorkspaceAIUsage answers for a single account + workspace. The
// chat surface needs the current workspace's rolling windows on every
// conversation; reusing the pool-wide endpoint would query every other
// workspace too, so this one takes the target explicitly.
func (a *App) handleAdminWorkspaceAIUsage(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthOK(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	if force {
		a.State.aiUsageMu.Lock()
		if time.Since(a.State.aiUsageLastForced) < aiUsageForceMinInterval {
			force = false
		} else {
			a.State.aiUsageLastForced = time.Now()
		}
		a.State.aiUsageMu.Unlock()
	}
	cfg, _, _ := a.State.Snapshot()
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	account, _, ok := cfg.FindAccount(email)
	if email == "" {
		account, _, ok = cfg.ResolveActiveAccount()
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
		return
	}
	account = normalizeAccountWorkspaces(account)
	// An explicit workspace that does not belong to this account is a 404, not
	// a silent report for a different workspace.
	if workspaceID != "" {
		if _, found := accountForWorkspace(account, workspaceID); !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "workspace not found for account"})
			return
		}
	}
	report := a.workspaceAIUsageReportForWorkspace(r.Context(), cfg, account, workspaceID, force)
	writeJSON(w, http.StatusOK, map[string]any{
		"report":      report,
		"ttl_seconds": int(aiUsageCacheTTL.Seconds()),
	})
}

func (a *App) handleAdminAccountsAIUsage(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthOK(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	if force {
		a.State.aiUsageMu.Lock()
		if time.Since(a.State.aiUsageLastForced) < aiUsageForceMinInterval {
			force = false
		} else {
			a.State.aiUsageLastForced = time.Now()
		}
		a.State.aiUsageMu.Unlock()
	}
	cfg, _, _ := a.State.Snapshot()
	reports := make([]workspaceAIUsageReport, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		account = normalizeAccountWorkspaces(account)
		if len(account.Workspaces) == 0 {
			reports = append(reports, a.workspaceAIUsageReport(r.Context(), cfg, account, force))
			continue
		}
		for _, workspace := range account.Workspaces {
			reports = append(reports, a.workspaceAIUsageReportForWorkspace(r.Context(), cfg, account, workspace.ID, force))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":    reports,
		"ttl_seconds": int(aiUsageCacheTTL.Seconds()),
	})
}
