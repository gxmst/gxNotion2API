package app

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	conversationSessionStatusActive      = "active"
	conversationSessionStatusStale       = "stale"
	conversationSessionStatusInvalidated = "invalidated"
)

type ConversationSession struct {
	ID               string    `json:"id"`
	ConversationID   string    `json:"conversation_id"`
	Fingerprint      string    `json:"fingerprint"`
	ThreadID         string    `json:"thread_id"`
	AccountEmail     string    `json:"account_email"`
	ConfigID         string    `json:"config_id"`
	ContextID        string    `json:"context_id"`
	OriginalDatetime string    `json:"original_datetime"`
	ModelUsed        string    `json:"model_used,omitempty"`
	TurnCount        int       `json:"turn_count"`
	RawMessageCount  int       `json:"raw_message_count"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	LastUsedAt       time.Time `json:"last_used_at"`
	DeletedAt        time.Time `json:"deleted_at,omitempty"`
}

type ConversationSessionStep struct {
	SessionID       string    `json:"session_id"`
	StepIndex       int       `json:"step_index"`
	UpdatedConfigID string    `json:"updated_config_id"`
	ResponseID      string    `json:"response_id,omitempty"`
	MessageID       string    `json:"message_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type conversationContinuationState struct {
	Session          ConversationSession
	UpdatedConfigIDs []string
}

func canonicalConversationFingerprint(hiddenPrompt string, segments []conversationPromptSegment) string {
	return canonicalConversationFingerprintScoped("", hiddenPrompt, segments)
}

func canonicalConversationFingerprintScoped(scope string, hiddenPrompt string, segments []conversationPromptSegment) string {
	h := sha256.New()
	if cleanScope := collapseWhitespace(scope); cleanScope != "" {
		h.Write([]byte("scope:"))
		h.Write([]byte(cleanScope))
		h.Write([]byte{'\n'})
	}
	if hidden := collapseWhitespace(hiddenPrompt); hidden != "" {
		if len(hidden) > 800 {
			hidden = truncateRunes(hidden, 800)
		}
		h.Write([]byte("hidden:"))
		h.Write([]byte(hidden))
		h.Write([]byte{'\n'})
	}
	normalized := normalizeConversationHistorySegments(segments)
	if len(normalized) == 0 {
		return ""
	}
	firstUser := ""
	for _, segment := range normalized {
		if segment.Role == "user" {
			firstUser = segment.Text
			break
		}
	}
	if firstUser != "" {
		if len(firstUser) > 800 {
			firstUser = truncateRunes(firstUser, 800)
		}
		h.Write([]byte("first-user:"))
		h.Write([]byte(firstUser))
		h.Write([]byte{'\n'})
	}
	firstAssistant := ""
	for _, segment := range normalized {
		if segment.Role == "assistant" {
			firstAssistant = segment.Text
			break
		}
	}
	if firstAssistant != "" {
		if len(firstAssistant) > 400 {
			firstAssistant = truncateRunes(firstAssistant, 400)
		}
		h.Write([]byte("first-assistant:"))
		h.Write([]byte(firstAssistant))
		h.Write([]byte{'\n'})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func sessionRawMessageCount(segments []conversationPromptSegment) int {
	return len(normalizeConversationHistorySegments(segments))
}

func buildContinuationDraft(state *conversationContinuationState) *continuationTurnDraft {
	if state == nil {
		return nil
	}
	updated := make([]string, len(state.UpdatedConfigIDs))
	copy(updated, state.UpdatedConfigIDs)
	return &continuationTurnDraft{
		SessionID:        strings.TrimSpace(state.Session.ID),
		ConfigID:         strings.TrimSpace(state.Session.ConfigID),
		ContextID:        strings.TrimSpace(state.Session.ContextID),
		UpdatedConfigIDs: updated,
		OriginalDatetime: strings.TrimSpace(state.Session.OriginalDatetime),
		TurnCount:        state.Session.TurnCount,
		RawMessageCount:  state.Session.RawMessageCount,
		Fingerprint:      strings.TrimSpace(state.Session.Fingerprint),
	}
}

func shouldInvalidateConversationSession(session ConversationSession, rawMessageCount int) bool {
	if rawMessageCount <= 0 || session.RawMessageCount <= 0 {
		return false
	}
	return rawMessageCount < session.RawMessageCount
}
