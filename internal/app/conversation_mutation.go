package app

import (
	"errors"
	"fmt"
)

var errConversationMutationPersistence = errors.New("failed to persist conversation change")

// Resolve persistence before taking conversation locks. Mutations hold
// persistMu then mu, matching snapshot writes, and publish only after saving.
func (s *ServerState) conversationMutationSaver() func(ConversationEntry) error {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := conversationSnapshotsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled {
		return nil
	}
	return store.SaveConversation
}

func persistConversationMutation(entry ConversationEntry, savers []func(ConversationEntry) error) error {
	if len(savers) > 0 && savers[0] != nil {
		if err := savers[0](entry); err != nil {
			return fmt.Errorf("%w: %v", errConversationMutationPersistence, err)
		}
	}
	return nil
}
