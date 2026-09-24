package app

import (
	"container/heap"
	"strings"
	"time"
)

const responseStoreCleanupInterval = 30 * time.Second

// responseLinkRetentionFloor is the minimum time a cleared response row is kept
// as a continuation link after its body expires. The link only serves a client
// that resends the previous response id, so a bounded window is enough; without
// one the responses table grows with every turn ever served.
const responseLinkRetentionFloor = 7 * 24 * time.Hour

// responseLinkRetention scales the link window with the configured body TTL so
// a deployment that keeps bodies for a long time also keeps their links longer.
func responseLinkRetention(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return responseLinkRetentionFloor
	}
	retention := ttl * 24
	if retention < responseLinkRetentionFloor {
		retention = responseLinkRetentionFloor
	}
	return retention
}

// maxResponsesLoadedAtStartup caps the startup load so an oversized table cannot
// stall the process. The retention window bounds it in steady state; this is the
// safety net for a database that predates it.
const maxResponsesLoadedAtStartup = 200000

// Payload retention and continuation retention are independent. A retained
// link can resume only its existing conversation, never a deleted thread.
func (s *ServerState) getContinuationResponse(id string) (StoredResponse, bool) {
	if record, ok := s.getStoredResponse(id); ok {
		return record, true
	}
	s.mu.RLock()
	var record StoredResponse
	var ok bool
	if s.ResponseStore != nil {
		record, ok = s.ResponseStore.links[strings.TrimSpace(id)]
	}
	s.mu.RUnlock()
	if !ok || record.ConversationID == "" {
		return StoredResponse{}, false
	}
	entry, exists := s.conversations().Get(record.ConversationID)
	return record, exists && entry.ThreadID != "" && entry.ThreadID == record.ThreadID
}

type responseExpiryEntry struct {
	responseID string
	createdAt  time.Time
}

type responseExpiryHeap []responseExpiryEntry

func (h responseExpiryHeap) Len() int {
	return len(h)
}

func (h responseExpiryHeap) Less(i, j int) bool {
	return h[i].createdAt.Before(h[j].createdAt)
}

func (h responseExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *responseExpiryHeap) Push(x any) {
	entry, _ := x.(responseExpiryEntry)
	*h = append(*h, entry)
}

func (h *responseExpiryHeap) Pop() any {
	if h == nil || len(*h) == 0 {
		return responseExpiryEntry{}
	}
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type responseStore struct {
	ttl         time.Duration
	items       map[string]StoredResponse
	links       map[string]StoredResponse
	expirations responseExpiryHeap
}

var testHookResponseStorePrunePop func()

func normalizeResponseStoreTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Second
	}
	return ttl
}

func newResponseStore(ttl time.Duration) *responseStore {
	store := &responseStore{
		ttl:         normalizeResponseStoreTTL(ttl),
		items:       map[string]StoredResponse{},
		expirations: responseExpiryHeap{},
	}
	heap.Init(&store.expirations)
	return store
}

func (s *responseStore) setTTL(ttl time.Duration) {
	if s == nil {
		return
	}
	s.ttl = normalizeResponseStoreTTL(ttl)
}

func (s *responseStore) ensureInitialized() {
	if s == nil {
		return
	}
	if s.links == nil {
		s.links = map[string]StoredResponse{}
	}
	if s.items == nil {
		s.items = map[string]StoredResponse{}
	}
	if s.expirations == nil {
		s.expirations = responseExpiryHeap{}
		heap.Init(&s.expirations)
	}
}

func (s *responseStore) save(responseID string, record StoredResponse, now time.Time) {
	if s == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	s.ensureInitialized()
	now = now.UTC()
	s.pruneExpired(now)

	createdAt := record.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = now
	}
	record.CreatedAt = createdAt
	record.ConversationID = strings.TrimSpace(record.ConversationID)
	record.ThreadID = strings.TrimSpace(record.ThreadID)
	record.AccountEmail = strings.TrimSpace(record.AccountEmail)

	s.items[responseID] = record
	link := record
	link.Payload = nil
	s.links[responseID] = link
	heap.Push(&s.expirations, responseExpiryEntry{
		responseID: responseID,
		createdAt:  createdAt,
	})
}

func (s *responseStore) get(responseID string, now time.Time) (StoredResponse, bool) {
	if s == nil {
		return StoredResponse{}, false
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return StoredResponse{}, false
	}
	s.ensureInitialized()
	now = now.UTC()
	s.pruneExpired(now)
	record, ok := s.items[responseID]
	if !ok {
		return StoredResponse{}, false
	}
	if now.Sub(record.CreatedAt) > s.ttl {
		delete(s.items, responseID)
		return StoredResponse{}, false
	}
	return record, true
}

func (s *responseStore) replaceAll(records map[string]StoredResponse) {
	if s == nil {
		return
	}
	s.ensureInitialized()
	s.items = map[string]StoredResponse{}
	s.links = map[string]StoredResponse{}
	s.expirations = responseExpiryHeap{}
	heap.Init(&s.expirations)
	for responseID, record := range records {
		cleanID := strings.TrimSpace(responseID)
		if cleanID == "" {
			continue
		}
		createdAt := record.CreatedAt.UTC()
		record.CreatedAt = createdAt
		record.ConversationID = strings.TrimSpace(record.ConversationID)
		record.ThreadID = strings.TrimSpace(record.ThreadID)
		record.AccountEmail = strings.TrimSpace(record.AccountEmail)
		link := record
		link.Payload = nil
		s.links[cleanID] = link
		// SQLite retains an empty payload after cleanup. A later TTL increase
		// must not make that deleted body readable again.
		if len(record.Payload) == 0 {
			continue
		}
		s.items[cleanID] = record
		heap.Push(&s.expirations, responseExpiryEntry{
			responseID: cleanID,
			createdAt:  createdAt,
		})
	}
}

func (s *responseStore) pruneExpired(now time.Time) int {
	if s == nil {
		return 0
	}
	s.ensureInitialized()
	if len(s.items) == 0 || len(s.expirations) == 0 {
		return 0
	}
	now = now.UTC()
	removed := 0
	for len(s.expirations) > 0 {
		top := s.expirations[0]
		if now.Sub(top.createdAt) <= s.ttl {
			break
		}
		entry, _ := heap.Pop(&s.expirations).(responseExpiryEntry)
		if testHookResponseStorePrunePop != nil {
			testHookResponseStorePrunePop()
		}
		current, ok := s.items[entry.responseID]
		if !ok {
			continue
		}
		if !current.CreatedAt.UTC().Equal(entry.createdAt) {
			continue
		}
		delete(s.items, entry.responseID)
		// A link without a conversation can never resume anything once its
		// payload is gone.
		if link, ok := s.links[entry.responseID]; ok && link.ConversationID == "" {
			delete(s.links, entry.responseID)
		}
		removed++
	}
	return removed
}

func (s *responseStore) deleteByConversationOrThread(conversationID string, threadID string) int {
	if s == nil {
		return 0
	}
	conversationID = strings.TrimSpace(conversationID)
	threadID = strings.TrimSpace(threadID)
	if conversationID == "" && threadID == "" {
		return 0
	}
	s.ensureInitialized()
	removed := 0
	for responseID, record := range s.links {
		if (conversationID != "" && strings.TrimSpace(record.ConversationID) == conversationID) ||
			(threadID != "" && strings.TrimSpace(record.ThreadID) == threadID) {
			delete(s.items, responseID)
			delete(s.links, responseID)
			removed++
		}
	}
	return removed
}

// pruneOrphanedResponseLinks drops continuation links whose conversation is no
// longer resident: getContinuationResponse only resumes a conversation it can
// load from memory, so such a link is dead weight that would otherwise grow
// with every response ever served. The conversation lookup runs without s.mu
// held because conversations() takes it.
func (s *ServerState) pruneOrphanedResponseLinks() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	var links map[string]string
	if s.ResponseStore != nil {
		links = make(map[string]string, len(s.ResponseStore.links))
		for responseID, link := range s.ResponseStore.links {
			if _, live := s.ResponseStore.items[responseID]; live {
				continue
			}
			links[responseID] = link.ConversationID
		}
	}
	s.mu.RUnlock()
	if len(links) == 0 {
		return 0
	}
	conversations := s.conversations()
	orphaned := make([]string, 0)
	for responseID, conversationID := range links {
		if conversationID == "" || !conversations.Contains(conversationID) {
			orphaned = append(orphaned, responseID)
		}
	}
	if len(orphaned) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ResponseStore == nil {
		return 0
	}
	removed := 0
	for _, responseID := range orphaned {
		if _, live := s.ResponseStore.items[responseID]; live {
			continue
		}
		if _, ok := s.ResponseStore.links[responseID]; ok {
			delete(s.ResponseStore.links, responseID)
			removed++
		}
	}
	return removed
}
