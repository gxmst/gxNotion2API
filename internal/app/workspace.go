package app

import "strings"

func normalizeWorkspace(workspace NotionWorkspace) NotionWorkspace {
	workspace.ID = strings.TrimSpace(workspace.ID)
	workspace.ViewID = strings.TrimSpace(workspace.ViewID)
	workspace.Name = strings.TrimSpace(workspace.Name)
	workspace.PlanType = strings.TrimSpace(workspace.PlanType)
	workspace.SubscriptionTier = strings.TrimSpace(workspace.SubscriptionTier)
	workspace.Status = strings.TrimSpace(workspace.Status)
	workspace.LastError = strings.TrimSpace(workspace.LastError)
	workspace.WindowStartedAt = strings.TrimSpace(workspace.WindowStartedAt)
	workspace.CooldownUntil = strings.TrimSpace(workspace.CooldownUntil)
	workspace.LastUsedAt = strings.TrimSpace(workspace.LastUsedAt)
	workspace.LastSuccessAt = strings.TrimSpace(workspace.LastSuccessAt)
	workspace.LastRefreshAt = strings.TrimSpace(workspace.LastRefreshAt)
	workspace.LastQuotaExhaustedAt = strings.TrimSpace(workspace.LastQuotaExhaustedAt)
	workspace.MaxConcurrency = normalizeAccountMaxConcurrency(workspace.MaxConcurrency)
	if workspace.HourlyQuota < 0 {
		workspace.HourlyQuota = 0
	}
	return workspace
}

func workspaceFromAccountFields(account NotionAccount) NotionWorkspace {
	return normalizeWorkspace(NotionWorkspace{
		ID:                   account.SpaceID,
		ViewID:               account.SpaceViewID,
		Name:                 account.SpaceName,
		PlanType:             account.PlanType,
		Priority:             account.Priority,
		HourlyQuota:          account.HourlyQuota,
		MaxConcurrency:       account.MaxConcurrency,
		WindowStartedAt:      account.WindowStartedAt,
		WindowRequestCount:   account.WindowRequestCount,
		CooldownUntil:        account.CooldownUntil,
		LastUsedAt:           account.LastUsedAt,
		LastSuccessAt:        account.LastSuccessAt,
		LastRefreshAt:        account.LastRefreshAt,
		LastQuotaExhaustedAt: account.LastQuotaExhaustedAt,
		ConsecutiveFailures:  account.ConsecutiveFailures,
		TotalSuccesses:       account.TotalSuccesses,
		TotalFailures:        account.TotalFailures,
		Status:               account.Status,
		LastError:            account.LastError,
	})
}

func projectWorkspaceToAccount(account *NotionAccount, workspace NotionWorkspace) {
	if account == nil {
		return
	}
	workspace = normalizeWorkspace(workspace)
	account.SpaceID = workspace.ID
	account.SpaceViewID = workspace.ViewID
	account.SpaceName = workspace.Name
	account.PlanType = workspace.PlanType
	account.Priority = workspace.Priority
	account.HourlyQuota = workspace.HourlyQuota
	account.MaxConcurrency = workspace.MaxConcurrency
	account.WindowStartedAt = workspace.WindowStartedAt
	account.WindowRequestCount = workspace.WindowRequestCount
	account.CooldownUntil = workspace.CooldownUntil
	account.LastUsedAt = workspace.LastUsedAt
	account.LastSuccessAt = workspace.LastSuccessAt
	account.LastRefreshAt = workspace.LastRefreshAt
	account.LastQuotaExhaustedAt = workspace.LastQuotaExhaustedAt
	account.ConsecutiveFailures = workspace.ConsecutiveFailures
	account.TotalSuccesses = workspace.TotalSuccesses
	account.TotalFailures = workspace.TotalFailures
	if workspace.Status != "" {
		account.Status = workspace.Status
	}
	if workspace.LastError != "" {
		account.LastError = workspace.LastError
	}
}

func backfillWorkspaceFromAccount(workspace NotionWorkspace, account NotionAccount) NotionWorkspace {
	if workspace.ViewID == "" {
		workspace.ViewID = strings.TrimSpace(account.SpaceViewID)
	}
	if workspace.Name == "" {
		workspace.Name = strings.TrimSpace(account.SpaceName)
	}
	if workspace.PlanType == "" {
		workspace.PlanType = strings.TrimSpace(account.PlanType)
	}
	if workspace.MaxConcurrency <= 0 && account.MaxConcurrency > 0 {
		workspace.MaxConcurrency = account.MaxConcurrency
	}
	if workspace.HourlyQuota == 0 && account.HourlyQuota > 0 {
		workspace.HourlyQuota = account.HourlyQuota
	}
	if workspace.WindowStartedAt == "" {
		workspace.WindowStartedAt = account.WindowStartedAt
	}
	if workspace.WindowRequestCount == 0 {
		workspace.WindowRequestCount = account.WindowRequestCount
	}
	if workspace.CooldownUntil == "" {
		workspace.CooldownUntil = account.CooldownUntil
	}
	if workspace.LastUsedAt == "" {
		workspace.LastUsedAt = account.LastUsedAt
	}
	if workspace.LastSuccessAt == "" {
		workspace.LastSuccessAt = account.LastSuccessAt
	}
	if workspace.LastRefreshAt == "" {
		workspace.LastRefreshAt = account.LastRefreshAt
	}
	if workspace.LastQuotaExhaustedAt == "" {
		workspace.LastQuotaExhaustedAt = account.LastQuotaExhaustedAt
	}
	if workspace.ConsecutiveFailures == 0 {
		workspace.ConsecutiveFailures = account.ConsecutiveFailures
	}
	if workspace.TotalSuccesses == 0 {
		workspace.TotalSuccesses = account.TotalSuccesses
	}
	if workspace.TotalFailures == 0 {
		workspace.TotalFailures = account.TotalFailures
	}
	if workspace.Status == "" {
		workspace.Status = account.Status
	}
	if workspace.LastError == "" {
		workspace.LastError = account.LastError
	}
	return normalizeWorkspace(workspace)
}

func accountWorkspaceID(account NotionAccount) string {
	if selected := strings.TrimSpace(account.selectedWorkspaceID); selected != "" {
		return selected
	}
	if defaultID := strings.TrimSpace(account.DefaultWorkspaceID); defaultID != "" {
		return defaultID
	}
	if spaceID := strings.TrimSpace(account.SpaceID); spaceID != "" {
		return spaceID
	}
	if len(account.Workspaces) > 0 {
		return strings.TrimSpace(account.Workspaces[0].ID)
	}
	return ""
}

func accountWorkspace(account NotionAccount, workspaceID string) (NotionWorkspace, bool) {
	want := strings.TrimSpace(workspaceID)
	if want == "" {
		want = accountWorkspaceID(account)
	}
	for _, workspace := range account.Workspaces {
		workspace = normalizeWorkspace(workspace)
		if workspace.ID != "" && workspace.ID == want {
			if workspace.Status == "" {
				workspace.Status = strings.TrimSpace(account.Status)
			}
			if workspace.LastError == "" && strings.TrimSpace(workspace.ID) == strings.TrimSpace(account.SpaceID) {
				workspace.LastError = strings.TrimSpace(account.LastError)
			}
			return workspace, true
		}
	}
	legacy := workspaceFromAccountFields(account)
	if want == "" && legacy.ID == "" {
		return legacy, true
	}
	if legacy.ID != "" && (want == "" || legacy.ID == want) {
		return legacy, true
	}
	return NotionWorkspace{}, false
}

func setAccountWorkspace(account *NotionAccount, workspace NotionWorkspace) {
	if account == nil {
		return
	}
	workspace = normalizeWorkspace(workspace)
	if workspace.ID == "" {
		return
	}
	for i := range account.Workspaces {
		if strings.TrimSpace(account.Workspaces[i].ID) == workspace.ID {
			account.Workspaces[i] = workspace
			if workspace.ID == strings.TrimSpace(account.DefaultWorkspaceID) {
				projectWorkspaceToAccount(account, workspace)
			}
			return
		}
	}
	account.Workspaces = append(account.Workspaces, workspace)
	if strings.TrimSpace(account.DefaultWorkspaceID) == "" {
		account.DefaultWorkspaceID = workspace.ID
		projectWorkspaceToAccount(account, workspace)
	}
}

// normalizeAccountWorkspaces migrates the legacy single-workspace fields into
// the list and keeps those fields projected from the selected default so old
// callers and old probe files continue to work.
func normalizeAccountWorkspaces(account NotionAccount) NotionAccount {
	selectedWorkspaceID := account.selectedWorkspaceID
	account.selectedWorkspaceID = ""
	account.DefaultWorkspaceID = strings.TrimSpace(account.DefaultWorkspaceID)
	legacy := workspaceFromAccountFields(account)
	workspaces := make([]NotionWorkspace, 0, len(account.Workspaces)+1)
	seen := map[string]struct{}{}
	for _, raw := range account.Workspaces {
		workspace := normalizeWorkspace(raw)
		if workspace.ID == "" {
			continue
		}
		if _, ok := seen[workspace.ID]; ok {
			continue
		}
		if workspace.Status == "" && workspace.ID == legacy.ID {
			workspace.Status = legacy.Status
		}
		workspaces = append(workspaces, workspace)
		seen[workspace.ID] = struct{}{}
	}
	if legacy.ID != "" {
		if _, ok := seen[legacy.ID]; !ok {
			replacedDefault := false
			if selectedWorkspaceID == "" && account.DefaultWorkspaceID != "" {
				// A caller using the legacy top-level fields may have changed
				// space_id after the account was normalized. Treat that as a
				// replacement of the old default workspace rather than keeping
				// a stale candidate that can receive old conversations.
				for i := range workspaces {
					if workspaces[i].ID != account.DefaultWorkspaceID {
						continue
					}
					delete(seen, workspaces[i].ID)
					workspaces[i] = legacy
					seen[legacy.ID] = struct{}{}
					account.DefaultWorkspaceID = legacy.ID
					replacedDefault = true
					break
				}
			}
			if !replacedDefault {
				workspaces = append(workspaces, legacy)
				seen[legacy.ID] = struct{}{}
			}
		} else if account.DefaultWorkspaceID == "" {
			// A legacy top-level edit is the only representation old configs
			// have, so it remains authoritative until a default is explicit.
			for i := range workspaces {
				if workspaces[i].ID == legacy.ID {
					workspaces[i] = legacy
				}
			}
		}
	}
	account.Workspaces = workspaces
	if account.DefaultWorkspaceID == "" {
		account.DefaultWorkspaceID = strings.TrimSpace(account.SpaceID)
	}
	if account.DefaultWorkspaceID == "" && len(account.Workspaces) > 0 {
		account.DefaultWorkspaceID = account.Workspaces[0].ID
	}
	if _, ok := accountWorkspace(account, account.DefaultWorkspaceID); !ok && len(account.Workspaces) > 0 {
		account.DefaultWorkspaceID = account.Workspaces[0].ID
	}
	if workspace, ok := accountWorkspace(account, account.DefaultWorkspaceID); ok {
		if legacy.ID == "" || legacy.ID == workspace.ID {
			workspace = backfillWorkspaceFromAccount(workspace, account)
		}
		projectWorkspaceToAccount(&account, workspace)
	}
	account.selectedWorkspaceID = selectedWorkspaceID
	if selectedWorkspaceID != "" {
		if workspace, ok := accountWorkspace(account, selectedWorkspaceID); ok {
			projectWorkspaceToAccount(&account, workspace)
		}
	}
	return account
}

func accountForWorkspace(account NotionAccount, workspaceID string) (NotionAccount, bool) {
	account = normalizeAccountWorkspaces(account)
	workspace, ok := accountWorkspace(account, workspaceID)
	if !ok {
		return NotionAccount{}, false
	}
	account.selectedWorkspaceID = workspace.ID
	projectWorkspaceToAccount(&account, workspace)
	return account, true
}

func syncSelectedWorkspaceFromAccount(account NotionAccount) NotionAccount {
	workspaceID := accountWorkspaceID(account)
	if workspaceID == "" {
		return account
	}
	workspace := workspaceFromAccountFields(account)
	workspace.ID = workspaceID
	account = normalizeAccountWorkspaces(account)
	setAccountWorkspace(&account, workspace)
	account.selectedWorkspaceID = workspaceID
	projectWorkspaceToAccount(&account, workspace)
	return account
}

func mergeWorkspaceValues(existing NotionWorkspace, incoming NotionWorkspace) NotionWorkspace {
	merged := normalizeWorkspace(existing)
	incoming = normalizeWorkspace(incoming)
	if incoming.ID != "" {
		merged.ID = incoming.ID
	}
	if incoming.ViewID != "" {
		merged.ViewID = incoming.ViewID
	}
	if incoming.Name != "" {
		merged.Name = incoming.Name
	}
	if incoming.PlanType != "" {
		merged.PlanType = incoming.PlanType
	}
	if incoming.SubscriptionTier != "" {
		merged.SubscriptionTier = incoming.SubscriptionTier
	}
	if incoming.AIEnabled {
		merged.AIEnabled = true
	}
	if incoming.Priority != 0 {
		merged.Priority = incoming.Priority
	}
	if incoming.HourlyQuota != 0 {
		merged.HourlyQuota = incoming.HourlyQuota
	}
	if incoming.MaxConcurrency != 0 {
		merged.MaxConcurrency = incoming.MaxConcurrency
	}
	if incoming.WindowStartedAt != "" {
		merged.WindowStartedAt = incoming.WindowStartedAt
	}
	if incoming.WindowRequestCount != 0 {
		merged.WindowRequestCount = incoming.WindowRequestCount
	}
	if incoming.CooldownUntil != "" {
		merged.CooldownUntil = incoming.CooldownUntil
	}
	if incoming.LastUsedAt != "" {
		merged.LastUsedAt = incoming.LastUsedAt
	}
	if incoming.LastSuccessAt != "" {
		merged.LastSuccessAt = incoming.LastSuccessAt
	}
	if incoming.LastRefreshAt != "" {
		merged.LastRefreshAt = incoming.LastRefreshAt
	}
	if incoming.LastQuotaExhaustedAt != "" {
		merged.LastQuotaExhaustedAt = incoming.LastQuotaExhaustedAt
	}
	if incoming.ConsecutiveFailures != 0 {
		merged.ConsecutiveFailures = incoming.ConsecutiveFailures
	}
	if incoming.TotalSuccesses != 0 {
		merged.TotalSuccesses = incoming.TotalSuccesses
	}
	if incoming.TotalFailures != 0 {
		merged.TotalFailures = incoming.TotalFailures
	}
	if incoming.Status != "" {
		merged.Status = incoming.Status
	}
	if incoming.LastError != "" {
		merged.LastError = incoming.LastError
	}
	return normalizeWorkspace(merged)
}

func dispatchWorkspaceKey(account NotionAccount) string {
	email := getAccountEmailKey(account)
	workspaceID := accountWorkspaceID(account)
	if workspaceID == "" || len(account.Workspaces) <= 1 {
		return email
	}
	return email + "\x00" + workspaceID
}

func accountWorkspaceCandidates(account NotionAccount) []NotionAccount {
	account = normalizeAccountWorkspaces(account)
	if len(account.Workspaces) == 0 {
		return []NotionAccount{account}
	}
	out := make([]NotionAccount, 0, len(account.Workspaces))
	for _, workspace := range account.Workspaces {
		if candidate, ok := accountForWorkspace(account, workspace.ID); ok {
			out = append(out, candidate)
		}
	}
	return out
}
