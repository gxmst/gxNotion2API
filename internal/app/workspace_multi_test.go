package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeAccountWorkspacesMigratesLegacyFields(t *testing.T) {
	account := normalizeAccountWorkspaces(NotionAccount{
		Email:          "user@example.com",
		SpaceID:        "legacy-space",
		SpaceViewID:    "legacy-view",
		SpaceName:      "Legacy",
		PlanType:       "personal",
		HourlyQuota:    7,
		MaxConcurrency: 2,
	})

	if account.DefaultWorkspaceID != "legacy-space" {
		t.Fatalf("default workspace = %q, want legacy-space", account.DefaultWorkspaceID)
	}
	if len(account.Workspaces) != 1 || account.Workspaces[0].ID != "legacy-space" {
		t.Fatalf("legacy workspace migration = %#v", account.Workspaces)
	}
	if account.Workspaces[0].HourlyQuota != 7 || account.Workspaces[0].MaxConcurrency != 2 {
		t.Fatalf("legacy runtime settings were not migrated: %#v", account.Workspaces[0])
	}
}

func TestUpsertAccountMergesSameEmailWorkspacesWithoutChangingDefault(t *testing.T) {
	cfg := defaultConfig()
	cfg.UpsertAccount(NotionAccount{
		Email:       "user@example.com",
		SpaceID:     "workspace-one",
		SpaceViewID: "view-one",
		SpaceName:   "One",
		PlanType:    "team",
	})
	cfg.UpsertAccount(NotionAccount{
		Email:       "user@example.com",
		SpaceID:     "workspace-two",
		SpaceViewID: "view-two",
		SpaceName:   "Two",
		PlanType:    "team",
	})

	account, _, ok := cfg.FindAccount("USER@example.com")
	if !ok {
		t.Fatal("merged account not found")
	}
	if account.DefaultWorkspaceID != "workspace-one" {
		t.Fatalf("default workspace changed during merge: %q", account.DefaultWorkspaceID)
	}
	if len(account.Workspaces) != 2 {
		t.Fatalf("workspace count = %d, want 2: %#v", len(account.Workspaces), account.Workspaces)
	}
	second, ok := accountWorkspace(account, "workspace-two")
	if !ok || second.ViewID != "view-two" {
		t.Fatalf("second workspace was not merged: %#v", second)
	}

	cfg.UpsertAccount(NotionAccount{
		Email:              "user@example.com",
		DefaultWorkspaceID: "workspace-two",
	})
	account, _, _ = cfg.FindAccount("user@example.com")
	if account.DefaultWorkspaceID != "workspace-two" {
		t.Fatalf("explicit default workspace was ignored: %q", account.DefaultWorkspaceID)
	}
}

func TestDispatchCandidatesFilterByWorkspaceAcrossAccounts(t *testing.T) {
	cfg := defaultConfig()
	cfg.Accounts = []NotionAccount{
		{Email: "one@example.com", Workspaces: []NotionWorkspace{{ID: "shared", Status: "ready"}, {ID: "other", Status: "ready"}}},
		{Email: "two@example.com", Workspaces: []NotionWorkspace{{ID: "shared", Status: "ready"}}},
	}
	var pool []NotionAccount
	for _, account := range cfg.Accounts {
		pool = append(pool, accountWorkspaceCandidates(account)...)
	}
	candidates, err := resolveDispatchCandidatesWithPool(cfg, pool, PromptRunRequest{WorkspaceID: "shared"}, nowForWorkspaceTest())
	if err != nil {
		t.Fatalf("resolve workspace candidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2: %#v", len(candidates), candidates)
	}
	for _, candidate := range candidates {
		if candidate.SpaceID != "shared" {
			t.Fatalf("candidate workspace = %q, want shared", candidate.SpaceID)
		}
	}
}

func TestWorkspaceDispatchSlotsAreIndependent(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Storage.SQLitePath = ""
	cfg.Accounts = []NotionAccount{{
		Email:              "user@example.com",
		DefaultWorkspaceID: "one",
		Workspaces: []NotionWorkspace{
			{ID: "one", MaxConcurrency: 1, Status: "ready"},
			{ID: "two", MaxConcurrency: 1, Status: "ready"},
		},
	}}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatalf("newServerState: %v", err)
	}
	defer state.Close()

	if !state.TryAcquireWorkspaceDispatchSlot("user@example.com", "one") {
		t.Fatal("first workspace slot was not acquired")
	}
	if !state.TryAcquireWorkspaceDispatchSlot("user@example.com", "two") {
		t.Fatal("second workspace should have an independent slot")
	}
	if state.TryAcquireWorkspaceDispatchSlot("user@example.com", "one") {
		t.Fatal("first workspace exceeded its concurrency limit")
	}
	state.ReleaseWorkspaceDispatchSlot("user@example.com", "one")
	state.ReleaseWorkspaceDispatchSlot("user@example.com", "two")
}

func TestWorkspaceRuntimeStateDoesNotLeakAcrossWorkspaces(t *testing.T) {
	cfg := defaultConfig()
	now := time.Now()
	cfg.Accounts = []NotionAccount{{
		Email:              "user@example.com",
		DefaultWorkspaceID: "one",
		Workspaces: []NotionWorkspace{
			{ID: "one", HourlyQuota: 2, WindowStartedAt: formatRFC3339OrEmpty(now), WindowRequestCount: 2, TotalSuccesses: 1},
			{ID: "two", HourlyQuota: 5, WindowStartedAt: formatRFC3339OrEmpty(now), WindowRequestCount: 0, TotalSuccesses: 7},
		},
	}}
	cfg = normalizeConfig(cfg)
	secondAccount, _, ok := cfg.FindAccountWorkspace("user@example.com", "two")
	if !ok {
		t.Fatal("second workspace not found")
	}
	secondAccount.TotalSuccesses = 8
	secondAccount.CooldownUntil = formatRFC3339OrEmpty(now.Add(time.Hour))
	cfg.UpsertAccountRuntimeState(secondAccount)

	stored, _, _ := cfg.FindAccount("user@example.com")
	first, ok := accountWorkspace(stored, "one")
	if !ok || first.TotalSuccesses != 1 || first.CooldownUntil != "" {
		t.Fatalf("first workspace was changed by second workspace update: %#v", first)
	}
	secondWorkspace, ok := accountWorkspace(stored, "two")
	if !ok || secondWorkspace.TotalSuccesses != 8 || secondWorkspace.CooldownUntil == "" {
		t.Fatalf("second workspace update was lost: %#v", secondWorkspace)
	}
	firstAccount, _, _ := cfg.FindAccountWorkspace("user@example.com", "one")
	if remaining, limited := accountRemainingQuota(firstAccount, now); !limited || remaining != 0 {
		t.Fatalf("first workspace quota = %d/%v, want 0/true", remaining, limited)
	}
	if remaining, limited := accountRemainingQuota(secondAccount, now); !limited || remaining != 5 {
		t.Fatalf("second workspace quota = %d/%v, want 5/true", remaining, limited)
	}
}

func TestAccountRuntimeSummaryExposesWorkspaceRuntime(t *testing.T) {
	cfg := defaultConfig()
	cfg.ActiveAccount = "user@example.com"
	cfg.ActiveWorkspaceID = "two"
	cfg.Accounts = []NotionAccount{{
		Email:              "user@example.com",
		DefaultWorkspaceID: "one",
		Workspaces: []NotionWorkspace{
			{ID: "one", Name: "Personal"},
			{ID: "two", Name: "Business", HourlyQuota: 10, MaxConcurrency: 3},
		},
	}}
	cfg = normalizeConfig(cfg)
	item := (&App{}).accountRuntimeSummary(cfg, cfg.Accounts[0])
	workspaces, ok := item["workspaces"].([]map[string]any)
	if !ok || len(workspaces) != 2 {
		t.Fatalf("workspace runtime summary = %#v", item["workspaces"])
	}
	if workspaces[0]["default"] != true || workspaces[0]["active"] != false {
		t.Fatalf("default workspace flags = %#v", workspaces[0])
	}
	if workspaces[1]["active"] != true || workspaces[1]["max_concurrency"] != 3 {
		t.Fatalf("active workspace flags = %#v", workspaces[1])
	}
}

func TestSQLitePersistsActiveWorkspaceSelection(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Storage.SQLitePath = filepath.Join(t.TempDir(), "accounts.sqlite")
	cfg.ActiveAccount = "user@example.com"
	cfg.ActiveWorkspaceID = "two"
	cfg.Accounts = []NotionAccount{{
		Email:              "user@example.com",
		DefaultWorkspaceID: "one",
		Workspaces:         []NotionWorkspace{{ID: "one"}, {ID: "two"}},
	}}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatalf("newServerState: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close first state: %v", err)
	}

	cfg.ActiveWorkspaceID = ""
	reloaded, err := newServerState(cfg)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	defer reloaded.Close()
	if reloaded.Config.ActiveAccount != "user@example.com" || reloaded.Config.ActiveWorkspaceID != "two" {
		t.Fatalf("reloaded active target = %s/%s, want user@example.com/two", reloaded.Config.ActiveAccount, reloaded.Config.ActiveWorkspaceID)
	}
}

func TestNormalizeConfigKeepsActiveWorkspaceSelection(t *testing.T) {
	cfg := defaultConfig()
	cfg.ActiveAccount = "user@example.com"
	cfg.ActiveWorkspaceID = "paid"
	cfg.Accounts = []NotionAccount{{
		Email:              "user@example.com",
		DefaultWorkspaceID: "personal",
		Workspaces:         []NotionWorkspace{{ID: "personal", Name: "Personal"}, {ID: "paid", Name: "Paid"}},
	}}

	cfg = normalizeConfig(cfg)
	if cfg.ActiveWorkspaceID != "paid" {
		t.Fatalf("active workspace = %q, want paid", cfg.ActiveWorkspaceID)
	}
	account, _, ok := cfg.ResolveActiveWorkspace()
	if !ok || account.SpaceID != "paid" {
		t.Fatalf("active account projection = %#v, want paid workspace", account)
	}
}

func nowForWorkspaceTest() time.Time {
	return time.Now()
}
