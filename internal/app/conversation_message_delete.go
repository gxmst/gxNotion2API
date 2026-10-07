package app

import (
	"fmt"
	"strings"
	"time"
)

// ImportRemote retains upstream identity and history before local continuation.
func (s *ConversationStore) ImportRemote(entry ConversationEntry) ConversationEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.items[entry.ID]; existing != nil {
		return cloneConversationEntry(existing)
	}
	entry = cloneConversationEntry(&entry)
	entry.RemoteOnly = false
	refreshConversationDerivedFields(&entry)
	s.items[entry.ID] = &entry
	s.moveToFrontLocked(entry.ID)
	s.trimLocked()
	return cloneConversationEntry(&entry)
}

// DeleteMessage edits only the local transcript, like SetMessageContent.
func (s *ConversationStore) DeleteMessage(conversationID, messageID string, savers ...func(ConversationEntry) error) (ConversationEntry, error) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	entry := s.items[strings.TrimSpace(conversationID)]
	if entry == nil {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation not found")
	}
	if conversationStatusBusy(entry.Status) || entry.Status == "deleting" {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation is running; stop it before deleting a message")
	}
	index := -1
	for i, m := range entry.Messages {
		if m.ID == strings.TrimSpace(messageID) && m.ID != "" {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("message not found")
	}
	next := cloneConversationEntry(entry)
	message := next.Messages[index]
	// Old snapshots did not retain the upstream user step ID. Remember only
	// the following assistant's identity so a later sync can hide that user
	// step without retaining the deleted text or pairing unrelated turns.
	// Capture pairs before either member is removed, in either deletion order.
	for i, answer := range next.Messages {
		if answer.Role != "assistant" {
			continue
		}
		if user, ok := precedingTurnUser(next.Messages, i); ok && user.UpstreamMessageID == "" {
			anchor := firstNonEmpty(answer.UpstreamMessageID, answer.ID)
			if i == len(next.Messages)-1 && next.MessageID != "" && !containsTrimmedString(next.DeletedMessageIDs, next.MessageID) {
				anchor = next.MessageID
			}
			if next.LegacyUserTurnIDs == nil {
				next.LegacyUserTurnIDs = map[string]string{}
			}
			if next.LegacyUserTurnIDs[user.ID] == "" {
				next.LegacyUserTurnIDs[user.ID] = anchor
			}
		}
	}
	next.DeletedMessageIDs = append(next.DeletedMessageIDs, message.ID)
	if message.UpstreamMessageID != "" {
		next.DeletedMessageIDs = append(next.DeletedMessageIDs, message.UpstreamMessageID)
	}
	if index == len(next.Messages)-1 && message.Role == "assistant" && next.MessageID != "" {
		next.DeletedMessageIDs = append(next.DeletedMessageIDs, next.MessageID)
	}
	next.Messages = append(next.Messages[:index:index], next.Messages[index+1:]...)
	next.RequestFingerprint = ""
	next.UpdatedAt = time.Now().UTC()
	refreshConversationDerivedFields(&next)
	if err := persistConversationMutation(next, savers); err != nil {
		s.mu.Unlock()
		return ConversationEntry{}, err
	}
	s.items[next.ID] = &next
	summary := buildConversationSummary(&next)
	s.mu.Unlock()
	s.broadcast(ConversationEvent{Type: "conversation.updated", ConversationID: next.ID, At: next.UpdatedAt, Summary: &summary})
	return cloneConversationEntry(&next), nil
}
