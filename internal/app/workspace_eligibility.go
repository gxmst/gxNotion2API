package app

import "strings"

// Trial status alone is not an entitlement. Only an explicit Business or
// Enterprise tier (including its trial variant) qualifies; team/personal do not.
func commercialWorkspaceTier(tier, plan string) bool {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" || tier == "trial" {
		tier = strings.ToLower(strings.TrimSpace(plan))
	}
	switch tier {
	case "business", "enterprise", "business_trial", "enterprise_trial":
		return true
	}
	return false
}

func workspaceEligibility(workspace NotionWorkspace) (bool, string) {
	if !commercialWorkspaceTier(workspace.SubscriptionTier, workspace.PlanType) {
		if workspace.SubscriptionTier == "" || strings.EqualFold(workspace.SubscriptionTier, "trial") {
			return false, "workspace_plan_unverified: refresh workspace metadata to confirm Business trial, Business or Enterprise"
		}
		return false, "workspace_plan_excluded: only Business trial, Business and Enterprise are supported; Free and Plus are excluded"
	}
	if workspace.AIDisabled {
		return false, "workspace_ai_disabled"
	}
	return true, "eligible"
}

func accountWorkspaceEligibility(account NotionAccount) (bool, string) {
	workspace, ok := accountWorkspace(account, accountWorkspaceID(account))
	if !ok {
		workspace = workspaceFromAccountFields(account)
	}
	return workspaceEligibility(workspace)
}
