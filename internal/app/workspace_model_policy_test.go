package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type policyTestUpstream struct {
	app          *App
	role         string
	settings     map[string]any
	writes       []map[string]any
	updateStatus int
	unconfirmed  bool
	calls        int
	catalog      string
	// rawUpdateBody, when set, is returned verbatim from updateSpaceSettings so
	// a test can simulate an accepted write whose response cannot be parsed.
	rawUpdateBody string
}

func newPolicyTestUpstream(t *testing.T) *policyTestUpstream {
	t.Helper()
	fixture := &policyTestUpstream{app: newConversationRequestTestApp(t), role: "owner", settings: map[string]any{
		"personal_agent_model_policy": workspaceModelPolicy{DisabledModels: []string{"model-a", "retired-model"}, DisabledProviders: []string{"glm", "retired-provider"}},
		"custom_agent_model_policy":   workspaceModelPolicy{DisabledModels: []string{"custom-old"}, DisabledProviders: []string{"glm"}},
		"unrelated_setting":           true,
	}}
	cfg, _, _ := fixture.app.State.Snapshot()
	account, _, _ := cfg.FindAccount("primary@example.com")
	session, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].Workspaces = append(cfg.Accounts[0].Workspaces, NotionWorkspace{ID: "policy-space", PlanType: "business"})
	record := func(value any) map[string]any { return map[string]any{"value": map[string]any{"value": value}} }
	spaceRecords := func() map[string]any {
		return map[string]any{"space": map[string]any{"policy-space": record(map[string]any{"id": "policy-space", "settings": fixture.settings})}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.calls++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/api/v3/getSpacesInitial":
			_ = json.NewEncoder(w).Encode(map[string]any{"users": map[string]any{session.UserID: map[string]any{"user_root": map[string]any{session.UserID: record(map[string]any{"space_view_pointers": []any{
				map[string]any{"id": "default-view", "spaceId": "space-primary"}, map[string]any{"id": "policy-view", "spaceId": "policy-space"},
			}})}}}})
		case "/api/v3/getSpacesFanout":
			pointers := sliceValue(mapValue(payload["users"])[session.UserID])
			if len(pointers) != 1 || mapValue(pointers[0])["id"] != "policy-view" || mapValue(pointers[0])["spaceId"] != "policy-space" || mapValue(pointers[0])["table"] != "space_view" {
				t.Error("read the wrong workspace view")
			}
			records := spaceRecords()
			records["space_user"] = map[string]any{
				"current": record(map[string]any{"user_id": session.UserID, "space_id": "policy-space", "membership_type": fixture.role}),
				"other":   record(map[string]any{"user_id": "other-user", "space_id": "policy-space", "membership_type": "owner"}),
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"users": map[string]any{session.UserID: records}})
		case "/api/v3/getAvailableModels":
			if payload["spaceId"] != "policy-space" || payload["surface"] != "workspace_model_settings" {
				t.Error("wrong model catalog surface")
			}
			if fixture.catalog != "" {
				_, _ = w.Write([]byte(fixture.catalog))
				return
			}
			_, _ = w.Write([]byte(`{"modelSelectionRestricted":false,"models":[
{"model":"model-a","modelMessage":"Model A","modelFamily":"mystery","modelProvider":"glm","restrictedForPersonalAgent":true,"workflow":{},"customAgent":{}},
{"model":"model-b","modelMessage":"Model B","modelProvider":"anthropic","workflow":{},"customAgent":{}},
{"model":"model-c","modelMessage":"Model C","modelProvider":"glm","workflow":{},"customAgent":{}},
{"model":"paid-only","modelMessage":"Paid Only","modelProvider":"openai","isDisabled":true,"workflow":{"isDisabled":true,"disabledReason":"trial_not_allowed"}}
]}`))
		case "/api/v3/updateSpaceSettings":
			fixture.writes = append(fixture.writes, payload)
			if fixture.updateStatus != 0 {
				w.Header().Set("Retry-After", "600")
				if fixture.updateStatus == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "/api/v3/updateSpaceSettings")
				}
				w.WriteHeader(fixture.updateStatus)
				return
			}
			if payload["spaceId"] != "policy-space" {
				t.Error("write changed the wrong workspace")
			}
			patch := mapValue(payload["settingsPatch"])
			unset := sliceValue(payload["unsetSettingKeys"])
			if len(patch)+len(unset) != 1 {
				t.Error("write touched multiple settings")
			}
			for key, value := range patch {
				fixture.settings[key] = value
			}
			for _, key := range unset {
				delete(fixture.settings, stringValue(key))
			}
			if fixture.rawUpdateBody != "" {
				_, _ = w.Write([]byte(fixture.rawUpdateBody))
				return
			}
			if fixture.unconfirmed {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": spaceRecords()})
		default:
			t.Errorf("unexpected upstream call: %s", r.URL.Path)
			http.Error(w, "unexpected endpoint", 404)
		}
	}))
	t.Cleanup(server.Close)
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
	if err := fixture.app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *policyTestUpstream) read(t *testing.T, scope string) workspaceModelPolicySnapshot {
	t.Helper()
	query := url.Values{"email": {"primary@example.com"}, "workspace_id": {"policy-space"}, "scope": {scope}}
	r := httptest.NewRequest(http.MethodGet, "/admin/accounts/model-policy?"+query.Encode(), nil)
	r.Header.Set("X-Admin-Token", "test-admin-token")
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("policy read: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("permission and policy snapshots must not be cached")
	}
	var snapshot workspaceModelPolicySnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *policyTestUpstream) edit(t *testing.T, edit modelPolicyEdit) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(edit)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/admin/accounts/model-policy", strings.NewReader(string(body)))
	r.Header.Set("X-Admin-Token", "test-admin-token")
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}

func policyLockEdit(snapshot workspaceModelPolicySnapshot) modelPolicyEdit {
	return modelPolicyEdit{Email: snapshot.Email, WorkspaceID: snapshot.WorkspaceID, Scope: snapshot.Scope, Revision: snapshot.Revision, Action: "lock", ModelID: "model-a"}
}

func TestManualModelPolicyLockAndRestore(t *testing.T) {
	for _, scope := range []string{"personal", "custom"} {
		t.Run(scope, func(t *testing.T) {
			f := newPolicyTestUpstream(t)
			before := f.read(t, scope)
			if !before.CanEdit || before.MembershipType != "owner" || len(f.writes) != 0 {
				t.Fatal("read changed settings or lost membership")
			}
			otherKey := "custom_agent_model_policy"
			if scope == "custom" {
				otherKey = "personal_agent_model_policy"
			}
			otherBefore, _ := json.Marshal(f.settings[otherKey])
			w := f.edit(t, policyLockEdit(before))
			if w.Code != 200 || len(f.writes) != 1 {
				t.Fatalf("lock: %d %s", w.Code, w.Body.String())
			}
			var after workspaceModelPolicySnapshot
			_ = json.Unmarshal(w.Body.Bytes(), &after)
			allowed := []string{}
			for _, model := range after.Models {
				if model.Allowed {
					allowed = append(allowed, model.ID)
				}
			}
			if !reflect.DeepEqual(allowed, []string{"model-a"}) || policyContains(after.Policy.DisabledProviders, "glm") || !policyContains(after.Policy.DisabledProviders, "anthropic") {
				t.Fatalf("incorrect target or provider: %+v", after)
			}
			if scope == "personal" && !policyContains(after.Policy.DisabledModels, "retired-model") {
				t.Fatal("unknown existing restrictions lost")
			}
			otherAfter, _ := json.Marshal(f.settings[otherKey])
			if string(otherBefore) != string(otherAfter) || f.settings["unrelated_setting"] != true {
				t.Fatal("unrelated settings changed")
			}
			// Applying an identical policy should avoid an unnecessary write.
			if w := f.edit(t, policyLockEdit(after)); w.Code != 200 || len(f.writes) != 1 {
				t.Fatal("identical setting was written again")
			}
			// The pre-change policy is kept server-side, so a reload loses nothing.
			if reread := f.read(t, scope); reread.RestorePoint == nil || modelPolicyRevision(scope, reread.RestorePoint.Policy, reread.RestorePoint.Present) != before.Revision {
				t.Fatalf("restore point missing or wrong: %+v", reread.RestorePoint)
			}
			restore := policyLockEdit(after)
			restore.Action = "restore"
			w = f.edit(t, restore)
			if w.Code != 200 || len(f.writes) != 2 {
				t.Fatalf("restore: %d %s", w.Code, w.Body.String())
			}
			if restored := f.read(t, scope); restored.Revision != before.Revision || restored.RestorePoint != nil {
				t.Fatal("restore did not preserve the exact prior policy or kept a stale restore point")
			}
			cfg, _, _ := f.app.State.Snapshot()
			workspace, _ := accountWorkspace(cfg.Accounts[0], "policy-space")
			if workspace.ModelCapabilities != nil {
				t.Fatal("policy change granted manual chat capability")
			}
		})
	}
}

func TestModelPolicyOwnerAndConflictGuards(t *testing.T) {
	for _, scenario := range []string{"member", "unknown", "revoked", "conflict", "unavailable", "cooldown", "invalid_scope"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPolicyTestUpstream(t)
			if scenario == "member" {
				f.role = "member"
			}
			if scenario == "unknown" {
				f.role = ""
			}
			before := f.read(t, "personal")
			edit := policyLockEdit(before)
			want := 403
			switch scenario {
			case "member", "unknown":
				if before.CanEdit {
					t.Fatal("another user's ownership was used")
				}
			case "revoked":
				f.role = "member"
			case "conflict":
				f.settings["personal_agent_model_policy"] = workspaceModelPolicy{}
				want = 409
			case "unavailable":
				edit.ModelID = "paid-only"
				want = 400
			case "invalid_scope":
				edit.Scope = "arbitrary_setting"
				want = 400
			case "cooldown":
				cfg, _, _ := f.app.State.Snapshot()
				cfg.Accounts = cloneAccounts(cfg.Accounts)
				cfg.Accounts[0].CredentialCooldownUntil = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
				if err := f.app.State.SaveAndApply(cfg); err != nil {
					t.Fatal(err)
				}
				want = 429
			}
			w := f.edit(t, edit)
			if w.Code != want || len(f.writes) != 0 {
				t.Fatalf("guard failed: %d %s writes=%d", w.Code, w.Body.String(), len(f.writes))
			}
		})
	}
}

func TestModelPolicyFailuresNeverReplayAndMissingPolicyRestores(t *testing.T) {
	for _, scenario := range []string{"forbidden", "limited", "redirect", "unconfirmed", "absent", "unsupported_policy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPolicyTestUpstream(t)
			if scenario == "absent" {
				delete(f.settings, "personal_agent_model_policy")
			}
			before := f.read(t, "personal")
			want := 200
			switch scenario {
			case "forbidden":
				f.updateStatus = 403
				want = 403
			case "limited":
				f.updateStatus = 429
				want = 429
			case "redirect":
				f.updateStatus = http.StatusTemporaryRedirect
				want = 502
			case "unsupported_policy":
				f.settings["personal_agent_model_policy"] = map[string]any{"disabledModels": []string{}, "future_setting": true}
				want = 502
			case "unconfirmed":
				f.unconfirmed = true
				want = 502
			}
			w := f.edit(t, policyLockEdit(before))
			wantWrites := 1
			if scenario == "unsupported_policy" {
				wantWrites = 0
			}
			if w.Code != want || len(f.writes) != wantWrites {
				t.Fatalf("unexpected retry/result: %d %s writes=%d", w.Code, w.Body.String(), len(f.writes))
			}
			if scenario == "forbidden" || scenario == "limited" {
				// An explicit rejection changed nothing, so no restore point.
				cfg, _, _ := f.app.State.Snapshot()
				if f.app.State.modelPolicyRestorePoint(cfg, modelPolicyRestoreKey("policy-space", "personal")) != nil {
					t.Fatal("rejected write left a restore point")
				}
			}
			if scenario == "unconfirmed" || scenario == "redirect" {
				if after := f.read(t, "personal"); after.RestorePoint == nil {
					t.Fatal("unconfirmed write must keep the restore point")
				}
			}
			if scenario == "limited" {
				cfg, _, _ := f.app.State.Snapshot()
				if w.Header().Get("Retry-After") == "" || !parseOptionalRFC3339(cfg.Accounts[0].CredentialCooldownUntil).After(time.Now()) {
					t.Fatal("rate-limit backoff lost")
				}
			}
			if scenario == "absent" {
				after := f.read(t, "personal")
				restore := policyLockEdit(after)
				restore.Action = "restore"
				if w := f.edit(t, restore); w.Code != 200 {
					t.Fatalf("unset failed: %s", w.Body.String())
				}
				if _, exists := f.settings["personal_agent_model_policy"]; exists {
					t.Fatal("original absence was not restored")
				}
			}
		})
	}
}

func TestModelPolicyRequiresAdminAuthentication(t *testing.T) {
	f := newPolicyTestUpstream(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := httptest.NewRecorder()
		f.app.ServeHTTP(w, httptest.NewRequest(method, "/admin/accounts/model-policy", strings.NewReader(`{}`)))
		if w.Code == 200 || f.calls != 0 {
			t.Fatal("unauthenticated request reached upstream")
		}
	}
}

func TestModelPolicyRejectsIncompleteCatalogBeforeWriting(t *testing.T) {
	for _, catalog := range []string{
		`{"models":[{}]}`,
		`{"models":[{"model":"model-a","modelProvider":"glm"},{}]}`,
		`{"models":[{"model":"model-a","modelProvider":"glm"},{"model":"model-a","modelProvider":"anthropic","isDisabled":true}]}`,
	} {
		t.Run(catalog, func(t *testing.T) {
			f := newPolicyTestUpstream(t)
			before := f.read(t, "personal")
			f.catalog = catalog
			w := f.edit(t, policyLockEdit(before))
			if w.Code != http.StatusBadGateway || len(f.writes) != 0 {
				t.Fatalf("incomplete catalog allowed a policy write: %d writes=%d", w.Code, len(f.writes))
			}
		})
	}
}

func TestModelPolicyClearAndRestoreGuards(t *testing.T) {
	f := newPolicyTestUpstream(t)
	before := f.read(t, "personal")
	restore := policyLockEdit(before)
	restore.Action = "restore"
	if w := f.edit(t, restore); w.Code != http.StatusConflict || len(f.writes) != 0 {
		t.Fatalf("restore without a restore point: %d writes=%d", w.Code, len(f.writes))
	}
	partial := policyLockEdit(before)
	partial.Action, partial.RestorePolicy = "restore", &before.Policy
	if w := f.edit(t, partial); w.Code != http.StatusBadRequest || len(f.writes) != 0 {
		t.Fatalf("restore_policy without restore_present: %d", w.Code)
	}
	clear := policyLockEdit(before)
	clear.Action = "clear"
	if w := f.edit(t, clear); w.Code != 200 || len(f.writes) != 1 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if _, exists := f.settings["personal_agent_model_policy"]; exists {
		t.Fatal("clear did not unset the policy")
	}
}

func TestModelPolicyCatalogSurfacesAndExtraFields(t *testing.T) {
	f := newPolicyTestUpstream(t)
	f.settings["custom_agent_model_policy"] = map[string]any{"disabledModels": []string{"custom-old"}, "disabledProviders": []string{}, "allowedRestrictedModels": []string{"paid-only"}, "defaultModel": "model-c"}
	f.catalog = `{"models":[
{"model":"model-a","modelMessage":"Model A","modelProvider":"glm","workflow":{}},
{"model":"agent-only","modelMessage":"Agent Only","modelProvider":"anthropic","customAgent":{}}
]}`
	personal := f.read(t, "personal")
	for _, model := range personal.Models {
		if model.ID == "agent-only" && model.Available {
			t.Fatal("a model without a workflow entry was offered for personal agents")
		}
	}
	custom := f.read(t, "custom")
	if custom.Policy.DefaultModel != "model-c" || len(custom.Policy.AllowedRestrictedModels) != 1 {
		t.Fatalf("extra policy fields dropped: %+v", custom.Policy)
	}
	lock := policyLockEdit(custom)
	lock.ModelID = "agent-only"
	if w := f.edit(t, lock); w.Code != 200 {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}
	written, _ := json.Marshal(f.settings["custom_agent_model_policy"])
	var policy workspaceModelPolicy
	_ = json.Unmarshal(written, &policy)
	if policy.DefaultModel != "agent-only" || !reflect.DeepEqual(policy.AllowedRestrictedModels, []string{"paid-only"}) {
		t.Fatalf("lock mishandled extra fields: %+v", policy)
	}
}

// readModelPolicyAudit returns every entry written beside the config.
func readModelPolicyAudit(t *testing.T, app *App) []modelPolicyAuditEntry {
	t.Helper()
	cfg, _, _ := app.State.Snapshot()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Storage.SQLitePath), "model_policy_audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []modelPolicyAuditEntry
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry modelPolicyAuditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func lastModelPolicyAuditEntry(t *testing.T, app *App) modelPolicyAuditEntry {
	t.Helper()
	entries := readModelPolicyAudit(t, app)
	if len(entries) == 0 {
		t.Fatal("no audit entry was written")
	}
	return entries[len(entries)-1]
}

// The audit trail exists to answer "what did this write replace?". The success
// path reassigns snapshot.Policy, so an entry that reads it at defer time would
// record before == after and lose the policy the operator overwrote.
func TestModelPolicyAuditRecordsTheReplacedPolicy(t *testing.T) {
	fixture := newPolicyTestUpstream(t)
	before := fixture.read(t, "personal")
	if slices.Contains(before.Policy.DisabledModels, "model-b") {
		t.Fatalf("precondition failed: model-b is already locked (%v)", before.Policy.DisabledModels)
	}

	w := fixture.edit(t, modelPolicyEdit{
		Email: before.Email, WorkspaceID: before.WorkspaceID, Scope: before.Scope,
		Revision: before.Revision, Action: "lock", ModelID: "model-b",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}

	last := lastModelPolicyAuditEntry(t, fixture.app)
	if last.Result != "ok" {
		t.Fatalf("expected a confirmed write, got result=%q", last.Result)
	}
	// Locking one model disables every other one, so before and after must
	// differ. Reading snapshot.Policy at defer time made them identical.
	if !reflect.DeepEqual(last.Before, before.Policy) {
		t.Fatalf("audit recorded %v as the previous policy, want %v", last.Before, before.Policy)
	}
	if reflect.DeepEqual(last.Before, last.After) {
		t.Fatalf("audit recorded the applied policy as its own predecessor: %v", last.After.DisabledModels)
	}
	if slices.Contains(last.After.DisabledModels, "model-b") {
		t.Fatalf("lock did not disable the other models: after=%v", last.After.DisabledModels)
	}
	if !slices.Contains(last.After.DisabledModels, "model-c") {
		t.Fatalf("lock did not disable model-c: after=%v", last.After.DisabledModels)
	}
}

// Notion accepting the write and then returning a body we cannot parse is not
// the same as the write never leaving. The audit trail must not claim the
// request was unsent when the workspace setting may already have changed.
func TestModelPolicyAuditRecordsSentButUnparsableWrite(t *testing.T) {
	fixture := newPolicyTestUpstream(t)
	before := fixture.read(t, "personal")
	fixture.rawUpdateBody = "this is not json"

	w := fixture.edit(t, modelPolicyEdit{
		Email: before.Email, WorkspaceID: before.WorkspaceID, Scope: before.Scope,
		Revision: before.Revision, Action: "lock", ModelID: "model-b",
	})
	if w.Code == http.StatusOK {
		t.Fatalf("unparsable confirmation must not be reported as success: %s", w.Body.String())
	}

	last := lastModelPolicyAuditEntry(t, fixture.app)
	if last.Result == "not_sent" {
		t.Fatal("audit recorded an accepted write as never sent")
	}
	if last.Result != "unconfirmed" {
		t.Fatalf("expected an unconfirmed audit result, got %q", last.Result)
	}
	// The write did reach upstream, so the fixture's settings reflect it.
	if _, ok := fixture.settings["personal_agent_model_policy"]; !ok {
		t.Fatal("precondition failed: upstream never received the write")
	}
}
