package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// This endpoint is only used by explicit admin actions. Inference and
// background refresh never write workspace model policies.
type workspaceModelPolicy struct {
	DisabledModels    []string `json:"disabledModels"`
	DisabledProviders []string `json:"disabledProviders"`
}

type policyCatalogModel struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Available      bool   `json:"available"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	Allowed        bool   `json:"allowed"`
}

type workspaceModelPolicySnapshot struct {
	Email          string               `json:"email"`
	WorkspaceID    string               `json:"workspace_id"`
	Scope          string               `json:"scope"`
	MembershipType string               `json:"membership_type"`
	CanEdit        bool                 `json:"can_edit"`
	Policy         workspaceModelPolicy `json:"policy"`
	PolicyPresent  bool                 `json:"policy_present"`
	Revision       string               `json:"revision"`
	Models         []policyCatalogModel `json:"models"`
}

type modelPolicyEdit struct {
	Email          string                `json:"email"`
	WorkspaceID    string                `json:"workspace_id"`
	Scope          string                `json:"scope"`
	Revision       string                `json:"revision"`
	Action         string                `json:"action"`
	ModelID        string                `json:"model_id"`
	RestorePolicy  *workspaceModelPolicy `json:"restore_policy"`
	RestorePresent bool                  `json:"restore_present"`
}

func modelPolicyKey(scope string) (string, bool) {
	switch scope {
	case "personal":
		return "personal_agent_model_policy", true
	case "custom":
		return "custom_agent_model_policy", true
	default:
		return "", false
	}
}

func canonicalPolicyList(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeModelPolicy(policy workspaceModelPolicy) workspaceModelPolicy {
	return workspaceModelPolicy{canonicalPolicyList(policy.DisabledModels), canonicalPolicyList(policy.DisabledProviders)}
}

func modelPolicyRevision(scope string, policy workspaceModelPolicy, present bool) string {
	body, _ := json.Marshal(struct {
		Scope   string
		Present bool
		Policy  workspaceModelPolicy
	}{scope, present, normalizeModelPolicy(policy)})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func parseModelPolicyRecord(records map[string]any, workspaceID, scope string) (workspaceModelPolicy, bool, error) {
	key, _ := modelPolicyKey(scope)
	value := unwrapRecordValue(mapValue(records["space"])[workspaceID])
	settings := mapValue(value["settings"])
	if stringValue(value["id"]) != workspaceID || settings == nil {
		return workspaceModelPolicy{}, false, errors.New("upstream workspace settings are incomplete")
	}
	raw, present := settings[key]
	if !present {
		return normalizeModelPolicy(workspaceModelPolicy{}), false, nil
	}
	data, _ := json.Marshal(raw)
	var policy workspaceModelPolicy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if mapValue(raw) == nil || decoder.Decode(&policy) != nil {
		return policy, false, errors.New("upstream model policy has an unsupported format")
	}
	return normalizeModelPolicy(policy), true, nil
}

func (c *NotionAIClient) readModelPolicyRecords(ctx context.Context) (map[string]any, error) {
	// Read the current user's pointers, rather than relying on a stale probe's
	// first workspace or inferring ownership from the space's creator.
	body, err := c.postJSON(ctx, c.Config.NotionUpstream().API("getSpacesInitial"), map[string]any{}, "application/json")
	if err != nil {
		return nil, err
	}
	var initial map[string]any
	if err := json.Unmarshal(body, &initial); err != nil {
		return nil, err
	}
	user := mapValue(mapValue(initial["users"])[c.Session.UserID])
	root := unwrapRecordValue(mapValue(user["user_root"])[c.Session.UserID])
	viewID := ""
	for _, raw := range sliceValue(root["space_view_pointers"]) {
		pointer := mapValue(raw)
		if stringValue(pointer["spaceId"]) == c.Session.SpaceID {
			viewID = stringValue(pointer["id"])
			break
		}
	}
	if viewID == "" {
		return nil, errors.New("当前账号无法访问此工作区，请刷新工作区列表")
	}
	body, err = c.postJSON(ctx, c.Config.NotionUpstream().API("getSpacesFanout"), map[string]any{
		"users": map[string]any{c.Session.UserID: []any{map[string]any{"id": viewID, "table": "space_view", "spaceId": c.Session.SpaceID}}},
		"depth": 0,
	}, "application/json")
	if err != nil {
		return nil, err
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	return mapValue(mapValue(response["users"])[c.Session.UserID]), nil
}

func (c *NotionAIClient) readWorkspaceModelPolicy(ctx context.Context, scope string) (workspaceModelPolicySnapshot, error) {
	out := workspaceModelPolicySnapshot{Email: c.AccountEmail, WorkspaceID: c.Session.SpaceID, Scope: scope, MembershipType: "unknown"}
	records, err := c.readModelPolicyRecords(ctx)
	if err != nil {
		return out, err
	}
	for _, raw := range mapValue(records["space_user"]) {
		member := unwrapRecordValue(raw)
		if stringValue(member["user_id"]) == c.Session.UserID && stringValue(member["space_id"]) == c.Session.SpaceID {
			out.MembershipType = firstNonEmpty(stringValue(member["membership_type"]), "unknown")
			break
		}
	}
	out.CanEdit = out.MembershipType == "owner"
	out.Policy, out.PolicyPresent, err = parseModelPolicyRecord(records, c.Session.SpaceID, scope)
	if err != nil {
		return out, err
	}
	out.Revision = modelPolicyRevision(scope, out.Policy, out.PolicyPresent)
	body, err := c.postJSON(ctx, c.Config.NotionUpstream().API("getAvailableModels"), map[string]any{"spaceId": c.Session.SpaceID, "surface": "workspace_model_settings"}, "application/json")
	if err != nil {
		return out, err
	}
	var catalog struct {
		Models []struct {
			Model    string `json:"model"`
			Name     string `json:"modelMessage"`
			Provider string `json:"modelProvider"`
			Disabled bool   `json:"isDisabled"`
			Reason   string `json:"disabledReason"`
			Workflow struct {
				Disabled bool   `json:"isDisabled"`
				Reason   string `json:"disabledReason"`
			} `json:"workflow"`
			CustomAgent struct {
				Disabled bool   `json:"isDisabled"`
				Reason   string `json:"disabledReason"`
			} `json:"customAgent"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return out, err
	}
	if len(catalog.Models) == 0 {
		return out, errors.New("上游未返回模型设置目录，请稍后重新读取")
	}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		id, provider := strings.TrimSpace(model.Model), strings.TrimSpace(model.Provider)
		if id == "" || seen[id] {
			return out, errors.New("上游模型设置目录缺少模型标识或存在重复标识，请重新读取")
		}
		seen[id] = true
		disabled, reason := model.Workflow.Disabled, model.Workflow.Reason
		if scope == "custom" {
			disabled, reason = model.CustomAgent.Disabled, model.CustomAgent.Reason
		}
		available := !model.Disabled && !disabled && provider != ""
		out.Models = append(out.Models, policyCatalogModel{ID: id, Name: firstNonEmpty(model.Name, id), Provider: provider, Available: available, DisabledReason: firstNonEmpty(reason, model.Reason)})
	}
	updatePolicyAllowedModels(&out)
	return out, nil
}

func policyContains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func updatePolicyAllowedModels(snapshot *workspaceModelPolicySnapshot) {
	for i := range snapshot.Models {
		model := &snapshot.Models[i]
		model.Allowed = model.Available && !policyContains(snapshot.Policy.DisabledModels, model.ID) && !policyContains(snapshot.Policy.DisabledProviders, model.Provider)
	}
}

func lockWorkspaceModel(snapshot workspaceModelPolicySnapshot, id string) (workspaceModelPolicy, error) {
	var target *policyCatalogModel
	for i := range snapshot.Models {
		if snapshot.Models[i].ID == id {
			target = &snapshot.Models[i]
			break
		}
	}
	if target == nil || !target.Available {
		return workspaceModelPolicy{}, errors.New("此模型当前不可用，不能通过工作区设置解除套餐限制")
	}
	policy := normalizeModelPolicy(snapshot.Policy)
	for _, model := range snapshot.Models {
		if model.ID != id {
			policy.DisabledModels = append(policy.DisabledModels, model.ID)
		}
		if model.Provider != "" && model.Provider != target.Provider {
			policy.DisabledProviders = append(policy.DisabledProviders, model.Provider)
		}
	}
	remove := func(values []string, value string) []string {
		out := []string{}
		for _, item := range values {
			if item != value {
				out = append(out, item)
			}
		}
		return out
	}
	policy.DisabledModels = remove(policy.DisabledModels, id)
	policy.DisabledProviders = remove(policy.DisabledProviders, target.Provider)
	return normalizeModelPolicy(policy), nil
}

func (a *App) writeModelPolicyUpstreamError(w http.ResponseWriter, started NotionAccount, err error, writing bool) {
	if until := credentialBackoff(err, time.Now()); !until.IsZero() {
		a.State.refreshMu.Lock()
		cfg, _, _ := a.State.Snapshot()
		current, index, ok := cfg.FindAccount(started.Email)
		if ok && workspaceDiscoveryIdentityUnchanged(started, current) && until.After(parseOptionalRFC3339(current.CredentialCooldownUntil)) {
			cfg.Accounts = cloneAccounts(cfg.Accounts)
			cfg.Accounts[index].CredentialCooldownUntil = until.UTC().Format(time.RFC3339)
			if saveErr := a.State.saveAndApplyCommitted(cfg); saveErr != nil {
				log.Printf("[model-policy] failed to save credential cooldown: %v", saveErr)
			}
		}
		a.State.refreshMu.Unlock()
	}
	var apiErr *notionAPIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
			writeJSON(w, http.StatusForbidden, map[string]any{"detail": "Notion 拒绝此操作，请确认当前账号登录有效且仍是工作区所有者；没有自动重试"})
			return
		}
		if apiErr.StatusCode == http.StatusTooManyRequests {
			a.writeUpstreamError(w, err)
			return
		}
	}
	message := "读取工作区模型设置失败，请稍后重试"
	if writing {
		message = "保存请求未能确认结果，请重新读取设置；没有自动重试"
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"detail": message})
}

func (a *App) handleAdminModelPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.adminAuthOK(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	request := modelPolicyEdit{Email: r.URL.Query().Get("email"), WorkspaceID: r.URL.Query().Get("workspace_id"), Scope: r.URL.Query().Get("scope")}
	if r.Method == http.MethodPut {
		payload, err := a.decodeBody(w, r)
		if err != nil {
			writeInvalidBodyError(w, err)
			return
		}
		body, _ := json.Marshal(payload)
		if err := json.Unmarshal(body, &request); err != nil {
			writeJSON(w, 400, map[string]any{"detail": "invalid model policy request"})
			return
		}
		// Serialize manual edits across accounts that share a workspace. The
		// upstream has no captured compare-and-swap API, so also re-read below.
		a.State.modelPolicyMu.Lock()
		defer a.State.modelPolicyMu.Unlock()
	}
	key, validScope := modelPolicyKey(request.Scope)
	if strings.TrimSpace(request.Email) == "" || strings.TrimSpace(request.WorkspaceID) == "" || !validScope {
		writeJSON(w, 400, map[string]any{"detail": "email、workspace_id 和 personal/custom scope 必须明确指定"})
		return
	}
	cfg, _, _ := a.State.Snapshot()
	account, _, ok := cfg.FindAccountWorkspace(request.Email, request.WorkspaceID)
	if !ok {
		writeJSON(w, 404, map[string]any{"detail": "workspace not found"})
		return
	}
	if until := parseOptionalRFC3339(account.CredentialCooldownUntil); until.After(time.Now()) {
		w.Header().Set("Retry-After", until.UTC().Format(http.TimeFormat))
		writeJSON(w, 429, map[string]any{"detail": "账号正在冷却，请稍后再读取或修改设置"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client, err := a.notionClientForWorkspace(ctx, account.Email, request.WorkspaceID)
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": "无法加载账号登录态，请先检查账号"})
		return
	}
	// Even a transport failure after sending a settings write must not replay it.
	client.FallbackHTTPClient = nil
	policyHTTPClient := *client.HTTPClient
	policyHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.HTTPClient = &policyHTTPClient
	snapshot, err := client.readWorkspaceModelPolicy(ctx, request.Scope)
	if err != nil {
		a.writeModelPolicyUpstreamError(w, account, err, false)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, snapshot)
		return
	}
	if !snapshot.CanEdit {
		writeJSON(w, 403, map[string]any{"detail": "仅已确认的工作区所有者可修改模型设置；当前权限为 " + snapshot.MembershipType})
		return
	}
	if request.Revision == "" || request.Revision != snapshot.Revision {
		writeJSON(w, 409, map[string]any{"detail": "工作区模型设置已变化，请重新读取后再应用"})
		return
	}
	var policy workspaceModelPolicy
	present := true
	switch request.Action {
	case "lock":
		policy, err = lockWorkspaceModel(snapshot, request.ModelID)
	case "restore":
		present = request.RestorePresent
		if request.RestorePolicy == nil {
			err = errors.New("缺少恢复前的设置快照")
		} else {
			policy = normalizeModelPolicy(*request.RestorePolicy)
		}
		if !present {
			policy = normalizeModelPolicy(workspaceModelPolicy{})
		}
	default:
		err = errors.New("请选择手动应用或恢复设置")
	}
	if len(policy.DisabledModels) > 512 || len(policy.DisabledProviders) > 64 {
		err = errors.New("模型策略条目过多")
	}
	for _, value := range append(append([]string(nil), policy.DisabledModels...), policy.DisabledProviders...) {
		if len(value) > 200 || strings.ContainsAny(value, "\r\n\x00") {
			err = errors.New("无效的模型策略条目")
		}
	}
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": err.Error()})
		return
	}
	// Recheck local identity/cooldown after the upstream reads, before writing.
	live, _, _ := a.State.Snapshot()
	current, _, exists := live.FindAccountWorkspace(account.Email, request.WorkspaceID)
	if !exists || !workspaceDiscoveryIdentityUnchanged(account, current) || parseOptionalRFC3339(current.CredentialCooldownUntil).After(time.Now()) {
		writeJSON(w, 409, map[string]any{"detail": "账号状态已变化，请重新读取设置"})
		return
	}
	wanted := modelPolicyRevision(request.Scope, policy, present)
	if wanted == snapshot.Revision {
		writeJSON(w, 200, snapshot)
		return
	}
	patch, unset := map[string]any{}, []string{}
	if present {
		patch[key] = policy
	} else {
		unset = append(unset, key)
	}
	body, err := client.postJSON(ctx, cfg.NotionUpstream().API("updateSpaceSettings"), map[string]any{"spaceId": request.WorkspaceID, "settingsPatch": patch, "unsetSettingKeys": unset}, "application/json")
	if err != nil {
		a.writeModelPolicyUpstreamError(w, account, err, true)
		return
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		a.writeModelPolicyUpstreamError(w, account, err, true)
		return
	}
	confirmed, confirmedPresent, err := parseModelPolicyRecord(mapValue(response["recordMap"]), request.WorkspaceID, request.Scope)
	if err != nil || wanted != modelPolicyRevision(request.Scope, confirmed, confirmedPresent) {
		writeJSON(w, 502, map[string]any{"detail": "Notion 已接收保存请求，但返回的策略未确认一致；请重新读取设置，没有自动重试"})
		return
	}
	snapshot.Policy, snapshot.PolicyPresent, snapshot.Revision = confirmed, confirmedPresent, wanted
	updatePolicyAllowedModels(&snapshot)
	writeJSON(w, 200, snapshot)
}
