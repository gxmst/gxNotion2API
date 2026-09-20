package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Chat capability and the settings-page catalog have different authority.
// A catalog response must never grant permission to select a model.
type WorkspaceModelCapabilities struct {
	Mode      string            `json:"mode"`
	CheckedAt string            `json:"checked_at"`
	Models    []ModelDefinition `json:"models"`
	Catalog   []ModelDefinition `json:"catalog,omitempty"`
}

func cloneWorkspaceModelCapabilities(c *WorkspaceModelCapabilities) *WorkspaceModelCapabilities {
	if c == nil {
		return nil
	}
	out := *c
	out.Models = cloneModelDefinitions(c.Models)
	out.Catalog = cloneModelDefinitions(c.Catalog)
	return &out
}

func cloneModelDefinitions(models []ModelDefinition) []ModelDefinition {
	out := append([]ModelDefinition(nil), models...)
	for i := range out {
		out[i].Aliases = append([]string(nil), out[i].Aliases...)
		out[i].SupportedReasoningEfforts = append([]string(nil), out[i].SupportedReasoningEfforts...)
	}
	return out
}

func parseWorkspaceModelCapabilities(raw []byte) (*WorkspaceModelCapabilities, error) {
	var envelope struct {
		Restricted *bool           `json:"modelSelectionRestricted"`
		Models     json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Restricted == nil || len(envelope.Models) == 0 || string(envelope.Models) == "null" {
		return nil, fmt.Errorf("upstream model capability metadata is incomplete")
	}
	var models []json.RawMessage
	if err := json.Unmarshal(envelope.Models, &models); err != nil {
		return nil, err
	}
	mode := "manual"
	if *envelope.Restricted {
		mode = "auto_only"
	}
	return &WorkspaceModelCapabilities{Mode: mode, CheckedAt: time.Now().UTC().Format(time.RFC3339), Models: parseProbeModelsBlob(string(raw))}, nil
}

func (c *NotionAIClient) fetchWorkspaceModelCapabilities(ctx context.Context) (*WorkspaceModelCapabilities, error) {
	body := map[string]any{"spaceId": c.Session.SpaceID}
	raw, err := c.postJSON(ctx, c.Config.NotionUpstream().API("getAvailableModels"), body, "application/json")
	if err != nil {
		return nil, err
	}
	capability, err := parseWorkspaceModelCapabilities(raw)
	if err != nil {
		return nil, err
	}
	if capability.Mode == "manual" {
		return capability, nil
	}
	// Only a user-initiated refresh reads the descriptive settings catalog.
	// Failure here does not discard a valid chat restriction response.
	body["surface"] = "workspace_model_settings"
	raw, err = c.postJSON(ctx, c.Config.NotionUpstream().API("getAvailableModels"), body, "application/json")
	if err == nil {
		capability.Catalog = parseProbeModelsBlob(string(raw))
	}
	return capability, nil
}

func (s *ServerState) applyWorkspaceModelCapabilities(started NotionAccount, workspaceID string, capability *WorkspaceModelCapabilities) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cfg, _, _ := s.Snapshot()
	current, index, ok := cfg.FindAccount(started.Email)
	if !ok || !workspaceDiscoveryIdentityUnchanged(started, current) {
		return fmt.Errorf("account changed during model refresh; retry")
	}
	workspace, ok := accountWorkspace(current, workspaceID)
	if !ok {
		return fmt.Errorf("workspace removed during model refresh")
	}
	workspace.ModelCapabilities = cloneWorkspaceModelCapabilities(capability)
	setAccountWorkspace(&current, workspace)
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[index] = current
	return s.saveAndApplyCommitted(cfg)
}

type modelSelectionError struct{ reason string }

func (e *modelSelectionError) Error() string { return e.reason }
func isModelSelectionError(err error) bool   { var e *modelSelectionError; return errors.As(err, &e) }

func selectWorkspaceModel(cfg AppConfig, account NotionAccount, request PromptRunRequest) (PromptRunRequest, error) {
	request.ModelSelectionMode = "auto"
	if strings.TrimSpace(request.NotionModel) == "" {
		return request, nil
	}
	workspace, _ := accountWorkspace(account, accountWorkspaceID(account))
	capability := workspace.ModelCapabilities
	if capability == nil || (capability.Mode != "manual" && capability.Mode != "auto_only") {
		return request, &modelSelectionError{"workspace model capability is unknown; refresh models or use model=auto"}
	}
	if capability.Mode == "auto_only" {
		if cfg.Dispatch.RestrictedModelFallback {
			request.NotionModel = ""
			request.ModelSelectionMode = "auto_fallback"
			return request, nil
		}
		return request, &modelSelectionError{"workspace only supports Auto; use model=auto"}
	}
	for _, model := range capability.Models {
		if !model.Enabled {
			continue
		}
		if model.ID == request.PublicModel || model.NotionModel == request.NotionModel {
			request.NotionModel = model.NotionModel
			request.ModelSelectionMode = "manual"
			return request, nil
		}
	}
	return request, &modelSelectionError{"requested model is unavailable in this workspace; refresh models or use model=auto"}
}

func (a *App) prepareWorkspaceModelRequest(cfg AppConfig, session SessionInfo, email string, request PromptRunRequest) (PromptRunRequest, error) {
	// Recheck live capabilities after login refresh or a concurrent settings edit.
	live, _, _ := a.State.Snapshot()
	account, _, ok := live.FindAccountWorkspace(email, session.SpaceID)
	if !ok {
		return request, &modelSelectionError{"workspace is no longer available"}
	}
	return selectWorkspaceModel(live, account, request)
}

func publicWorkspaceModelRegistry(cfg AppConfig, registry ModelRegistry) ModelRegistry {
	out := registry
	out.Entries = nil
	for _, entry := range registry.Entries {
		if entry.ID == "auto" {
			out.Entries = append(out.Entries, entry)
			continue
		}
		for _, account := range cfg.Accounts {
			if account.Disabled {
				continue
			}
			found := false
			for _, workspace := range account.Workspaces {
				eligible, _ := workspaceEligibility(workspace)
				if !eligible || workspace.ModelCapabilities == nil || workspace.ModelCapabilities.Mode != "manual" {
					continue
				}
				for _, model := range workspace.ModelCapabilities.Models {
					if model.Enabled && (model.ID == entry.ID || model.NotionModel == entry.NotionModel) {
						found = true
						break
					}
				}
			}
			if found {
				out.Entries = append(out.Entries, entry)
				break
			}
		}
	}
	return out
}

func writeModelSelectionError(w http.ResponseWriter, err error) {
	writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_selection_unavailable")
}
