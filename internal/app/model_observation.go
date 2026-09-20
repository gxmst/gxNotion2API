package app

import "strings"

// Evidence reported by an inference step, never the requested config model.
// Preserve unknown codenames without guessing a public model identity.
type ModelObservation struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
	Source   string `json:"source"`
	StepID   string `json:"step_id,omitempty"`
}

func mergeModelObservations(a, b []ModelObservation) []ModelObservation {
	out := append([]ModelObservation(nil), a...)
	for _, item := range b {
		item.Model = strings.TrimSpace(item.Model)
		if item.Model == "" {
			continue
		}
		found := false
		for i, prior := range out {
			if prior.Model == item.Model && prior.StepID == item.StepID {
				if out[i].Provider == "" {
					out[i].Provider = item.Provider
				}
				found = true
				break
			}
		}
		if !found {
			out = append(out, item)
		}
	}
	return out
}

func observeStepModels(step map[string]any, stepID, source string) []ModelObservation {
	var out []ModelObservation
	if model := strings.TrimSpace(stringValue(step["model"])); model != "" {
		out = append(out, ModelObservation{Model: model, StepID: stepID, Source: source, Provider: strings.TrimSpace(stringValue(step["modelProvider"]))})
	}
	for _, raw := range sliceValue(step["value"]) {
		part := mapValue(raw)
		out = mergeModelObservations(out, []ModelObservation{{Model: strings.TrimSpace(stringValue(part["notionModelName"])), Provider: strings.TrimSpace(stringValue(part["modelProvider"])), StepID: stepID, Source: source}})
	}
	return out
}

func (s *ndjsonTranscriptState) observeModelPatch(op ndjsonPatchOperation) {
	index, rest, ok := parsePatchStepIndex(op.P)
	if !ok || index < 0 || index >= len(s.Steps) || s.Steps[index].Type != "agent-inference" {
		return
	}
	step := &s.Steps[index]
	if rest == "/model" && op.O != "r" {
		step.ModelObservations = mergeModelObservations(step.ModelObservations, []ModelObservation{{Model: stringValue(op.V), StepID: step.ID, Source: "stream"}})
	}
	if strings.HasPrefix(rest, "/value/") {
		if part := mapValue(op.V); part != nil {
			step.ModelObservations = mergeModelObservations(step.ModelObservations, observeStepModels(map[string]any{"value": []any{part}}, step.ID, "stream"))
		}
		if (strings.HasSuffix(rest, "/notionModelName") || strings.HasSuffix(rest, "/modelProvider")) && op.O != "r" {
			if step.ModelParts == nil {
				step.ModelParts = map[string]ModelObservation{}
			}
			key := rest[:strings.LastIndex(rest, "/")]
			part := step.ModelParts[key]
			part.StepID, part.Source = step.ID, "stream"
			if strings.HasSuffix(rest, "/notionModelName") {
				part.Model = stringValue(op.V)
			} else {
				part.Provider = stringValue(op.V)
			}
			step.ModelParts[key] = part
			step.ModelObservations = mergeModelObservations(step.ModelObservations, []ModelObservation{part})
		}
	}
}
