package app

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// aiUsageCacheTTL bounds how long a workspace AI usage report is reused. The
// upstream counters update lazily, so polling more often than this only adds
// requests; the admin endpoint accepts refresh=1 to bypass it deliberately.
const aiUsageCacheTTL = 5 * time.Minute

// workspaceAIUsageReport is what the admin API shows for one account's
// workspace allowance. Status "unknown" means there is nothing to report (no
// workspace recorded, account disabled); "error" means the upstream query
// itself failed and Detail says why. A report never invents numbers.
type workspaceAIUsageReport struct {
	Email     string            `json:"email"`
	SpaceID   string            `json:"space_id,omitempty"`
	Status    string            `json:"status"`
	Detail    string            `json:"detail,omitempty"`
	Cached    bool              `json:"cached"`
	FetchedAt time.Time         `json:"fetched_at,omitempty"`
	Usage     *workspaceAIUsage `json:"usage,omitempty"`
}

func (a *App) workspaceAIUsageReport(ctx context.Context, cfg AppConfig, account NotionAccount, force bool) workspaceAIUsageReport {
	email := strings.TrimSpace(account.Email)
	spaceID := strings.TrimSpace(account.SpaceID)
	report := workspaceAIUsageReport{Email: email, SpaceID: spaceID, Status: "unknown"}
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
	return client.getAIUsageEligibility(ctx, spaceID)
}

func (a *App) handleAdminAccountsAIUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	cfg, _, _ := a.State.Snapshot()
	reports := make([]workspaceAIUsageReport, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		reports = append(reports, a.workspaceAIUsageReport(r.Context(), cfg, account, force))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":    reports,
		"ttl_seconds": int(aiUsageCacheTTL.Seconds()),
	})
}
