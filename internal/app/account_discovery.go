package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type discoveredAccountMetadata struct {
	Email         string
	UserID        string
	UserName      string
	SpaceID       string
	SpaceViewID   string
	SpaceName     string
	PlanType      string
	Workspaces    []discoveredSpaceCandidate
	ClientVersion string
	Models        []ModelDefinition
}

type discoveredSpaceCandidate struct {
	ID               string
	ViewID           string
	Name             string
	PlanType         string
	SubscriptionTier string
	AIEnabled        bool
	AIDisabled       bool
}

// paidSubscriptionTier reports whether a workspace carries a subscription that
// comes with AI credit.
//
// plan_type is the wrong field to judge this by: a free personal workspace
// reports plan_type "personal", not "free", so a "plan is not free" test scores
// it exactly as high as a paid team workspace. subscription_tier is the field
// that actually separates them ("free" vs "business"/"enterprise"/"plus").
func paidSubscriptionTier(tier string) bool {
	return commercialWorkspaceTier(tier, "")
}

func unwrapRecordValue(raw any) map[string]any {
	node := mapValue(raw)
	if node == nil {
		return nil
	}
	if inner := mapValue(node["value"]); inner != nil {
		if nested := mapValue(inner["value"]); nested != nil {
			return nested
		}
		return inner
	}
	return node
}

func boolValue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func discoverSpaceCandidates(recordMap map[string]any, userID string) []discoveredSpaceCandidate {
	if recordMap == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	userRoots := mapValue(recordMap["user_root"])
	spaces := mapValue(recordMap["space"])
	if userRoots == nil || spaces == nil {
		return nil
	}
	root := unwrapRecordValue(userRoots[userID])
	if root == nil {
		return nil
	}
	pointers := sliceValue(root["space_view_pointers"])
	candidates := make([]discoveredSpaceCandidate, 0, len(pointers))
	for _, rawPointer := range pointers {
		pointer := mapValue(rawPointer)
		spaceID := strings.TrimSpace(stringValue(pointer["spaceId"]))
		if spaceID == "" {
			continue
		}
		value := unwrapRecordValue(spaces[spaceID])
		if value == nil {
			continue
		}
		settings := mapValue(value["settings"])
		disabledAI := boolValue(settings["disable_ai_feature"])
		enabledAI := boolValue(settings["enable_ai_feature"])
		candidate := discoveredSpaceCandidate{
			ID:               firstNonEmpty(strings.TrimSpace(stringValue(value["id"])), spaceID),
			ViewID:           strings.TrimSpace(stringValue(pointer["id"])),
			Name:             strings.TrimSpace(stringValue(value["name"])),
			PlanType:         strings.TrimSpace(stringValue(value["plan_type"])),
			SubscriptionTier: strings.TrimSpace(stringValue(value["subscription_tier"])),
			AIEnabled:        !disabledAI && (enabledAI || settings["enable_ai_feature"] == nil),
			AIDisabled:       disabledAI,
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func spaceCandidateScore(candidate discoveredSpaceCandidate) int {
	if ok, _ := workspaceEligibility(NotionWorkspace{SubscriptionTier: candidate.SubscriptionTier, PlanType: candidate.PlanType, AIDisabled: candidate.AIDisabled}); !ok {
		return -1
	}
	// A paid subscription outweighs everything else: AI quota is billed per
	// workspace, and a free workspace accepts messages then silently never
	// answers once its trial allowance is gone. Weight it above AIEnabled,
	// which is true for free workspaces too.
	score := 0
	if paidSubscriptionTier(candidate.SubscriptionTier) {
		score += 4
	}
	if candidate.AIEnabled {
		score += 2
	}
	if plan := strings.ToLower(candidate.PlanType); plan != "" && plan != "personal" {
		score++
	}
	if candidate.Name != "" {
		score++
	}
	return score
}

func chooseBestSpace(recordMap map[string]any, userID string) discoveredSpaceCandidate {
	best := discoveredSpaceCandidate{}
	bestScore := -1
	for _, candidate := range discoverSpaceCandidates(recordMap, userID) {
		if score := spaceCandidateScore(candidate); score > bestScore {
			best = candidate
			bestScore = score
		}
	}
	return best
}

func parseLoadUserContentMetadata(payload map[string]any) discoveredAccountMetadata {
	return parseLoadUserContentMetadataForUser(payload, "")
}

func parseLoadUserContentMetadataForUser(payload map[string]any, activeUserID string) discoveredAccountMetadata {
	recordMap := mapValue(payload["recordMap"])
	if recordMap == nil {
		return discoveredAccountMetadata{}
	}
	users := mapValue(recordMap["notion_user"])
	var meta discoveredAccountMetadata
	for userID, rawUser := range users {
		if activeUserID != "" && userID != activeUserID {
			continue
		}
		value := unwrapRecordValue(rawUser)
		if value == nil {
			continue
		}
		meta.UserID = strings.TrimSpace(userID)
		meta.Email = strings.TrimSpace(stringValue(value["email"]))
		meta.UserName = strings.TrimSpace(stringValue(value["name"]))
		break
	}
	meta.Workspaces = discoverSpaceCandidates(recordMap, meta.UserID)
	space := discoveredSpaceCandidate{}
	bestScore := -1
	for _, candidate := range meta.Workspaces {
		if score := spaceCandidateScore(candidate); score > bestScore {
			space = candidate
			bestScore = score
		}
	}
	meta.SpaceID = space.ID
	meta.SpaceViewID = space.ViewID
	meta.SpaceName = space.Name
	meta.PlanType = space.PlanType
	return meta
}

func fetchLoadUserContentMetadata(ctx context.Context, session *loginHTTPSession, upstream NotionUpstream, clientVersion string, activeUserID string) (discoveredAccountMetadata, error) {
	payload, err := postNotionLoginJSON(ctx, session, upstream, upstream.API("loadUserContent"), clientVersion, upstream.HomeURL(), activeUserID, map[string]any{})
	if err != nil {
		return discoveredAccountMetadata{}, err
	}
	meta := parseLoadUserContentMetadataForUser(payload, activeUserID)
	if meta.UserID == "" && meta.Email == "" && meta.SpaceID == "" {
		return discoveredAccountMetadata{}, fmt.Errorf("loadUserContent returned no account metadata")
	}
	return meta, nil
}

func fetchAvailableModelsMetadata(ctx context.Context, session *loginHTTPSession, upstream NotionUpstream, clientVersion string, activeUserID string, spaceID string) ([]ModelDefinition, error) {
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil, fmt.Errorf("getAvailableModels requires a space_id")
	}
	body := map[string]any{"spaceId": spaceID}
	payload, err := postNotionLoginJSON(ctx, session, upstream, upstream.API("getAvailableModels"), clientVersion, upstream.HomeURL(), activeUserID, body)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return parseProbeModelsBlob(string(raw)), nil
}

// refreshAccountModels re-asks upstream for the model list of an already-imported
// account. Full discovery only runs at import time and is skipped entirely when
// the pasted probe JSON is complete, so without this the model list can only be
// updated by re-importing the account.
func refreshAccountModels(ctx context.Context, cfg AppConfig, accountEmail string, cookies []ProbeCookie, clientVersion string, userID string, spaceID string) ([]ModelDefinition, error) {
	cookies = normalizeProbeCookies(cookies)
	if len(cookies) == 0 {
		return nil, fmt.Errorf("cookies are required to refresh models")
	}
	if strings.TrimSpace(spaceID) == "" {
		return nil, fmt.Errorf("space_id is required to refresh models")
	}
	upstream := cfg.NotionUpstream()
	resolver := NewProxyResolver(cfg)
	session, err := newNotionLoginSession(helperTimeout(cfg), upstream, resolver, accountEmail, cfg)
	if err != nil {
		return nil, err
	}
	restoreProbeCookies(session.Jar, upstream.HomeURL(), cookies)
	restoreProbeCookies(session.Jar, upstream.LoginURL(), cookies)

	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		bootstrap, bootErr := fetchLoginBootstrap(ctx, session, upstream)
		if bootErr != nil {
			return nil, bootErr
		}
		clientVersion = strings.TrimSpace(bootstrap.ClientVersion)
	}
	if clientVersion == "" {
		return nil, fmt.Errorf("client_version unavailable")
	}

	lookupUserID := firstNonEmpty(strings.TrimSpace(userID), probeCookieValue(cookies, "notion_user_id"))
	models, err := fetchAvailableModelsMetadata(ctx, session, upstream, clientVersion, lookupUserID, spaceID)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("upstream returned no models")
	}
	return models, nil
}

func discoverImportedAccountMetadata(ctx context.Context, cfg AppConfig, accountEmail string, cookies []ProbeCookie, fallback discoveredAccountMetadata) (discoveredAccountMetadata, error) {
	meta := fallback
	cookies = normalizeProbeCookies(cookies)
	if len(cookies) == 0 {
		return meta, fmt.Errorf("cookies are required for auto-discovery")
	}
	upstream := cfg.NotionUpstream()
	resolver := NewProxyResolver(cfg)
	session, err := newNotionLoginSession(helperTimeout(cfg), upstream, resolver, accountEmail, cfg)
	if err != nil {
		return meta, err
	}
	restoreProbeCookies(session.Jar, upstream.HomeURL(), cookies)
	restoreProbeCookies(session.Jar, upstream.LoginURL(), cookies)

	if strings.TrimSpace(meta.ClientVersion) == "" {
		bootstrap, err := fetchLoginBootstrap(ctx, session, upstream)
		if err != nil {
			return meta, err
		}
		meta.ClientVersion = strings.TrimSpace(bootstrap.ClientVersion)
	}

	lookupUserID := firstNonEmpty(
		meta.UserID,
		probeCookieValue(cookies, "notion_user_id"),
		probeCookieValue(probeCookiesFromJar(session.Jar, upstream.HomeURL()), "notion_user_id"),
		probeCookieValue(probeCookiesFromJar(session.Jar, upstream.LoginURL()), "notion_user_id"),
	)

	var primaryErr error
	if strings.TrimSpace(meta.ClientVersion) != "" {
		discovered, err := fetchLoadUserContentMetadata(ctx, session, upstream, meta.ClientVersion, lookupUserID)
		if err == nil {
			meta.Email = firstNonEmpty(meta.Email, discovered.Email)
			meta.UserID = firstNonEmpty(meta.UserID, discovered.UserID)
			meta.UserName = firstNonEmpty(meta.UserName, discovered.UserName)
			if len(discovered.Workspaces) > 0 {
				meta.Workspaces = discovered.Workspaces
			}
			// The view id has to come from the same space as the id it accompanies,
			// so adopt the pair together or not at all.
			if strings.TrimSpace(meta.SpaceID) == "" {
				meta.SpaceID = discovered.SpaceID
				meta.SpaceViewID = discovered.SpaceViewID
			}
			meta.SpaceName = firstNonEmpty(meta.SpaceName, discovered.SpaceName)
			meta.PlanType = firstNonEmpty(meta.PlanType, discovered.PlanType)
			lookupUserID = firstNonEmpty(meta.UserID, lookupUserID)
		} else {
			primaryErr = err
		}
	}

	if strings.TrimSpace(meta.UserID) == "" || strings.TrimSpace(meta.SpaceID) == "" || strings.TrimSpace(meta.Email) == "" {
		if strings.TrimSpace(meta.ClientVersion) != "" && strings.TrimSpace(lookupUserID) != "" {
			bootstrap, err := getSpacesInitial(ctx, session, upstream, meta.ClientVersion, lookupUserID)
			if err == nil {
				meta.UserID = firstNonEmpty(meta.UserID, lookupUserID)
				meta.Email = firstNonEmpty(meta.Email, bootstrap.Email)
				meta.UserName = firstNonEmpty(meta.UserName, bootstrap.UserName)
				if strings.TrimSpace(meta.SpaceID) == "" {
					meta.SpaceID = bootstrap.SpaceID
					meta.SpaceViewID = bootstrap.SpaceViewID
				}
				if len(meta.Workspaces) == 0 && bootstrap.SpaceID != "" {
					meta.Workspaces = []discoveredSpaceCandidate{{ID: bootstrap.SpaceID, ViewID: bootstrap.SpaceViewID, Name: meta.SpaceName}}
				}
			} else if primaryErr == nil {
				primaryErr = err
			}
		}
	}

	if strings.TrimSpace(meta.ClientVersion) != "" {
		if models, err := fetchAvailableModelsMetadata(ctx, session, upstream, meta.ClientVersion, firstNonEmpty(meta.UserID, lookupUserID), meta.SpaceID); err == nil {
			meta.Models = models
		}
	}

	if strings.TrimSpace(meta.Email) == "" || strings.TrimSpace(meta.UserID) == "" || strings.TrimSpace(meta.SpaceID) == "" || strings.TrimSpace(meta.ClientVersion) == "" {
		if primaryErr != nil {
			return meta, fmt.Errorf("auto-discovery incomplete: %w", primaryErr)
		}
		return meta, fmt.Errorf("auto-discovery incomplete: missing email, user_id, space_id, or client_version")
	}
	return meta, nil
}
