package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Refresh only workspace metadata. This never starts an inference or replaces
// the account's cookies, proxy identity, counters, or conversation ownership.
func (a *App) handleAdminWorkspaceRefresh(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthOK(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	payload, err := a.decodeBody(w, r)
	if err != nil {
		writeInvalidBodyError(w, err)
		return
	}
	cfg, _, _ := a.State.Snapshot()
	account, _, ok := cfg.FindAccount(strings.TrimSpace(stringValue(payload["email"])))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
		return
	}
	if until := parseOptionalRFC3339(account.CredentialCooldownUntil); until.After(time.Now()) {
		w.Header().Set("Retry-After", until.UTC().Format(http.TimeFormat))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"detail": "account is cooling down; refresh after " + account.CredentialCooldownUntil})
		return
	}
	session, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client := newNotionAIClient(session, cfg, account.Email)
	body, err := client.postJSON(ctx, cfg.NotionUpstream().API("loadUserContent"), map[string]any{}, "application/json")
	var decoded map[string]any
	if err == nil {
		err = json.Unmarshal(body, &decoded)
	}
	if err != nil {
		a.writeUpstreamError(w, err)
		return
	}
	metadata := parseLoadUserContentMetadataForUser(decoded, session.UserID)
	metadataPresent := len(metadata.Workspaces) > 0
	if !metadataPresent {
		root := unwrapRecordValue(mapValue(mapValue(decoded["recordMap"])["user_root"])[session.UserID])
		pointers, present := root["space_view_pointers"].([]any)
		metadataPresent = present && len(pointers) == 0
	}
	if metadata.UserID != session.UserID || !metadataPresent {
		writeJSON(w, http.StatusBadGateway, map[string]any{"detail": "workspace metadata missing or belongs to a different user"})
		return
	}
	if err = a.State.applyWorkspaceDiscovery(account, metadata.Workspaces); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.buildAccountsPayload())
}

func (s *ServerState) applyWorkspaceDiscovery(started NotionAccount, discovered []discoveredSpaceCandidate) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cfg, _, _ := s.Snapshot()
	current, index, ok := cfg.FindAccount(started.Email)
	if !ok || !workspaceDiscoveryIdentityUnchanged(started, current) {
		return fmt.Errorf("account changed during workspace refresh; retry")
	}
	// Do not let legacy account-level plan metadata backfill a workspace whose
	// entitlement the fresh response no longer confirms.
	current.PlanType = ""
	for _, prior := range current.Workspaces {
		prior.SubscriptionTier = ""
		prior.PlanType = ""
		prior.AIEnabled = false
		prior.AIDisabled = true
		setAccountWorkspace(&current, prior)
	}
	for _, candidate := range discovered {
		workspace, _ := accountWorkspace(current, candidate.ID)
		workspace.ID, workspace.ViewID, workspace.Name = candidate.ID, candidate.ViewID, candidate.Name
		workspace.PlanType, workspace.SubscriptionTier = candidate.PlanType, candidate.SubscriptionTier
		workspace.AIEnabled, workspace.AIDisabled = candidate.AIEnabled, candidate.AIDisabled
		setAccountWorkspace(&current, workspace)
	}
	selected, _ := accountWorkspace(current, current.DefaultWorkspaceID)
	if eligible, _ := workspaceEligibility(selected); !eligible {
		for _, workspace := range current.Workspaces {
			if eligible, _ := workspaceEligibility(workspace); eligible {
				current.DefaultWorkspaceID = workspace.ID
				break
			}
		}
	}
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[index] = normalizeAccountWorkspaces(current)
	if canonicalEmailKey(cfg.ActiveAccount) == canonicalEmailKey(current.Email) {
		active, _ := accountWorkspace(current, cfg.ActiveWorkspaceID)
		if eligible, _ := workspaceEligibility(active); !eligible {
			cfg.ActiveWorkspaceID = current.DefaultWorkspaceID
		}
	}
	return s.saveAndApplyCommitted(cfg)
}

func workspaceDiscoveryIdentityUnchanged(started, current NotionAccount) bool {
	// Workspace metadata is precisely what this request refreshes. Compare the
	// credential identity, without projecting a workspace's fields onto it.
	return canonicalEmailKey(started.Email) == canonicalEmailKey(current.Email) &&
		started.UserID == current.UserID && started.LastLoginAt == current.LastLoginAt &&
		started.ProbeJSON == current.ProbeJSON && started.StorageStatePath == current.StorageStatePath &&
		started.ProfileDir == current.ProfileDir && started.ClientVersion == current.ClientVersion
}
