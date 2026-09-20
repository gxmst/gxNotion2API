package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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
{"model":"model-a","modelMessage":"Model A","modelFamily":"mystery","modelProvider":"glm","restrictedForPersonalAgent":true,"workflow":{}},
{"model":"model-b","modelMessage":"Model B","modelProvider":"anthropic","workflow":{}},
{"model":"model-c","modelMessage":"Model C","modelProvider":"glm","workflow":{}},
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
			restore := policyLockEdit(after)
			restore.Action, restore.RestorePolicy, restore.RestorePresent = "restore", &before.Policy, before.PolicyPresent
			w = f.edit(t, restore)
			if w.Code != 200 || len(f.writes) != 2 {
				t.Fatalf("restore: %d %s", w.Code, w.Body.String())
			}
			if restored := f.read(t, scope); restored.Revision != before.Revision {
				t.Fatal("restore did not preserve the exact prior policy")
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
				restore.RestorePolicy = &before.Policy
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
