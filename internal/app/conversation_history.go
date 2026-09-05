package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

var errConversationWorkspaceMismatch = errors.New("conversation belongs to another workspace; select its original workspace or start a new conversation")

func exactConversationSegments(segments []conversationPromptSegment) []conversationPromptSegment {
	out := make([]conversationPromptSegment, 0, len(segments))
	for _, segment := range segments {
		role := strings.ToLower(strings.TrimSpace(segment.Role))
		text := strings.TrimSpace(segment.Text)
		if (role == "user" || role == "assistant") && text != "" {
			out = append(out, conversationPromptSegment{Role: role, Text: text})
		}
	}
	return out
}

func exactHistorySuffix(stored, incoming []conversationPromptSegment) bool {
	if len(incoming) == 0 || len(incoming) > len(stored) {
		return false
	}
	offset := len(stored) - len(incoming)
	for i := range incoming {
		if stored[offset+i] != incoming[i] {
			return false
		}
	}
	return true
}

// A stable opening is only a lookup hint. Reuse requires the supplied history
// to describe the stored branch, including edits in the middle of a transcript.
func conversationHistoryCompatible(entry ConversationEntry, segments []conversationPromptSegment, incremental bool) bool {
	incoming := exactConversationSegments(segments)
	if incremental && len(incoming) <= 1 {
		return true
	}
	stored := conversationMessageSegments(&entry)
	if len(incoming) == 0 || len(stored) == 0 {
		return false
	}
	history := incoming
	if history[len(history)-1].Role == "user" {
		history = history[:len(history)-1]
	}
	if exactHistorySuffix(stored, history) {
		return true
	}
	if stored[len(stored)-1].Role == "assistant" {
		return exactHistorySuffix(stored[:len(stored)-1], incoming)
	}
	return exactHistorySuffix(stored, incoming)
}

func conversationRequestFingerprint(request PromptRunRequest) string {
	data, _ := json.Marshal(struct {
		History                            []conversationPromptSegment
		Prompt, Hidden, Model, NotionModel string
		WebSearch                          bool
		Attachments                        []ConversationAttachment
	}{
		History: exactConversationSegments(request.HistorySegments),
		Prompt:  strings.TrimSpace(request.LatestUserPrompt),
		Hidden:  strings.TrimSpace(request.HiddenPrompt),
		Model:   request.PublicModel, NotionModel: request.NotionModel,
		WebSearch: request.UseWebSearch, Attachments: summarizeInputAttachments(request.Attachments),
	})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
