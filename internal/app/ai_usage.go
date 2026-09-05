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
	QuotaEnforced               bool  `json:"quota_enforced"`
	BasicUsageKnown             bool  `json:"basic_usage_known"`
	BasicLimitsKnown            bool  `json:"basic_limits_known"`
	SpaceUsage                  int   `json:"space_usage"`
	SpaceLimit                  int   `json:"space_limit"`
	UserUsage                   int   `json:"user_usage"`
	UserLimit                   int   `json:"user_limit"`
	CurrentPeriodUsageKnown     bool  `json:"current_period_usage_known"`
	CurrentPeriodSpaceUsage     int   `json:"current_period_space_usage,omitempty"`
	CurrentPeriodUserUsage      int   `json:"current_period_user_usage,omitempty"`
	PromotionalUsageKnown       bool  `json:"promotional_usage_known"`
	PromotionalUsage            int   `json:"promotional_usage,omitempty"`
	PromotionalLimitKnown       bool  `json:"promotional_limit_known"`
	PromotionalLimit            int   `json:"promotional_limit,omitempty"`
	PremiumCreditBalance        int   `json:"premium_credit_balance,omitempty"`
	PremiumCreditKnown          bool  `json:"premium_credit_known,omitempty"`
	CreditsInOverage            int   `json:"credits_in_overage,omitempty"`
	OverageLimit                int   `json:"overage_limit,omitempty"`
	PremiumServicePeriodStartMs int64 `json:"premium_service_period_start_ms,omitempty"`
	LastUsageAtMs               int64 `json:"last_usage_at_ms,omitempty"`

	usageFromV2  bool `json:"-"`
	limitsFromV2 bool `json:"-"`
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
				SpaceUsage *int `json:"spaceUsage"`
				UserUsage  *int `json:"userUsage"`
			} `json:"currentServicePeriod"`
			Lifetime *struct {
				SpaceUsage           *int `json:"spaceUsage"`
				UserUsage            *int `json:"userUsage"`
				UserPromotionalUsage *int `json:"userPromotionalUsage"`
			} `json:"lifetime"`
			TotalCreditBalance *int  `json:"totalCreditBalance"`
			CreditsInOverage   *int  `json:"creditsInOverage"`
			LastSpaceUsageAtMs int64 `json:"lastSpaceUsageAtMs"`
		} `json:"usage"`
		Limits *struct {
			Free *struct {
				SpaceLimit           *int `json:"spaceLimit"`
				UserLimit            *int `json:"userLimit"`
				UserPromotionalLimit *int `json:"userPromotionalLimit"`
			} `json:"free"`
		} `json:"limits"`
		BasicCredits *struct {
			SpaceUsage           *int `json:"spaceUsage"`
			SpaceLimit           *int `json:"spaceLimit"`
			UserUsage            *int `json:"userUsage"`
			UserLimit            *int `json:"userLimit"`
			UserPromotionalUsage *int `json:"userPromotionalUsage"`
			UserPromotionalLimit *int `json:"userPromotionalLimit"`
		} `json:"basicCredits"`
		PremiumCredits *struct {
			TotalCreditBalance   *int  `json:"totalCreditBalance"`
			CreditsInOverage     *int  `json:"creditsInOverage"`
			OverageLimit         *int  `json:"overageLimit"`
			ServicePeriodStartMs int64 `json:"servicePeriodStartMs"`
		} `json:"premiumCredits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode ai usage v2: %w", err)
	}
	if basic := payload.BasicCredits; basic != nil {
		if basic.SpaceUsage != nil && basic.UserUsage != nil {
			out.SpaceUsage, out.UserUsage = *basic.SpaceUsage, *basic.UserUsage
			out.usageFromV2, out.BasicUsageKnown = true, true
		}
		if basic.SpaceLimit != nil && basic.UserLimit != nil {
			out.SpaceLimit, out.UserLimit = *basic.SpaceLimit, *basic.UserLimit
			out.limitsFromV2, out.BasicLimitsKnown = true, true
		}
		if basic.UserPromotionalUsage != nil {
			out.PromotionalUsage, out.PromotionalUsageKnown = *basic.UserPromotionalUsage, true
		}
		if basic.UserPromotionalLimit != nil {
			out.PromotionalLimit, out.PromotionalLimitKnown = *basic.UserPromotionalLimit, true
		}
	}
	if payload.Limits != nil && payload.Limits.Free != nil {
		free := payload.Limits.Free
		if !out.limitsFromV2 && free.SpaceLimit != nil && free.UserLimit != nil {
			out.SpaceLimit, out.UserLimit = *free.SpaceLimit, *free.UserLimit
			out.limitsFromV2, out.BasicLimitsKnown = true, true
		}
		if !out.PromotionalLimitKnown && free.UserPromotionalLimit != nil {
			out.PromotionalLimit, out.PromotionalLimitKnown = *free.UserPromotionalLimit, true
		}
	}
	if payload.Usage != nil && payload.Usage.CurrentServicePeriod != nil {
		period := payload.Usage.CurrentServicePeriod
		if period.SpaceUsage != nil && period.UserUsage != nil {
			out.CurrentPeriodSpaceUsage, out.CurrentPeriodUserUsage = *period.SpaceUsage, *period.UserUsage
			out.CurrentPeriodUsageKnown = true
		}
	}
	if payload.Usage != nil && payload.Usage.Lifetime != nil {
		lifetime := payload.Usage.Lifetime
		if !out.usageFromV2 && lifetime.SpaceUsage != nil && lifetime.UserUsage != nil {
			out.SpaceUsage, out.UserUsage = *lifetime.SpaceUsage, *lifetime.UserUsage
			out.usageFromV2, out.BasicUsageKnown = true, true
		}
		if !out.PromotionalUsageKnown && lifetime.UserPromotionalUsage != nil {
			out.PromotionalUsage, out.PromotionalUsageKnown = *lifetime.UserPromotionalUsage, true
		}
	}
	if payload.PremiumCredits != nil && payload.PremiumCredits.TotalCreditBalance != nil {
		out.PremiumCreditBalance = *payload.PremiumCredits.TotalCreditBalance
		out.PremiumCreditKnown = true
	} else if payload.Usage != nil && payload.Usage.TotalCreditBalance != nil {
		out.PremiumCreditBalance = *payload.Usage.TotalCreditBalance
		out.PremiumCreditKnown = true
	}
	if payload.PremiumCredits != nil {
		if payload.PremiumCredits.CreditsInOverage != nil {
			out.CreditsInOverage = *payload.PremiumCredits.CreditsInOverage
		} else if payload.Usage != nil && payload.Usage.CreditsInOverage != nil {
			out.CreditsInOverage = *payload.Usage.CreditsInOverage
		}
		if payload.PremiumCredits.OverageLimit != nil {
			out.OverageLimit = *payload.PremiumCredits.OverageLimit
		}
		out.PremiumServicePeriodStartMs = payload.PremiumCredits.ServicePeriodStartMs
	} else if payload.Usage != nil && payload.Usage.CreditsInOverage != nil {
		out.CreditsInOverage = *payload.Usage.CreditsInOverage
	}
	if payload.Usage != nil && payload.Usage.LastSpaceUsageAtMs > 0 {
		out.LastUsageAtMs = payload.Usage.LastSpaceUsageAtMs
	}
	if payload.BasicCredits == nil && payload.Usage == nil && payload.PremiumCredits == nil {
		return fmt.Errorf("decode ai usage v2: no recognized usage fields")
	}
	return nil
}

func parseAIUsageEligibilityV1(body []byte, out *workspaceAIUsage) error {
	var payload struct {
		IsEligible           *bool  `json:"isEligible"`
		Type                 string `json:"type"`
		SpaceUsage           *int   `json:"spaceUsage"`
		SpaceLimit           *int   `json:"spaceLimit"`
		UserUsage            *int   `json:"userUsage"`
		UserLimit            *int   `json:"userLimit"`
		LastSpaceUsageAtMs   int64  `json:"lastSpaceUsageAtMs"`
		UserPromotionalUsage *int   `json:"userPromotionalUsage"`
		UserPromotionalLimit *int   `json:"userPromotionalLimit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode ai usage v1: %w", err)
	}
	if payload.IsEligible != nil {
		out.IsEligible = *payload.IsEligible
		out.IsEligibleKnown = true
	}
	out.Type = strings.TrimSpace(payload.Type)
	out.QuotaEnforced = out.Type != "" && !strings.EqualFold(out.Type, "unlimited")
	if !out.usageFromV2 && payload.SpaceUsage != nil && payload.UserUsage != nil {
		out.SpaceUsage = *payload.SpaceUsage
		out.UserUsage = *payload.UserUsage
		out.BasicUsageKnown = true
	}
	if !out.limitsFromV2 && payload.SpaceLimit != nil && payload.UserLimit != nil {
		out.SpaceLimit = *payload.SpaceLimit
		out.UserLimit = *payload.UserLimit
		out.BasicLimitsKnown = true
	}
	if !out.PromotionalUsageKnown && payload.UserPromotionalUsage != nil {
		out.PromotionalUsage, out.PromotionalUsageKnown = *payload.UserPromotionalUsage, true
	}
	if !out.PromotionalLimitKnown && payload.UserPromotionalLimit != nil {
		out.PromotionalLimit, out.PromotionalLimitKnown = *payload.UserPromotionalLimit, true
	}
	if out.LastUsageAtMs == 0 {
		out.LastUsageAtMs = payload.LastSpaceUsageAtMs
	}
	if payload.IsEligible == nil && payload.Type == "" && payload.SpaceUsage == nil && payload.UserUsage == nil {
		return fmt.Errorf("decode ai usage v1: no recognized eligibility fields")
	}
	return nil
}
