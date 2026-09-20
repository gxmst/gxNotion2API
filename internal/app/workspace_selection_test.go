package app

import "testing"

// Notion bills AI per workspace, so which space an account points at decides
// whether inference works at all: a free workspace whose trial allowance is
// spent accepts the message and then never produces an answer. These tests pin
// the two places that used to move an account into the wrong workspace.

func TestResolveRefreshedSpaceKeepsOperatorChoice(t *testing.T) {
	account := NotionAccount{
		SpaceID:     "paid-space",
		SpaceViewID: "paid-view",
	}
	prior := SessionInfo{SpaceID: "prior-space", SpaceViewID: "prior-view"}
	// getSpacesInitial reports whichever space sits first in space_view_pointers,
	// which for a multi-workspace account is normally the free personal one.
	spaces := loginSpaceBootstrap{SpaceID: "free-space", SpaceViewID: "free-view"}

	spaceID, viewID := resolveRefreshedSpace(account, prior, spaces)
	if spaceID != "paid-space" {
		t.Fatalf("refresh moved the account out of the configured space: got %q", spaceID)
	}
	if viewID != "paid-view" {
		t.Fatalf("space view id did not travel with the space id: got %q", viewID)
	}
}

func TestResolveRefreshedSpaceFallsBackWhenUnset(t *testing.T) {
	spaces := loginSpaceBootstrap{SpaceID: "discovered", SpaceViewID: "discovered-view"}

	spaceID, viewID := resolveRefreshedSpace(NotionAccount{}, SessionInfo{}, spaces)
	if spaceID != "discovered" || viewID != "discovered-view" {
		t.Fatalf("first import should accept discovery: got %q/%q", spaceID, viewID)
	}

	spaceID, viewID = resolveRefreshedSpace(
		NotionAccount{},
		SessionInfo{SpaceID: "prior", SpaceViewID: "prior-view"},
		spaces,
	)
	if spaceID != "prior" || viewID != "prior-view" {
		t.Fatalf("prior session should outrank rediscovery: got %q/%q", spaceID, viewID)
	}
}

func TestResolveRefreshedSpaceNeverMixesIDAndView(t *testing.T) {
	// An account carrying an id but no view id must not borrow the view id of a
	// different space; a mismatched pair is worse than a missing view id.
	spaceID, viewID := resolveRefreshedSpace(
		NotionAccount{SpaceID: "chosen"},
		SessionInfo{SpaceID: "other", SpaceViewID: "other-view"},
		loginSpaceBootstrap{SpaceID: "free", SpaceViewID: "free-view"},
	)
	if spaceID != "chosen" {
		t.Fatalf("expected chosen space, got %q", spaceID)
	}
	if viewID != "" {
		t.Fatalf("borrowed a foreign space view id: %q", viewID)
	}
}

func TestPaidSubscriptionTier(t *testing.T) {
	for tier, want := range map[string]bool{
		"":           false,
		"free":       false,
		"FREE":       false,
		"trial":      false,
		"business":   true,
		"Business":   true,
		"enterprise": true,
		"plus":       false,
	} {
		if got := paidSubscriptionTier(tier); got != want {
			t.Errorf("paidSubscriptionTier(%q) = %v, want %v", tier, got, want)
		}
	}
}

// spaceRecordMap builds the loadUserContent recordMap shape: spaces keyed by id
// plus a user_root whose space_view_pointers give the client's ordering.
func spaceRecordMap(userID string, spaces []discoveredSpaceCandidate) map[string]any {
	spaceRecords := map[string]any{}
	pointers := []any{}
	for _, space := range spaces {
		spaceRecords[space.ID] = map[string]any{
			"value": map[string]any{
				"id":                space.ID,
				"name":              space.Name,
				"plan_type":         space.PlanType,
				"subscription_tier": space.SubscriptionTier,
			},
		}
		pointers = append(pointers, map[string]any{
			"spaceId": space.ID,
			"id":      space.ViewID,
		})
	}
	return map[string]any{
		"notion_user": map[string]any{
			userID: map[string]any{"value": map[string]any{"email": "user@example.com"}},
		},
		"space": spaceRecords,
		"user_root": map[string]any{
			userID: map[string]any{"value": map[string]any{"space_view_pointers": pointers}},
		},
	}
}

func TestChooseBestSpacePrefersPaidTierOverFirstListed(t *testing.T) {
	const userID = "user-1"
	// Real payload ordering: the free personal workspace comes first. Its
	// plan_type is "personal" rather than "free", so a plan-based test scored it
	// as highly as the business workspace and the tie went to whatever came first.
	recordMap := spaceRecordMap(userID, []discoveredSpaceCandidate{
		{ID: "free-personal", ViewID: "view-free", Name: "Personal", PlanType: "personal", SubscriptionTier: "free"},
		{ID: "free-team", ViewID: "view-team-free", Name: "Team", PlanType: "team", SubscriptionTier: "free"},
		{ID: "paid-team", ViewID: "view-paid", Name: "Team", PlanType: "team", SubscriptionTier: "business"},
	})

	best := chooseBestSpace(recordMap, userID)
	if best.ID != "paid-team" {
		t.Fatalf("expected the business workspace, got %q", best.ID)
	}
	if best.ViewID != "view-paid" {
		t.Fatalf("expected the matching view id, got %q", best.ViewID)
	}
}

func TestParseLoadUserContentMetadataReportsChosenSpace(t *testing.T) {
	const userID = "user-1"
	payload := map[string]any{
		"recordMap": spaceRecordMap(userID, []discoveredSpaceCandidate{
			{ID: "free-personal", ViewID: "view-free", Name: "Personal", PlanType: "personal", SubscriptionTier: "free"},
			{ID: "paid-team", ViewID: "view-paid", Name: "Team", PlanType: "team", SubscriptionTier: "business"},
		}),
	}

	meta := parseLoadUserContentMetadata(payload)
	if meta.SpaceID != "paid-team" || meta.SpaceViewID != "view-paid" {
		t.Fatalf("discovery picked %q/%q, want paid-team/view-paid", meta.SpaceID, meta.SpaceViewID)
	}
	if meta.UserID != userID {
		t.Fatalf("user id = %q", meta.UserID)
	}
}

func TestChooseBestSpaceSkipsAIDisabledWorkspace(t *testing.T) {
	const userID = "user-1"
	recordMap := spaceRecordMap(userID, []discoveredSpaceCandidate{
		{ID: "paid-no-ai", ViewID: "view-a", Name: "A", PlanType: "team", SubscriptionTier: "business"},
		{ID: "paid-ai", ViewID: "view-b", Name: "B", PlanType: "team", SubscriptionTier: "business"},
	})
	spaces := recordMap["space"].(map[string]any)
	disabled := spaces["paid-no-ai"].(map[string]any)["value"].(map[string]any)
	disabled["settings"] = map[string]any{"disable_ai_feature": true}

	if best := chooseBestSpace(recordMap, userID); best.ID != "paid-ai" {
		t.Fatalf("expected the AI-enabled workspace, got %q", best.ID)
	}
}
