package app

import "testing"

func TestFutureModelCatalogAndAliases(t *testing.T) {
	// Synthetic future model: no built-in mapping may be necessary.
	models := parseProbeModelsBlob(`{"models":[{"model":"future-settings-id","modelMessage":"GPT 8","modelFamily":"openai","workflow":{"finalModelName":"future-runtime-id"}}]}`)
	if len(models) != 1 || models[0].ID != "gpt-8" {
		t.Fatal("new catalog model was lost")
	}
	registry := buildModelRegistry(AppConfig{Models: models})
	for _, alias := range []string{"GPT 8", "gpt-8", "future-settings-id", "future-runtime-id"} {
		got, err := registry.Resolve(alias, "")
		if err != nil || got.NotionModel != "future-runtime-id" {
			t.Fatalf("future alias failed: %s", alias)
		}
	}
}

func TestAmbiguousModelAliasFailsClosed(t *testing.T) {
	models := []ModelDefinition{
		{ID: "review-a", Name: "Review A", NotionModel: "runtime-a", Enabled: true, Aliases: []string{"shared-alias"}},
		{ID: "review-b", Name: "Review B", NotionModel: "runtime-b", Enabled: true, Aliases: []string{"shared-alias", "review-a"}},
		{ID: "review-c", Name: "Review C", NotionModel: "runtime-c", Enabled: true, Aliases: []string{"shared-alias"}},
	}
	registry := buildModelRegistry(AppConfig{Models: models})
	if _, err := registry.Resolve("shared-alias", ""); err == nil {
		t.Fatal("ambiguous alias silently chose a model")
	}
	if got, err := registry.Resolve("review-a", ""); err != nil || got.ID != "review-a" {
		t.Fatal("alias shadowed stable model ID")
	}
	registry = buildModelRegistry(AppConfig{Models: models, ModelAliases: map[string]string{"shared-alias": "review-b"}})
	if got, err := registry.Resolve("shared-alias", ""); err != nil || got.ID != "review-b" {
		t.Fatal("explicit disambiguation ignored")
	}
}

func TestFuturePolicyModelKeepsScopeSpecificAlias(t *testing.T) {
	fixture := newPolicyTestUpstream(t)
	fixture.catalog = `{"models":[{"model":"future-settings-id","modelMessage":"GPT 8","modelProvider":"openai","workflow":{"finalModelName":"future-chat-id"},"customAgent":{"finalModelName":"future-custom-id"}}]}`
	for scope, alias := range map[string]string{"personal": "future-chat-id", "custom": "future-custom-id"} {
		snapshot := fixture.read(t, scope)
		if len(snapshot.Models) != 1 || snapshot.Models[0].FinalModel != alias {
			t.Fatal("scope alias was lost")
		}
		policy, err := lockWorkspaceModel(snapshot, "future-settings-id")
		if err != nil || policyContains(policy.DisabledModels, "future-settings-id") {
			t.Fatal("new policy model cannot be selected")
		}
	}
}
