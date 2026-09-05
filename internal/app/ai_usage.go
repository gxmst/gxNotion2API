package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// workspaceAIUsage is the normalized view of Notion's per-workspace AI
// allowance for one account. Raw counters are reported exactly as upstream
// names them; derived remainders would be guesswork, so callers derive them
// only when QuotaEnforced is true. Anything not confirmed by a successful
// upstream response stays at its zero value — treat this struct as "unknown"
// unless it came from a fetch that returned nil error.
type workspaceAIUsage struct {
	SpaceID    string `json:"space_id"`
	IsEligible bool   `json:"is_eligible"`
	// IsEligibleKnown distinguishes "V1 said not eligible" from "V1 was not
	// consulted", so a false flag is never mistaken for a real denial.
	IsEligibleKnown bool   `json:"is_eligible_known"`
	Type            string `json:"type,omitempty"`
	// QuotaEnforced is true only when upstream reported an allowance type other
	// than "unlimited"; on unlimited workspaces the free-tier limits below do
	// not actually gate usage.
	QuotaEnforced        bool  `json:"quota_enforced"`
	SpaceUsage           int   `json:"space_usage"`
	SpaceLimit           int   `json:"space_limit"`
	UserUsage            int   `json:"user_usage"`
	UserLimit            int   `json:"user_limit"`
	PremiumCreditBalance int   `json:"premium_credit_balance,omitempty"`
	PremiumCreditKnown   bool  `json:"premium_credit_known,omitempty"`
	LastUsageAtMs        int64 `json:"last_usage_at_ms,omitempty"`

	basicsFromV2 bool `json:"-"`
}

// getAIUsageEligibility fetches the workspace AI allowance for one space. The
// V2 endpoint carries the detailed usage and credit numbers; the V1 endpoint
// is the only one that reports the eligibility flag and allowance type, so
// both are consulted. Either one may fail without sinking the report as long
// as the other answered; only when both fail does the fetch error out.
func (c *NotionAIClient) getAIUsageEligibility(ctx context.Context, spaceID string) (workspaceAIUsage, error) {
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return workspaceAIUsage{}, fmt.Errorf("workspace ai usage: space id is empty")
	}
	out := workspaceAIUsage{SpaceID: spaceID}
	requestBody := map[string]any{"spaceId": spaceID}
	var firstErr error

	haveV2 := false
	body, err := c.postJSON(ctx, c.Config.NotionUpstream().API("getAIUsageEligibilityV2"), requestBody, "application/json")
	if err != nil {
		firstErr = err
	} else if parseErr := parseAIUsageEligibilityV2(body, &out); parseErr != nil {
		firstErr = parseErr
	} else {
		haveV2 = true
	}

	haveV1 := false
	body, err = c.postJSON(ctx, c.Config.NotionUpstream().API("getAIUsageEligibility"), requestBody, "application/json")
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
	} else if parseErr := parseAIUsageEligibilityV1(body, &out); parseErr != nil {
		if firstErr == nil {
			firstErr = parseErr
		}
	} else {
		haveV1 = true
	}

	if !haveV2 && !haveV1 {
		return workspaceAIUsage{}, firstErr
	}
	return out, nil
}

func parseAIUsageEligibilityV2(body []byte, out *workspaceAIUsage) error {
	var payload struct {
		Usage *struct {
			CurrentServicePeriod *struct {
				SpaceUsage int `json:"spaceUsage"`
				UserUsage  int `json:"userUsage"`
			} `json:"currentServicePeriod"`
			Lifetime *struct {
				SpaceUsage int `json:"spaceUsage"`
				UserUsage  int `json:"userUsage"`
			} `json:"lifetime"`
			TotalCreditBalance int   `json:"totalCreditBalance"`
			LastSpaceUsageAtMs int64 `json:"lastSpaceUsageAtMs"`
		} `json:"usage"`
		BasicCredits *struct {
			SpaceUsage int `json:"spaceUsage"`
			SpaceLimit int `json:"spaceLimit"`
			UserUsage  int `json:"userUsage"`
			UserLimit  int `json:"userLimit"`
		} `json:"basicCredits"`
		PremiumCredits *struct {
			TotalCreditBalance   int   `json:"totalCreditBalance"`
			ServicePeriodStartMs int64 `json:"servicePeriodStartMs"`
		} `json:"premiumCredits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode ai usage v2: %w", err)
	}
	if payload.BasicCredits != nil {
		out.SpaceUsage = payload.BasicCredits.SpaceUsage
		out.SpaceLimit = payload.BasicCredits.SpaceLimit
		out.UserUsage = payload.BasicCredits.UserUsage
		out.UserLimit = payload.BasicCredits.UserLimit
		out.basicsFromV2 = true
	} else if payload.Usage != nil && payload.Usage.Lifetime != nil {
		out.SpaceUsage = payload.Usage.Lifetime.SpaceUsage
		out.UserUsage = payload.Usage.Lifetime.UserUsage
		out.basicsFromV2 = true
	}
	if payload.PremiumCredits != nil {
		out.PremiumCreditBalance = payload.PremiumCredits.TotalCreditBalance
		out.PremiumCreditKnown = true
	}
	if payload.Usage != nil && payload.Usage.LastSpaceUsageAtMs > 0 {
		out.LastUsageAtMs = payload.Usage.LastSpaceUsageAtMs
	}
	return nil
}

func parseAIUsageEligibilityV1(body []byte, out *workspaceAIUsage) error {
	var payload struct {
		IsEligible         bool   `json:"isEligible"`
		Type               string `json:"type"`
		SpaceUsage         int    `json:"spaceUsage"`
		SpaceLimit         int    `json:"spaceLimit"`
		UserUsage          int    `json:"userUsage"`
		UserLimit          int    `json:"userLimit"`
		LastSpaceUsageAtMs int64  `json:"lastSpaceUsageAtMs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode ai usage v1: %w", err)
	}
	out.IsEligible = payload.IsEligible
	out.IsEligibleKnown = true
	out.Type = strings.TrimSpace(payload.Type)
	out.QuotaEnforced = out.Type != "" && !strings.EqualFold(out.Type, "unlimited")
	if !out.basicsFromV2 {
		out.SpaceUsage = payload.SpaceUsage
		out.SpaceLimit = payload.SpaceLimit
		out.UserUsage = payload.UserUsage
		out.UserLimit = payload.UserLimit
	}
	if out.LastUsageAtMs == 0 {
		out.LastUsageAtMs = payload.LastSpaceUsageAtMs
	}
	return nil
}
