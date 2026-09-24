package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errConversationInProgress reports a second turn arriving on a conversation
// that already has one executing. Callers can match it with errors.Is.
var errConversationInProgress = errors.New("conversation already has a turn in progress")

// errConversationDeleting reports a turn arriving on a conversation whose
// upstream thread is currently being claimed for deletion. Callers can match
// it with errors.Is.
var errConversationDeleting = errors.New("conversation is being deleted")

const maxConversationEntries = 1000

type ConversationAttachment struct {
	Name          string `json:"name,omitempty"`
	ContentType   string `json:"content_type,omitempty"`
	Source        string `json:"source,omitempty"`
	URL           string `json:"url,omitempty"`
	Path          string `json:"path,omitempty"`
	SizeBytes     int    `json:"size_bytes,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

type ConversationMessage struct {
	RequestedModel     string                   `json:"requested_model,omitempty"`
	ModelSelectionMode string                   `json:"model_selection_mode,omitempty"`
	ModelObservations  []ModelObservation       `json:"model_observations,omitempty"`
	ID                 string                   `json:"id"`
	Role               string                   `json:"role"`
	Status             string                   `json:"status"`
	Content            string                   `json:"content"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	Attachments        []ConversationAttachment `json:"attachments,omitempty"`
	// EditedAt marks content an operator rewrote by hand, so the UI can show
	// that the stored text is no longer exactly what the model produced.
	EditedAt *time.Time `json:"edited_at,omitempty"`
	// StepType is set on role="step" entries and on attachment steps. A "step"
	// entry is an intermediate upstream step (a tool call, a search, a thinking
	// pass) that Notion renders as part of a process trail rather than as a chat
	// bubble. The raw upstream type is preserved verbatim instead of being
	// mapped onto a fixed vocabulary, so an unfamiliar step still renders with
	// its own name rather than being dropped or mislabelled.
	StepType string `json:"step_type,omitempty"`
}

type ConversationEntry struct {
	ID                 string                   `json:"id"`
	Title              string                   `json:"title"`
	Origin             string                   `json:"origin,omitempty"`
	RemoteOnly         bool                     `json:"remote_only,omitempty"`
	Ephemeral          bool                     `json:"ephemeral,omitempty"`
	EphemeralReason    string                   `json:"ephemeral_reason,omitempty"`
	AutoDeleteAt       *time.Time               `json:"auto_delete_at,omitempty"`
	Source             string                   `json:"source"`
	Transport          string                   `json:"transport"`
	ClientScope        string                   `json:"client_scope,omitempty"`
	HiddenPrompt       string                   `json:"hidden_prompt,omitempty"`
	RequestFingerprint string                   `json:"request_fingerprint,omitempty"`
	Status             string                   `json:"status"`
	Model              string                   `json:"model"`
	NotionModel        string                   `json:"notion_model,omitempty"`
	UseWebSearch       bool                     `json:"use_web_search"`
	RequestPrompt      string                   `json:"request_prompt,omitempty"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	ResponseID         string                   `json:"response_id,omitempty"`
	CompletionID       string                   `json:"completion_id,omitempty"`
	ThreadID           string                   `json:"thread_id,omitempty"`
	TraceID            string                   `json:"trace_id,omitempty"`
	MessageID          string                   `json:"message_id,omitempty"`
	AccountEmail       string                   `json:"account_email,omitempty"`
	SpaceID            string                   `json:"space_id,omitempty"`
	SpaceViewID        string                   `json:"space_view_id,omitempty"`
	CreatedByDisplay   string                   `json:"created_by_display_name,omitempty"`
	Error              string                   `json:"error,omitempty"`
	InputAttachments   []ConversationAttachment `json:"input_attachments,omitempty"`
	OutputAttachments  []UploadedAttachment     `json:"output_attachments,omitempty"`
	Messages           []ConversationMessage    `json:"messages,omitempty"`
	cachedPreview      string                   `json:"-"`
}

type ConversationSummary struct {
	ID                    string     `json:"id"`
	Title                 string     `json:"title"`
	Origin                string     `json:"origin,omitempty"`
	RemoteOnly            bool       `json:"remote_only,omitempty"`
	Ephemeral             bool       `json:"ephemeral,omitempty"`
	EphemeralReason       string     `json:"ephemeral_reason,omitempty"`
	AutoDeleteAt          *time.Time `json:"auto_delete_at,omitempty"`
	Source                string     `json:"source"`
	Transport             string     `json:"transport"`
	Status                string     `json:"status"`
	Model                 string     `json:"model"`
	UseWebSearch          bool       `json:"use_web_search"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	ThreadID              string     `json:"thread_id,omitempty"`
	TraceID               string     `json:"trace_id,omitempty"`
	MessageID             string     `json:"message_id,omitempty"`
	ResponseID            string     `json:"response_id,omitempty"`
	CompletionID          string     `json:"completion_id,omitempty"`
	AccountEmail          string     `json:"account_email,omitempty"`
	SpaceID               string     `json:"space_id,omitempty"`
	CreatedByDisplay      string     `json:"created_by_display_name,omitempty"`
	Error                 string     `json:"error,omitempty"`
	Preview               string     `json:"preview,omitempty"`
	MessageCount          int        `json:"message_count"`
	InputAttachmentCount  int        `json:"input_attachment_count"`
	OutputAttachmentCount int        `json:"output_attachment_count"`
}

type ConversationEvent struct {
	Type           string               `json:"type"`
	ConversationID string               `json:"conversation_id"`
	At             time.Time            `json:"at"`
	Delta          string               `json:"delta,omitempty"`
	Error          string               `json:"error,omitempty"`
	Summary        *ConversationSummary `json:"summary,omitempty"`
	Conversation   *ConversationEntry   `json:"conversation,omitempty"`
	Message        *ConversationMessage `json:"message,omitempty"`
}

type ConversationCreateRequest struct {
	PreferredID        string
	Ephemeral          bool
	EphemeralReason    string
	AutoDeleteAt       time.Time
	Source             string
	Transport          string
	ClientScope        string
	Model              string
	NotionModel        string
	Prompt             string
	UseWebSearch       bool
	InputAttachments   []ConversationAttachment
	History            []conversationPromptSegment
	HiddenPrompt       string
	RequestFingerprint string
	Replay             bool
}

type ConversationStore struct {
	mu        sync.RWMutex
	items     map[string]*ConversationEntry
	order     []string
	subs      map[int]chan ConversationEvent
	nextSubID int
	// streams holds the append buffer of each assistant message being
	// streamed, so a delta costs O(len(delta)) instead of re-concatenating
	// the whole answer. Guarded by mu.
	streams map[string]*conversationStreamBuffer
	// sweepRetryAt keeps a conversation whose cleanup failed out of the next
	// cleanup batches for a while, so a few undeletable entries cannot starve
	// every later batch. Guarded by mu.
	sweepRetryAt map[string]time.Time
	// ephemeralTTLNanos is read on the streaming hot path (every delta goes
	// through conversations()), so it is atomic rather than guarded by mu.
	ephemeralTTLNanos atomic.Int64

	// persistMu serialises snapshot writes so the row in SQLite is always
	// the latest state, and guards the per-conversation delta throttle.
	persistMu     sync.Mutex
	persistLastAt map[string]time.Time
	persistDirty  map[string]struct{}
}

// conversationStreamBuffer accumulates one streaming assistant message.
// strings.Builder.String does not copy and later appends never touch bytes
// an earlier String result covers, so published snapshots stay immutable.
type conversationStreamBuffer struct {
	messageID     string
	buf           strings.Builder
	previewFrozen bool
}

// conversationPersistDeltaInterval bounds how often a streaming conversation
// is written to SQLite. Terminal states (complete/fail) always persist.
const conversationPersistDeltaInterval = time.Second

// conversationSweepRetryDelay is how long a conversation whose cleanup failed
// stays out of the cleanup batches.
const conversationSweepRetryDelay = 10 * time.Minute

// SetEphemeralTTL overrides the lifetime granted to an ephemeral conversation
// when a turn finishes. Zero or negative means "no override", which leaves each
// path's own built-in lifetime in place.
func (s *ConversationStore) SetEphemeralTTL(ttl time.Duration) {
	if s == nil {
		return
	}
	if ttl < 0 {
		ttl = 0
	}
	s.ephemeralTTLNanos.Store(int64(ttl))
}

// ephemeralTTL is the lifetime to grant a finishing ephemeral conversation.
// Safe to call with or without mu held.
func (s *ConversationStore) ephemeralTTL() time.Duration {
	if ttl := time.Duration(s.ephemeralTTLNanos.Load()); ttl > 0 {
		return ttl
	}
	return sillyTavernQuietConversationTTL
}

func newConversationStore() *ConversationStore {
	return &ConversationStore{
		items:         map[string]*ConversationEntry{},
		subs:          map[int]chan ConversationEvent{},
		streams:       map[string]*conversationStreamBuffer{},
		sweepRetryAt:  map[string]time.Time{},
		persistLastAt: map[string]time.Time{},
		persistDirty:  map[string]struct{}{},
	}
}

func newConversationStoreFromEntries(entries []ConversationEntry) *ConversationStore {
	store := newConversationStore()
	for _, entry := range entries {
		cloned := cloneConversationEntry(&entry)
		refreshConversationDerivedFields(&cloned)
		store.items[cloned.ID] = &cloned
		store.order = append(store.order, cloned.ID)
	}
	store.trimLocked()
	return store
}

func summarizeInputAttachments(items []InputAttachment) []ConversationAttachment {
	out := make([]ConversationAttachment, 0, len(items))
	for _, item := range items {
		contentSHA256 := ""
		if len(item.Data) > 0 {
			digest := sha256.Sum256(item.Data)
			contentSHA256 = hex.EncodeToString(digest[:])
		}
		out = append(out, ConversationAttachment{
			Name:          strings.TrimSpace(item.Name),
			ContentType:   strings.TrimSpace(item.ContentType),
			Source:        strings.TrimSpace(item.Source),
			URL:           strings.TrimSpace(item.URL),
			Path:          strings.TrimSpace(item.Path),
			SizeBytes:     len(item.Data),
			ContentSHA256: contentSHA256,
		})
	}
	return out
}

func summarizeUploadedAttachments(items []UploadedAttachment) []ConversationAttachment {
	out := make([]ConversationAttachment, 0, len(items))
	for _, item := range items {
		out = append(out, ConversationAttachment{
			Name:        strings.TrimSpace(item.Name),
			ContentType: strings.TrimSpace(item.ContentType),
			Source:      strings.TrimSpace(item.Source),
			URL:         strings.TrimSpace(firstNonEmpty(item.SignedGetURL, item.AttachmentURL)),
			SizeBytes:   item.SizeBytes,
		})
	}
	return out
}

// maxConversationTitleRunes bounds a title, whether derived from the first
// prompt or typed by an operator.
const maxConversationTitleRunes = 72

// maxConversationMessageRunes bounds a hand-edited message. The HTTP body limit
// already caps the request, so this only exists to keep a single message from
// dominating the in-memory store and the persisted snapshot.
const maxConversationMessageRunes = 200000

func conversationTitle(prompt string, attachments []ConversationAttachment) string {
	prompt = collapseWhitespace(prompt)
	if prompt != "" {
		return truncateRunes(prompt, maxConversationTitleRunes)
	}
	if len(attachments) > 0 {
		names := make([]string, 0, minInt(len(attachments), 2))
		for i := 0; i < len(attachments) && i < 2; i++ {
			name := strings.TrimSpace(attachments[i].Name)
			if name == "" {
				name = fmt.Sprintf("attachment-%d", i+1)
			}
			names = append(names, name)
		}
		return truncateRunes("Attachment · "+strings.Join(names, ", "), maxConversationTitleRunes)
	}
	return "Untitled conversation"
}

func collapseWhitespace(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func cloneConversationAttachments(items []ConversationAttachment) []ConversationAttachment {
	if len(items) == 0 {
		return nil
	}
	out := make([]ConversationAttachment, len(items))
	copy(out, items)
	return out
}

func cloneUploadedAttachments(items []UploadedAttachment) []UploadedAttachment {
	if len(items) == 0 {
		return nil
	}
	out := make([]UploadedAttachment, len(items))
	for i, item := range items {
		out[i] = item
		if len(item.Metadata) > 0 {
			out[i].Metadata = cloneStringAnyMap(item.Metadata)
		}
	}
	return out
}

func cloneStringAnyMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = cloneStringAnyMap(typed)
		case []any:
			out[key] = cloneAnySlice(typed)
		default:
			out[key] = typed
		}
	}
	return out
}

func cloneConversationMessage(msg ConversationMessage) ConversationMessage {
	msg.ModelObservations = append([]ModelObservation(nil), msg.ModelObservations...)
	msg.Attachments = cloneConversationAttachments(msg.Attachments)
	msg.EditedAt = cloneTimePointer(msg.EditedAt)
	return msg
}

func cloneConversationEntry(entry *ConversationEntry) ConversationEntry {
	if entry == nil {
		return ConversationEntry{}
	}
	out := *entry
	out.AutoDeleteAt = cloneTimePointer(entry.AutoDeleteAt)
	out.InputAttachments = cloneConversationAttachments(entry.InputAttachments)
	out.OutputAttachments = cloneUploadedAttachments(entry.OutputAttachments)
	if len(entry.Messages) > 0 {
		out.Messages = make([]ConversationMessage, len(entry.Messages))
		for i, msg := range entry.Messages {
			out.Messages[i] = cloneConversationMessage(msg)
		}
	}
	return out
}

func copyConversationEntryValue(entry *ConversationEntry) ConversationEntry {
	if entry == nil {
		return ConversationEntry{}
	}
	return *entry
}

func buildConversationSummary(entry *ConversationEntry) ConversationSummary {
	preview := entry.cachedPreview
	if preview == "" && len(entry.Messages) > 0 {
		preview = conversationPreviewFromMessages(entry.Messages)
	}
	return ConversationSummary{
		ID:                    entry.ID,
		Title:                 entry.Title,
		Origin:                firstNonEmpty(strings.TrimSpace(entry.Origin), "local"),
		RemoteOnly:            entry.RemoteOnly,
		Ephemeral:             entry.Ephemeral,
		EphemeralReason:       entry.EphemeralReason,
		AutoDeleteAt:          cloneTimePointer(entry.AutoDeleteAt),
		Source:                entry.Source,
		Transport:             entry.Transport,
		Status:                entry.Status,
		Model:                 entry.Model,
		UseWebSearch:          entry.UseWebSearch,
		CreatedAt:             entry.CreatedAt,
		UpdatedAt:             entry.UpdatedAt,
		ThreadID:              entry.ThreadID,
		TraceID:               entry.TraceID,
		MessageID:             entry.MessageID,
		ResponseID:            entry.ResponseID,
		CompletionID:          entry.CompletionID,
		AccountEmail:          entry.AccountEmail,
		SpaceID:               entry.SpaceID,
		CreatedByDisplay:      entry.CreatedByDisplay,
		Error:                 entry.Error,
		Preview:               preview,
		MessageCount:          len(entry.Messages),
		InputAttachmentCount:  len(entry.InputAttachments),
		OutputAttachmentCount: len(entry.OutputAttachments),
	}
}

func conversationPreviewFromMessages(messages []ConversationMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		// A process step carries tool/search text, not something a person said,
		// so it would read as a stray fragment if it became the preview.
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "step") {
			continue
		}
		text := collapseWhitespace(messages[i].Content)
		if text == "" && len(messages[i].Attachments) > 0 {
			text = fmt.Sprintf("%d attachments", len(messages[i].Attachments))
		}
		if text != "" {
			return truncateRunes(text, 96)
		}
	}
	return ""
}

func refreshConversationDerivedFields(entry *ConversationEntry) {
	if entry == nil {
		return
	}
	entry.cachedPreview = conversationPreviewFromMessages(entry.Messages)
}

func conversationMessageSegments(entry *ConversationEntry) []conversationPromptSegment {
	if entry == nil || len(entry.Messages) == 0 {
		return nil
	}
	segments := make([]conversationPromptSegment, 0, len(entry.Messages))
	for _, msg := range entry.Messages {
		role := strings.TrimSpace(strings.ToLower(msg.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		segments = append(segments, conversationPromptSegment{
			Role: role,
			Text: text,
		})
	}
	return segments
}

func conversationSegmentsMatchSuffix(entrySegments []conversationPromptSegment, history []conversationPromptSegment) bool {
	if len(entrySegments) == 0 || len(history) == 0 {
		return false
	}
	shorter := entrySegments
	longer := history
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	offset := len(longer) - len(shorter)
	for idx := range shorter {
		if longer[offset+idx].Role != shorter[idx].Role {
			return false
		}
		if longer[offset+idx].Text != shorter[idx].Text {
			return false
		}
	}
	return true
}

func (s *ConversationStore) broadcast(event ConversationEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ch := range s.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

// moveToFrontLocked shifts the entry to the front in place. It runs for every
// streamed delta, so it must not allocate a new order slice each time.
func (s *ConversationStore) moveToFrontLocked(id string) {
	if len(s.order) == 0 || s.order[0] == id {
		return
	}
	for i, itemID := range s.order {
		if itemID != id {
			continue
		}
		copy(s.order[1:i+1], s.order[:i])
		s.order[0] = id
		return
	}
	s.order = append([]string{id}, s.order...)
}

func (s *ConversationStore) trimLocked() {
	for len(s.order) > maxConversationEntries {
		last := s.order[len(s.order)-1]
		delete(s.items, last)
		s.order = s.order[:len(s.order)-1]
	}
}

func (s *ConversationStore) Create(req ConversationCreateRequest) ConversationEntry {
	now := time.Now().UTC()
	id := strings.TrimSpace(req.PreferredID)
	if id == "" {
		id = "conv_" + strings.ReplaceAll(randomUUID(), "-", "")
	}
	entry := ConversationEntry{
		ID:                 id,
		Title:              conversationTitle(req.Prompt, req.InputAttachments),
		Origin:             "local",
		Ephemeral:          req.Ephemeral,
		EphemeralReason:    strings.TrimSpace(req.EphemeralReason),
		AutoDeleteAt:       timePointer(req.AutoDeleteAt),
		Source:             firstNonEmpty(req.Source, "api"),
		Transport:          firstNonEmpty(req.Transport, "responses"),
		ClientScope:        strings.TrimSpace(req.ClientScope),
		HiddenPrompt:       strings.TrimSpace(req.HiddenPrompt),
		RequestFingerprint: req.RequestFingerprint,
		Status:             "running",
		Model:              strings.TrimSpace(req.Model),
		NotionModel:        strings.TrimSpace(req.NotionModel),
		UseWebSearch:       req.UseWebSearch,
		RequestPrompt:      strings.TrimSpace(req.Prompt),
		CreatedAt:          now,
		UpdatedAt:          now,
		InputAttachments:   cloneConversationAttachments(req.InputAttachments),
		OutputAttachments:  nil,
	}
	history := exactConversationSegments(req.History)
	if len(history) > 0 && history[len(history)-1].Role == "user" {
		history = history[:len(history)-1]
	}
	for _, segment := range history {
		entry.Messages = append(entry.Messages, ConversationMessage{
			ID:   "msg_history_" + strings.ReplaceAll(randomUUID(), "-", ""),
			Role: segment.Role, Content: segment.Text, Status: "completed", CreatedAt: now, UpdatedAt: now,
		})
	}
	if entry.RequestPrompt != "" || len(entry.InputAttachments) > 0 {
		entry.Messages = append(entry.Messages, ConversationMessage{
			ID:          "msg_user_" + strings.ReplaceAll(randomUUID(), "-", ""),
			Role:        "user",
			Status:      "completed",
			Content:     entry.RequestPrompt,
			CreatedAt:   now,
			UpdatedAt:   now,
			Attachments: cloneConversationAttachments(entry.InputAttachments),
		})
	}
	refreshConversationDerivedFields(&entry)

	s.mu.Lock()
	if s.items[id] != nil {
		id = "conv_" + strings.ReplaceAll(randomUUID(), "-", "")
		entry.ID = id
	}
	entryPtr := &entry
	s.items[id] = entryPtr
	s.order = append([]string{id}, s.order...)
	s.trimLocked()
	cloned := copyConversationEntryValue(entryPtr)
	summary := buildConversationSummary(entryPtr)
	s.mu.Unlock()

	s.broadcast(ConversationEvent{
		Type:           "conversation.created",
		ConversationID: id,
		At:             now,
		Summary:        &summary,
		Conversation:   entryPtr,
	})
	return cloned
}

func (s *ConversationStore) Continue(conversationID string, req ConversationCreateRequest) (ConversationEntry, error) {
	now := time.Now().UTC()
	var (
		cloned  ConversationEntry
		summary ConversationSummary
		ok      bool
		entry   *ConversationEntry
	)
	s.mu.Lock()
	current := s.items[conversationID]
	if current != nil {
		// Two turns racing on one conversation would interleave upstream
		// thread operations, so only a terminal status may start a new turn.
		if conversationStatusBusy(current.Status) {
			s.mu.Unlock()
			return ConversationEntry{}, fmt.Errorf("%w: %s", errConversationInProgress, conversationID)
		}
		if strings.EqualFold(strings.TrimSpace(current.Status), "deleting") {
			s.mu.Unlock()
			return ConversationEntry{}, fmt.Errorf("%w: %s", errConversationDeleting, conversationID)
		}
		if req.Replay {
			cloned = copyConversationEntryValue(current)
			s.mu.Unlock()
			return cloned, nil
		}
		delete(s.streams, conversationID)
		next := cloneConversationEntry(current)
		next.HiddenPrompt = firstNonEmpty(req.HiddenPrompt, next.HiddenPrompt)
		next.RequestFingerprint = req.RequestFingerprint
		next.Source = firstNonEmpty(req.Source, next.Source)
		next.Transport = firstNonEmpty(req.Transport, next.Transport)
		if req.Ephemeral {
			next.Ephemeral = true
			next.EphemeralReason = firstNonEmpty(strings.TrimSpace(req.EphemeralReason), next.EphemeralReason)
			if !req.AutoDeleteAt.IsZero() {
				next.AutoDeleteAt = timePointer(req.AutoDeleteAt)
			}
		}
		if clean := strings.TrimSpace(req.Model); clean != "" {
			next.Model = clean
		}
		if clean := strings.TrimSpace(req.NotionModel); clean != "" {
			next.NotionModel = clean
		}
		next.UseWebSearch = req.UseWebSearch
		next.Status = "running"
		next.Error = ""
		next.InputAttachments = cloneConversationAttachments(req.InputAttachments)
		next.UpdatedAt = now
		if len(next.Messages) > 0 {
			last := &next.Messages[len(next.Messages)-1]
			if last.Role == "assistant" && last.Status != "completed" {
				last.Status = "failed"
				last.UpdatedAt = now
			}
		}
		if strings.TrimSpace(req.Prompt) != "" || len(req.InputAttachments) > 0 {
			next.Messages = append(next.Messages, ConversationMessage{
				ID:          "msg_user_" + strings.ReplaceAll(randomUUID(), "-", ""),
				Role:        "user",
				Status:      "completed",
				Content:     strings.TrimSpace(req.Prompt),
				CreatedAt:   now,
				UpdatedAt:   now,
				Attachments: cloneConversationAttachments(req.InputAttachments),
			})
		}
		refreshConversationDerivedFields(&next)
		entry = &next
		s.items[conversationID] = entry
		s.moveToFrontLocked(conversationID)
		cloned = copyConversationEntryValue(entry)
		summary = buildConversationSummary(entry)
		ok = true
	}
	s.mu.Unlock()
	if !ok {
		return ConversationEntry{}, fmt.Errorf("conversation not found")
	}
	s.broadcast(ConversationEvent{
		Type:           "conversation.updated",
		ConversationID: conversationID,
		At:             now,
		Summary:        &summary,
		Conversation:   entry,
	})
	return cloned, nil
}

func (s *ConversationStore) ensureAssistantMessageLocked(entry *ConversationEntry, now time.Time) *ConversationMessage {
	if len(entry.Messages) > 0 {
		last := &entry.Messages[len(entry.Messages)-1]
		if last.Role == "assistant" && last.Status != "completed" {
			last.UpdatedAt = now
			return last
		}
	}
	entry.Messages = append(entry.Messages, ConversationMessage{
		ID:        "msg_assistant_" + strings.ReplaceAll(randomUUID(), "-", ""),
		Role:      "assistant",
		Status:    "streaming",
		CreatedAt: now,
		UpdatedAt: now,
	})
	return &entry.Messages[len(entry.Messages)-1]
}

func (s *ConversationStore) SetEnvelopeIDs(conversationID string, responseID string, completionID string) {
	now := time.Now().UTC()
	var (
		summary ConversationSummary
		ok      bool
		entry   *ConversationEntry
	)
	s.mu.Lock()
	current := s.items[conversationID]
	if current != nil {
		next := cloneConversationEntry(current)
		if strings.TrimSpace(responseID) != "" {
			next.ResponseID = strings.TrimSpace(responseID)
		}
		if strings.TrimSpace(completionID) != "" {
			next.CompletionID = strings.TrimSpace(completionID)
		}
		next.UpdatedAt = now
		refreshConversationDerivedFields(&next)
		entry = &next
		s.items[conversationID] = entry
		s.moveToFrontLocked(conversationID)
		summary = buildConversationSummary(entry)
		ok = true
	}
	s.mu.Unlock()
	if ok {
		s.broadcast(ConversationEvent{
			Type:           "conversation.updated",
			ConversationID: conversationID,
			At:             now,
			Summary:        &summary,
			Conversation:   entry,
		})
	}
}

// AppendAssistantDelta is the streaming hot path. It keeps the store's
// copy-on-write contract (published entries are never mutated) but only
// copies what changes: the entry header and the message slice, not every
// attachment and observation, and the answer text grows through an append
// buffer instead of being re-concatenated for every delta.
func (s *ConversationStore) AppendAssistantDelta(conversationID string, delta string) {
	delta = strings.TrimRight(delta, "\r")
	if delta == "" {
		return
	}
	now := time.Now().UTC()
	var (
		summary ConversationSummary
		msg     *ConversationMessage
		ok      bool
		entry   *ConversationEntry
	)
	s.mu.Lock()
	current := s.items[conversationID]
	if current != nil && conversationStatusBusy(current.Status) {
		next := *current
		next.Messages = make([]ConversationMessage, len(current.Messages), len(current.Messages)+1)
		copy(next.Messages, current.Messages)
		assistant := s.ensureAssistantMessageLocked(&next, now)
		if s.streams == nil {
			s.streams = map[string]*conversationStreamBuffer{}
		}
		stream := s.streams[conversationID]
		if stream == nil || stream.messageID != assistant.ID || stream.buf.Len() != len(assistant.Content) {
			// First delta of this message, or its content changed through
			// another path: restart the buffer from the published content.
			stream = &conversationStreamBuffer{messageID: assistant.ID}
			stream.buf.WriteString(assistant.Content)
			s.streams[conversationID] = stream
		}
		stream.buf.WriteString(delta)
		assistant.Content = stream.buf.String()
		assistant.Status = "streaming"
		assistant.UpdatedAt = now
		next.Status = "running"
		next.UpdatedAt = now
		// The preview is the first ~96 visible runes of the latest message.
		// Appending never changes an already-collapsed prefix, so once the
		// preview is full it stays valid and need not be recomputed.
		if !stream.previewFrozen {
			refreshConversationDerivedFields(&next)
			stream.previewFrozen = len([]rune(collapseWhitespace(assistant.Content))) > 96
		}
		entry = &next
		s.items[conversationID] = entry
		s.moveToFrontLocked(conversationID)
		summary = buildConversationSummary(entry)
		msg = assistant
		ok = true
	}
	s.mu.Unlock()
	if ok {
		s.broadcast(ConversationEvent{
			Type:           "conversation.delta",
			ConversationID: conversationID,
			At:             now,
			Delta:          delta,
			Summary:        &summary,
			Conversation:   entry,
			Message:        msg,
		})
	}
}

func (s *ConversationStore) Complete(conversationID string, result InferenceResult) {
	if result.cachedReplay {
		return
	}
	now := time.Now().UTC()
	var (
		summary ConversationSummary
		ok      bool
		entry   *ConversationEntry
	)
	s.mu.Lock()
	current := s.items[conversationID]
	if current != nil {
		delete(s.streams, conversationID)
		next := cloneConversationEntry(current)
		next.Status = "completed"
		next.UpdatedAt = now
		if next.Ephemeral {
			next.AutoDeleteAt = timePointer(now.Add(s.ephemeralTTL()))
		}
		next.ThreadID = strings.TrimSpace(result.ThreadID)
		next.TraceID = strings.TrimSpace(result.TraceID)
		next.MessageID = strings.TrimSpace(result.MessageID)
		next.AccountEmail = strings.TrimSpace(result.AccountEmail)
		next.SpaceID = firstNonEmpty(result.SpaceID, next.SpaceID)
		next.SpaceViewID = firstNonEmpty(result.SpaceViewID, next.SpaceViewID)
		next.Error = ""
		next.OutputAttachments = cloneUploadedAttachments(result.Attachments)
		assistant := s.ensureAssistantMessageLocked(&next, now)
		assistant.ID = firstNonEmpty(strings.TrimSpace(result.MessageID), assistant.ID)
		assistant.Status = "completed"
		assistant.Content = sanitizeAssistantVisibleText(result.Text)
		assistant.RequestedModel = firstNonEmpty(result.Model, next.Model)
		assistant.ModelSelectionMode = result.ModelSelectionMode
		assistant.ModelObservations = append([]ModelObservation(nil), result.ModelObservations...)
		assistant.Attachments = summarizeUploadedAttachments(result.Attachments)
		assistant.UpdatedAt = now
		log.Printf("[models] conversation=%s requested=%q selection=%q observed=%+v", conversationID, assistant.RequestedModel, assistant.ModelSelectionMode, assistant.ModelObservations)
		if len(next.Messages) > 0 {
			next.Messages[len(next.Messages)-1] = cloneConversationMessage(*assistant)
		}
		refreshConversationDerivedFields(&next)
		entry = &next
		s.items[conversationID] = entry
		s.moveToFrontLocked(conversationID)
		summary = buildConversationSummary(entry)
		ok = true
	}
	s.mu.Unlock()
	if ok {
		s.broadcast(ConversationEvent{
			Type:           "conversation.completed",
			ConversationID: conversationID,
			At:             now,
			Summary:        &summary,
			Conversation:   entry,
		})
	}
}

func (s *ConversationStore) Fail(conversationID string, err error) {
	if err == nil {
		return
	}
	now := time.Now().UTC()
	message := strings.TrimSpace(err.Error())
	var (
		summary ConversationSummary
		ok      bool
		entry   *ConversationEntry
	)
	s.mu.Lock()
	current := s.items[conversationID]
	if current != nil {
		delete(s.streams, conversationID)
		next := cloneConversationEntry(current)
		next.Status = "failed"
		next.Error = message
		next.UpdatedAt = now
		if next.Ephemeral {
			next.AutoDeleteAt = timePointer(now.Add(s.ephemeralTTL()))
		}
		if len(next.Messages) > 0 {
			last := &next.Messages[len(next.Messages)-1]
			if last.Role == "assistant" && last.Status != "completed" {
				last.Status = "failed"
				last.UpdatedAt = now
			}
		}
		refreshConversationDerivedFields(&next)
		entry = &next
		s.items[conversationID] = entry
		s.moveToFrontLocked(conversationID)
		summary = buildConversationSummary(entry)
		ok = true
	}
	s.mu.Unlock()
	if ok {
		s.broadcast(ConversationEvent{
			Type:           "conversation.failed",
			ConversationID: conversationID,
			At:             now,
			Error:          message,
			Summary:        &summary,
			Conversation:   entry,
		})
	}
}

func (s *ConversationStore) Delete(conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.items[conversationID]
	if entry == nil {
		return fmt.Errorf("conversation not found")
	}
	if entry.Status == "running" {
		return fmt.Errorf("conversation is still running")
	}
	delete(s.items, conversationID)
	delete(s.streams, conversationID)
	delete(s.sweepRetryAt, conversationID)
	next := make([]string, 0, len(s.order))
	for _, id := range s.order {
		if id != conversationID {
			next = append(next, id)
		}
	}
	s.order = next
	return nil
}

func (s *ConversationStore) List() []ConversationSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]ConversationSummary, 0, len(s.order))
	for _, id := range s.order {
		entry := s.items[id]
		if entry == nil {
			continue
		}
		items = append(items, buildConversationSummary(entry))
	}
	return items
}

func (s *ConversationStore) ListExpiredEphemeral(now time.Time, limit int) []ConversationEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = len(s.order)
	}
	items := make([]ConversationEntry, 0, minInt(limit, len(s.order)))
	for _, id := range s.order {
		entry := s.items[id]
		if entry == nil || !entry.Ephemeral {
			continue
		}
		if conversationStatusBusy(entry.Status) || s.sweepDeferredLocked(id, now) {
			continue
		}
		if entry.AutoDeleteAt == nil || entry.AutoDeleteAt.After(now) {
			continue
		}
		items = append(items, copyConversationEntryValue(entry))
		if len(items) >= limit {
			break
		}
	}
	return items
}

// ListIdleConversations returns finished conversations whose last turn is older
// than idleTTL, oldest first. Ephemeral entries are skipped because they are
// swept by their own deadline, and running turns are never returned however
// stale their timestamp looks.
func (s *ConversationStore) ListIdleConversations(now time.Time, idleTTL time.Duration, limit int) []ConversationEntry {
	if s == nil || idleTTL <= 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = len(s.order)
	}
	cutoff := now.Add(-idleTTL)
	items := make([]ConversationEntry, 0, minInt(limit, len(s.order)))
	for _, id := range s.order {
		entry := s.items[id]
		if entry == nil || entry.Ephemeral {
			continue
		}
		if conversationStatusBusy(entry.Status) || s.sweepDeferredLocked(id, now) {
			continue
		}
		last := entry.UpdatedAt
		if last.IsZero() {
			last = entry.CreatedAt
		}
		if last.IsZero() || !last.Before(cutoff) {
			continue
		}
		items = append(items, copyConversationEntryValue(entry))
		if len(items) >= limit {
			break
		}
	}
	return items
}

// DeferSweep keeps a conversation out of cleanup batches until the given
// time. A cleanup that failed for a reason that is not permanent is retried
// later instead of occupying a slot in every following batch.
func (s *ConversationStore) DeferSweep(conversationID string, until time.Time) {
	if s == nil || strings.TrimSpace(conversationID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sweepRetryAt == nil {
		s.sweepRetryAt = map[string]time.Time{}
	}
	s.sweepRetryAt[conversationID] = until
}

// SweepDeferred reports whether a cleanup of the conversation was deferred
// past now.
func (s *ConversationStore) SweepDeferred(conversationID string, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sweepDeferredLocked(conversationID, now)
}

func (s *ConversationStore) sweepDeferredLocked(conversationID string, now time.Time) bool {
	until, ok := s.sweepRetryAt[conversationID]
	return ok && now.Before(until)
}

// Contains reports whether the conversation is resident in memory.
func (s *ConversationStore) Contains(conversationID string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[conversationID] != nil
}

// conversationStatusBusy reports whether a conversation has a turn in flight.
// A busy entry must not be swept, retargeted, or reconciled as finished.
func conversationStatusBusy(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "running")
}

func (s *ConversationStore) Get(conversationID string) (ConversationEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.items[conversationID]
	if entry == nil {
		return ConversationEntry{}, false
	}
	return copyConversationEntryValue(entry), true
}

// SetExecutionTarget pins the upstream thread and account a running turn is
// executing against onto the conversation entry. It is written before the turn
// starts and refreshed with the thread the upload path actually returned, so if
// the process dies mid-request the entry still records what was in flight and
// startup reconciliation can fail the turn cleanly without orphaning the thread.
// Only running turns carry an execution target; finished entries are untouched.
func (s *ConversationStore) SetExecutionTarget(conversationID string, threadID string, accountEmail string, spaceID string) bool {
	conversationID = strings.TrimSpace(conversationID)
	threadID = strings.TrimSpace(threadID)
	accountEmail = strings.TrimSpace(accountEmail)
	if conversationID == "" || threadID == "" {
		return false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	entry := s.items[conversationID]
	if entry == nil || !conversationStatusBusy(entry.Status) {
		s.mu.Unlock()
		return false
	}
	next := cloneConversationEntry(entry)
	next.ThreadID = threadID
	next.AccountEmail = accountEmail
	next.SpaceID = spaceID
	next.UpdatedAt = now
	s.items[conversationID] = &next
	summary := buildConversationSummary(&next)
	s.mu.Unlock()
	s.broadcast(ConversationEvent{
		Type:           "conversation.updated",
		ConversationID: conversationID,
		At:             now,
		Summary:        &summary,
	})
	return true
}

// SetTitle replaces a conversation title. Titles are otherwise derived from the
// first prompt at creation time, so this is the only way for an operator to
// name a conversation themselves. It returns the updated entry for the caller
// to persist; the in-memory store is updated either way.
func (s *ConversationStore) SetTitle(conversationID string, title string) (ConversationEntry, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return ConversationEntry{}, fmt.Errorf("conversation id is required")
	}
	title = collapseWhitespace(title)
	if title == "" {
		return ConversationEntry{}, fmt.Errorf("title is required")
	}
	if len([]rune(title)) > maxConversationTitleRunes {
		return ConversationEntry{}, fmt.Errorf("title must be at most %d characters", maxConversationTitleRunes)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	entry := s.items[conversationID]
	if entry == nil {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation not found")
	}
	if conversationStatusBusy(entry.Status) {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation is running; stop it before renaming")
	}
	next := cloneConversationEntry(entry)
	next.Title = title
	next.UpdatedAt = now
	s.items[conversationID] = &next
	summary := buildConversationSummary(&next)
	s.mu.Unlock()

	s.broadcast(ConversationEvent{
		Type:           "conversation.updated",
		ConversationID: conversationID,
		At:             now,
		Summary:        &summary,
	})
	return copyConversationEntryValue(&next), nil
}

// SetMessageContent rewrites the stored text of one message, for either role.
// It refuses to touch a running conversation: an in-flight turn keeps appending
// deltas to the assistant message, so an edit there would be overwritten or
// interleaved. The edit is recorded with EditedAt so the transcript can say the
// text is no longer verbatim model output.
func (s *ConversationStore) SetMessageContent(conversationID string, messageID string, content string) (ConversationEntry, error) {
	conversationID = strings.TrimSpace(conversationID)
	messageID = strings.TrimSpace(messageID)
	if conversationID == "" {
		return ConversationEntry{}, fmt.Errorf("conversation id is required")
	}
	if messageID == "" {
		return ConversationEntry{}, fmt.Errorf("message id is required")
	}
	if len([]rune(content)) > maxConversationMessageRunes {
		return ConversationEntry{}, fmt.Errorf("message must be at most %d characters", maxConversationMessageRunes)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	entry := s.items[conversationID]
	if entry == nil {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation not found")
	}
	if conversationStatusBusy(entry.Status) {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation is running; stop it before editing")
	}
	index := -1
	for i := range entry.Messages {
		if strings.TrimSpace(entry.Messages[i].ID) == messageID {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("message not found")
	}
	next := cloneConversationEntry(entry)
	editedAt := now
	next.Messages[index].Content = content
	next.Messages[index].UpdatedAt = now
	next.Messages[index].EditedAt = &editedAt
	next.UpdatedAt = now
	s.items[conversationID] = &next
	summary := buildConversationSummary(&next)
	s.mu.Unlock()

	s.broadcast(ConversationEvent{
		Type:           "conversation.updated",
		ConversationID: conversationID,
		At:             now,
		Summary:        &summary,
	})
	return copyConversationEntryValue(&next), nil
}

// ClaimForDeletion marks a conversation as being deleted upstream. While the
// claim holds, continuations are rejected so no new turn can start against a
// thread that is about to disappear. If the upstream delete fails, the claim is
// released again with RestoreDeletionClaim.
func (s *ConversationStore) ClaimForDeletion(conversationID string) (ConversationEntry, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return ConversationEntry{}, fmt.Errorf("conversation id is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	entry := s.items[conversationID]
	if entry == nil {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("conversation not found")
	}
	if conversationStatusBusy(entry.Status) {
		s.mu.Unlock()
		return ConversationEntry{}, fmt.Errorf("%w: %s", errConversationInProgress, conversationID)
	}
	next := cloneConversationEntry(entry)
	next.Status = "deleting"
	next.UpdatedAt = now
	s.items[conversationID] = &next
	summary := buildConversationSummary(&next)
	s.mu.Unlock()
	s.broadcast(ConversationEvent{
		Type:           "conversation.updated",
		ConversationID: conversationID,
		At:             now,
		Summary:        &summary,
	})
	return copyConversationEntryValue(&next), nil
}

// RestoreDeletionClaim releases a deletion claim after the upstream delete
// failed, putting the conversation back into the status the caller recorded
// before claiming (typically "completed" or "failed") so it can be used again.
func (s *ConversationStore) RestoreDeletionClaim(conversationID string, restoredStatus string) error {
	conversationID = strings.TrimSpace(conversationID)
	restoredStatus = strings.TrimSpace(restoredStatus)
	if conversationID == "" || restoredStatus == "" {
		return fmt.Errorf("conversation id and status are required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	entry := s.items[conversationID]
	if entry == nil {
		s.mu.Unlock()
		return fmt.Errorf("conversation not found")
	}
	next := cloneConversationEntry(entry)
	next.Status = restoredStatus
	next.UpdatedAt = now
	s.items[conversationID] = &next
	summary := buildConversationSummary(&next)
	s.mu.Unlock()
	s.broadcast(ConversationEvent{
		Type:           "conversation.updated",
		ConversationID: conversationID,
		At:             now,
		Summary:        &summary,
	})
	return nil
}

func (s *ConversationStore) FindByThreadID(threadID string) (ConversationEntry, bool) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ConversationEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.order {
		entry := s.items[id]
		if entry == nil {
			continue
		}
		if strings.TrimSpace(entry.ThreadID) != threadID {
			continue
		}
		return copyConversationEntryValue(entry), true
	}
	return ConversationEntry{}, false
}

// FindContinuationBySegments matches a conversation whose recorded messages end
// with the request's history. It is a heuristic fallback for when the scoped
// fingerprint lookup missed (for example after a client IP or user-agent
// change), so it must never bridge different client scopes: entries carry the
// client scope they were created with, and only an exact match is allowed.
func (s *ConversationStore) FindContinuationBySegments(history []conversationPromptSegment, clientScope string) (ConversationEntry, bool) {
	normalizedHistory := exactConversationSegments(history)
	if len(normalizedHistory) == 0 {
		return ConversationEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.order {
		entry := s.items[id]
		if entry == nil {
			continue
		}
		if strings.TrimSpace(entry.ThreadID) == "" || strings.TrimSpace(strings.ToLower(entry.Status)) == "running" {
			continue
		}
		if entry.ClientScope != clientScope {
			continue
		}
		entrySegments := conversationMessageSegments(entry)
		if !exactHistorySuffix(entrySegments, normalizedHistory) {
			continue
		}
		return copyConversationEntryValue(entry), true
	}
	return ConversationEntry{}, false
}

func (s *ConversationStore) Subscribe() (int, <-chan ConversationEvent) {
	ch := make(chan ConversationEvent, 128)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSubID
	s.nextSubID++
	s.subs[id] = ch
	return id, ch
}

func (s *ConversationStore) Unsubscribe(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
}

// conversations is on the streaming hot path (pushConversationDelta calls it for
// every delta), so the common case takes only a shared read lock on the server
// state; the exclusive lock is needed just once, to create the store lazily.
// The ephemeral TTL is pushed in from ApplyConfig when config changes rather
// than recomputed per call.
func (s *ServerState) conversations() *ConversationStore {
	s.mu.RLock()
	store := s.Conversations
	s.mu.RUnlock()
	if store != nil {
		return store
	}
	s.mu.Lock()
	if s.Conversations == nil {
		s.Conversations = newConversationStore()
		s.Conversations.SetEphemeralTTL(configuredEphemeralTTLOverride(s.Config))
	}
	store = s.Conversations
	s.mu.Unlock()
	return store
}

// persistConversationSnapshot writes the conversation's current state to
// SQLite. Writes are serialised and each one reads the entry inside the
// critical section, so an older snapshot can never land after a newer one.
func (s *ServerState) persistConversationSnapshot(conversationID string) {
	if s == nil || strings.TrimSpace(conversationID) == "" {
		return
	}
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := conversationSnapshotsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled {
		return
	}
	convs := s.conversations()
	convs.persistMu.Lock()
	defer convs.persistMu.Unlock()
	if convs.persistLastAt == nil {
		convs.persistLastAt = map[string]time.Time{}
	}
	delete(convs.persistDirty, conversationID)
	entry, ok := convs.Get(conversationID)
	if !ok {
		delete(convs.persistLastAt, conversationID)
		return
	}
	if conversationStatusBusy(entry.Status) {
		convs.persistLastAt[conversationID] = time.Now()
	} else {
		// A finished turn needs no delta throttle state any more.
		delete(convs.persistLastAt, conversationID)
	}
	if err := store.SaveConversation(entry); err != nil {
		log.Printf("[sqlite] save conversation %s failed: %v", conversationID, err)
	}
}

// persistConversationDelta persists a streaming conversation at most once per
// conversationPersistDeltaInterval. Skipped deltas are marked dirty; the
// terminal complete/fail write (or flushConversationSnapshots on shutdown)
// always records the final state.
func (s *ServerState) persistConversationDelta(conversationID string) {
	if s == nil || strings.TrimSpace(conversationID) == "" {
		return
	}
	convs := s.conversations()
	now := time.Now()
	convs.persistMu.Lock()
	if last, ok := convs.persistLastAt[conversationID]; ok && now.Sub(last) < conversationPersistDeltaInterval {
		if convs.persistDirty == nil {
			convs.persistDirty = map[string]struct{}{}
		}
		convs.persistDirty[conversationID] = struct{}{}
		convs.persistMu.Unlock()
		return
	}
	convs.persistMu.Unlock()
	s.persistConversationSnapshot(conversationID)
}

// flushConversationSnapshots writes every conversation whose latest deltas
// were throttled. It runs on shutdown so a partial answer is not lost.
func (s *ServerState) flushConversationSnapshots() {
	if s == nil {
		return
	}
	convs := s.conversations()
	convs.persistMu.Lock()
	dirty := make([]string, 0, len(convs.persistDirty))
	for id := range convs.persistDirty {
		dirty = append(dirty, id)
	}
	convs.persistMu.Unlock()
	for _, id := range dirty {
		s.persistConversationSnapshot(id)
	}
}

func (s *ServerState) deleteResponsesByConversationOrThread(conversationID string, threadID string) {
	conversationID = strings.TrimSpace(conversationID)
	threadID = strings.TrimSpace(threadID)
	if conversationID == "" && threadID == "" {
		return
	}
	s.mu.Lock()
	if s.ResponseStore != nil {
		s.ResponseStore.deleteByConversationOrThread(conversationID, threadID)
	}
	sqliteWriter := s.sqliteWriter
	store := s.Store
	storeEnabled := store != nil && responsesPersistenceEnabled(s.Config)
	s.mu.Unlock()
	if storeEnabled {
		if sqliteWriter != nil {
			sqliteWriter.EnqueueDeleteResponsesByConversationOrThread(conversationID, threadID)
			return
		}
		if err := store.DeleteResponsesByConversationOrThread(conversationID, threadID); err != nil {
			log.Printf("[sqlite] delete responses conversation=%s thread=%s failed: %v", conversationID, threadID, err)
		}
	}
}

func (a *App) beginConversation(preferredConversationID string, source string, transport string, displayPrompt string, request PromptRunRequest) string {
	entry := a.State.conversations().Create(ConversationCreateRequest{
		PreferredID:        preferredConversationID,
		Ephemeral:          request.EphemeralConversation,
		EphemeralReason:    request.EphemeralReason,
		AutoDeleteAt:       request.EphemeralDeleteAfter,
		Source:             source,
		Transport:          transport,
		ClientScope:        request.ClientScope,
		History:            request.HistorySegments,
		HiddenPrompt:       request.HiddenPrompt,
		RequestFingerprint: conversationRequestFingerprint(request),
		Model:              request.PublicModel,
		NotionModel:        request.NotionModel,
		Prompt:             displayPrompt,
		UseWebSearch:       request.UseWebSearch,
		InputAttachments:   summarizeInputAttachments(request.Attachments),
	})
	a.State.persistConversationSnapshot(entry.ID)
	return entry.ID
}

func (a *App) continueConversation(conversationID string, source string, transport string, displayPrompt string, request PromptRunRequest) (string, error) {
	entry, err := a.State.conversations().Continue(conversationID, ConversationCreateRequest{
		HiddenPrompt:       request.HiddenPrompt,
		RequestFingerprint: conversationRequestFingerprint(request),
		Replay:             request.replayResult != nil,
		Ephemeral:          request.EphemeralConversation,
		EphemeralReason:    request.EphemeralReason,
		AutoDeleteAt:       request.EphemeralDeleteAfter,
		Source:             source,
		Transport:          transport,
		Model:              request.PublicModel,
		NotionModel:        request.NotionModel,
		Prompt:             displayPrompt,
		UseWebSearch:       request.UseWebSearch,
		InputAttachments:   summarizeInputAttachments(request.Attachments),
	})
	if err != nil {
		return "", err
	}
	a.State.persistConversationSnapshot(entry.ID)
	return entry.ID, nil
}

func (a *App) markConversationEnvelope(conversationID string, responseID string, completionID string) {
	if conversationID == "" {
		return
	}
	a.State.conversations().SetEnvelopeIDs(conversationID, responseID, completionID)
	a.State.persistConversationSnapshot(conversationID)
}

func (a *App) pushConversationDelta(conversationID string, delta string) {
	if conversationID == "" {
		return
	}
	a.State.conversations().AppendAssistantDelta(conversationID, delta)
	a.State.persistConversationDelta(conversationID)
}

func (a *App) completeConversation(conversationID string, result InferenceResult) {
	if conversationID == "" {
		return
	}
	a.State.conversations().Complete(conversationID, result)
	a.State.persistConversationSnapshot(conversationID)
}

func (a *App) persistConversationSession(conversationID string, request PromptRunRequest, result InferenceResult) {
	if result.cachedReplay {
		return
	}
	if request.UpstreamThreadID != "" && request.UpstreamThreadID != result.ThreadID {
		request.continuationDraft = nil
		request.continuationScaffold = nil
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || request.SuppressUpstreamThreadPersistence || strings.TrimSpace(result.ThreadID) == "" {
		return
	}
	a.State.mu.RLock()
	store := a.State.Store
	storeEnabled := store != nil && continuationSessionsPersistenceEnabled(a.State.Config)
	a.State.mu.RUnlock()
	if !storeEnabled {
		return
	}
	now := time.Now().UTC()
	sessionID := ""
	turnCount := 1
	appendStep := true
	if request.continuationDraft != nil && strings.TrimSpace(request.continuationDraft.SessionID) != "" {
		sessionID = strings.TrimSpace(request.continuationDraft.SessionID)
		if request.SessionRepeatTurn {
			turnCount = maxInt(request.continuationDraft.TurnCount, 1)
			appendStep = false
		} else {
			turnCount = maxInt(request.continuationDraft.TurnCount+1, 1)
		}
	} else {
		sessionID = "sess_" + strings.ReplaceAll(randomUUID(), "-", "")
	}
	session := ConversationSession{
		ID:               sessionID,
		ConversationID:   conversationID,
		Fingerprint:      strings.TrimSpace(request.SessionFingerprint),
		ThreadID:         strings.TrimSpace(result.ThreadID),
		AccountEmail:     strings.TrimSpace(result.AccountEmail),
		SpaceID:          strings.TrimSpace(result.SpaceID),
		SpaceViewID:      strings.TrimSpace(result.SpaceViewID),
		ConfigID:         strings.TrimSpace(result.ConfigID),
		ContextID:        strings.TrimSpace(result.ContextID),
		OriginalDatetime: strings.TrimSpace(result.OriginalDatetime),
		ModelUsed:        sessionModelUsed(request, result),
		TurnCount:        turnCount,
		RawMessageCount:  maxInt(request.RawMessageCount, 0),
		Status:           conversationSessionStatusActive,
		CreatedAt:        now,
		UpdatedAt:        now,
		LastUsedAt:       now,
	}
	if request.continuationDraft != nil {
		if existing, ok, err := store.LoadConversationSessionByConversationID(conversationID); err == nil && ok {
			session.CreatedAt = existing.CreatedAt
			if session.ConfigID == "" {
				session.ConfigID = existing.ConfigID
			}
			if session.ContextID == "" {
				session.ContextID = existing.ContextID
			}
			if session.OriginalDatetime == "" {
				session.OriginalDatetime = existing.OriginalDatetime
			}
			if session.ThreadID == "" {
				session.ThreadID = existing.ThreadID
			}
			if session.AccountEmail == "" {
				session.AccountEmail = existing.AccountEmail
			}
		}
	}
	if session.ConfigID == "" {
		session.ConfigID = randomUUID()
	} else {
		session.ConfigID = normalizeTranscriptStepID(session.ConfigID)
	}
	if session.ContextID == "" {
		session.ContextID = randomUUID()
	} else {
		session.ContextID = normalizeTranscriptStepID(session.ContextID)
	}
	if session.OriginalDatetime == "" {
		session.OriginalDatetime = isoNowMillis()
	}
	if existing, ok, err := store.LoadConversationSessionByConversationID(conversationID); err == nil && ok && strings.TrimSpace(existing.ID) != "" && existing.ID != session.ID {
		if markErr := store.MarkConversationSessionStatus(existing.ID, conversationSessionStatusStale); markErr != nil {
			log.Printf("[sqlite] stale previous continuation session conversation=%s existing=%s failed: %v", conversationID, existing.ID, markErr)
		}
	}
	if err := store.SaveConversationSession(session); err != nil {
		log.Printf("[sqlite] save continuation session conversation=%s failed: %v", conversationID, err)
		return
	}
	if !appendStep {
		return
	}
	stepIndex := turnCount - 1
	updatedConfigID := randomUUID()
	if request.continuationScaffold != nil && strings.TrimSpace(request.continuationScaffold.UpdatedConfigID) != "" {
		updatedConfigID = strings.TrimSpace(request.continuationScaffold.UpdatedConfigID)
	}
	step := ConversationSessionStep{
		SessionID:       session.ID,
		StepIndex:       stepIndex,
		UpdatedConfigID: updatedConfigID,
		ResponseID:      "",
		MessageID:       strings.TrimSpace(result.MessageID),
		CreatedAt:       now,
	}
	if entry, ok := a.State.conversations().Get(conversationID); ok {
		step.ResponseID = strings.TrimSpace(entry.ResponseID)
	}
	if err := store.SaveConversationSessionStep(step); err != nil {
		log.Printf("[sqlite] save continuation session step conversation=%s failed: %v", conversationID, err)
	}
}

func (a *App) failConversation(conversationID string, err error) {
	if conversationID == "" || err == nil {
		return
	}
	a.State.conversations().Fail(conversationID, err)
	a.State.persistConversationSnapshot(conversationID)
}

// errConversationTurnAbandoned is recorded on a turn whose handler exited
// without completing or failing it (a panic, or an early return path).
var errConversationTurnAbandoned = errors.New("turn ended without a result")

// abandonConversationTurn is deferred by every handler that starts a turn. If
// the handler leaves while the conversation is still running, the turn is
// failed here; otherwise the conversation would stay "running" forever and
// every continuation would be rejected as busy.
func (a *App) abandonConversationTurn(conversationID string) {
	if a == nil || a.State == nil || strings.TrimSpace(conversationID) == "" {
		return
	}
	entry, ok := a.State.conversations().Get(conversationID)
	if !ok || !conversationStatusBusy(entry.Status) {
		return
	}
	a.failConversation(conversationID, errConversationTurnAbandoned)
}

// sessionModelUsed records the Notion model that was actually sent upstream.
// Under auto_fallback the requested model was replaced by Auto, so recording
// the requested codename would claim a model that never ran.
func sessionModelUsed(request PromptRunRequest, result InferenceResult) string {
	if strings.EqualFold(strings.TrimSpace(result.ModelSelectionMode), "auto_fallback") {
		return strings.TrimSpace(result.NotionModel)
	}
	return firstNonEmpty(strings.TrimSpace(result.NotionModel), strings.TrimSpace(request.NotionModel))
}

func (a *App) notionClientForAccount(ctx context.Context, accountEmail string) (*NotionAIClient, error) {
	cfg, snapshot, _ := a.State.Snapshot()
	a.State.mu.RLock()
	fallbackClient := a.State.Client
	a.State.mu.RUnlock()
	if email := strings.TrimSpace(accountEmail); email != "" {
		if account, _, found := cfg.FindAccount(email); found {
			account = ensureAccountPaths(cfg, account)
			session, err := loadSessionInfoForAccountRefresh(cfg, account)
			if err != nil {
				return nil, fmt.Errorf("load account session for %s: %w", email, err)
			}
			return newNotionAIClient(session, cfg, email), nil
		}
		if len(cfg.Accounts) == 0 && canonicalEmailKey(snapshot.UserEmail) == canonicalEmailKey(email) && fallbackClient != nil {
			return fallbackClient, nil
		}
		return nil, fmt.Errorf("%w: account %s not found", errConversationOwnerGone, email)
	}
	if fallbackClient != nil {
		return fallbackClient, nil
	}
	session, err := a.loadPrimarySession(ctx, cfg, snapshot, "admin_conversation_sync")
	if err != nil {
		return nil, err
	}
	return newNotionAIClient(session, cfg, ""), nil
}

func (a *App) notionClientForWorkspace(ctx context.Context, accountEmail, workspaceID string) (*NotionAIClient, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return a.notionClientForAccount(ctx, accountEmail)
	}
	cfg, _, _ := a.State.Snapshot()
	if strings.TrimSpace(accountEmail) == "" {
		if active, _, ok := cfg.ResolveActiveAccount(); ok {
			accountEmail = active.Email
		}
	}
	account, _, ok := cfg.FindAccountWorkspace(accountEmail, workspaceID)
	if !ok {
		return nil, fmt.Errorf("%w: workspace %s for account %s is no longer available", errConversationOwnerGone, workspaceID, accountEmail)
	}
	session, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		return nil, err
	}
	return newNotionAIClient(session, cfg, account.Email), nil
}

// errConversationOwnerGone reports that the account or workspace owning a
// conversation's upstream thread no longer exists. The thread can never be
// reached again, so deleting the conversation drops the local record instead
// of retrying forever.
var errConversationOwnerGone = errors.New("conversation owner is gone")

// deleteConversation removes a conversation and, conservatively, its upstream
// thread. Conversations evicted from memory (the store keeps the newest
// maxConversationEntries) are still found in SQLite, so admin deletes and the
// cleanup sweeps also reach them.
func (a *App) deleteConversation(conversationID string) error {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return fmt.Errorf("conversation id is required")
	}
	entry, ok := a.State.conversations().Get(conversationID)
	if !ok {
		return a.deletePersistedConversation(conversationID)
	}
	if conversationStatusBusy(entry.Status) {
		return fmt.Errorf("conversation is still running")
	}
	// Claim the entry before touching the upstream thread so continuations are
	// blocked while the delete is in flight; on failure the claim is released
	// and the conversation keeps its old status.
	previousStatus := entry.Status
	if _, err := a.State.conversations().ClaimForDeletion(conversationID); err != nil {
		return err
	}
	if err := a.deleteConversationThread(entry); err != nil {
		_ = a.State.conversations().RestoreDeletionClaim(conversationID, previousStatus)
		return err
	}
	if err := a.State.conversations().Delete(conversationID); err != nil {
		return err
	}
	return a.deleteConversationLocalRecords(conversationID, entry.ThreadID)
}

// deletePersistedConversation deletes a conversation that exists only in
// SQLite. No turn can be running on it: every turn executes against the
// in-memory entry, so a "running" status here is stale from a crash.
func (a *App) deletePersistedConversation(conversationID string) error {
	store := a.State.conversationPersistenceStore()
	if store == nil {
		return fmt.Errorf("conversation not found")
	}
	entry, found, err := store.LoadConversation(conversationID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("conversation not found")
	}
	if a.State.conversations().Contains(conversationID) {
		// A turn recreated it in memory meanwhile; go through the claim path.
		return a.deleteConversation(conversationID)
	}
	if err := a.deleteConversationThread(entry); err != nil {
		return err
	}
	return a.deleteConversationLocalRecords(conversationID, entry.ThreadID)
}

// deleteConversationThread deletes the upstream thread of a conversation. It
// leaves the thread alone when another live conversation still points at it,
// and treats a vanished owner (account or workspace removed) as nothing left
// to delete upstream.
func (a *App) deleteConversationThread(entry ConversationEntry) error {
	threadID := strings.TrimSpace(entry.ThreadID)
	if threadID == "" {
		return nil
	}
	if other, ok := a.State.conversations().FindByThreadID(threadID); ok && other.ID != entry.ID {
		log.Printf("[cleanup] conversation=%s shares thread=%s with conversation=%s; keeping the upstream thread", entry.ID, threadID, other.ID)
		return nil
	}
	cfg, _, _ := a.State.Snapshot()
	timeout := time.Duration(maxInt(cfg.TimeoutSec, 10)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := a.notionClientForWorkspace(ctx, entry.AccountEmail, entry.SpaceID)
	if err != nil {
		if errors.Is(err, errConversationOwnerGone) {
			log.Printf("[cleanup] conversation=%s thread=%s owner is gone (%v); deleting the local record only", entry.ID, threadID, err)
			return nil
		}
		return err
	}
	if entry.SpaceID != "" && entry.SpaceID != client.Session.SpaceID {
		return errConversationWorkspaceMismatch
	}
	return client.deleteThread(ctx, threadID)
}

// conversationDeleted reports whether a conversation id is provably gone: it is
// absent from the in-memory store and from the persisted snapshot table. A
// continuation session can outlive a failed delete, so the resolver consults
// this before resurrecting a conversation from session state. An empty id or a
// read error proves nothing and is never treated as deleted.
func (a *App) conversationDeleted(conversationID string) bool {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || a == nil || a.State == nil {
		return false
	}
	if a.State.conversations().Contains(conversationID) {
		return false
	}
	a.State.mu.RLock()
	store := a.State.Store
	persisted := store != nil && conversationSnapshotsPersistenceEnabled(a.State.Config)
	a.State.mu.RUnlock()
	if !persisted {
		// Conversations live only in memory here, so a session row is the only
		// record that can still point at this conversation.
		return false
	}
	_, found, err := store.LoadConversation(conversationID)
	if err != nil {
		// A read failure must not silently drop a live conversation.
		return false
	}
	return !found
}

func (a *App) deleteConversationLocalRecords(conversationID string, threadID string) error {
	a.State.deleteResponsesByConversationOrThread(conversationID, threadID)
	// Remove the continuation session before the conversation row. A session
	// that outlives its conversation is exactly what lets a deleted thread be
	// revived, so a failed delete aborts here and leaves the conversation row
	// in place for a retry instead of creating that state.
	if err := a.State.deleteConversationSessionByConversationOrThread(conversationID, threadID); err != nil {
		return fmt.Errorf("delete continuation session for %s: %w", conversationID, err)
	}
	convs := a.State.conversations()
	convs.persistMu.Lock()
	delete(convs.persistLastAt, conversationID)
	delete(convs.persistDirty, conversationID)
	convs.persistMu.Unlock()
	a.State.mu.RLock()
	store := a.State.Store
	a.State.mu.RUnlock()
	if store != nil {
		if err := store.DeleteConversation(conversationID); err != nil {
			return err
		}
	}
	a.State.deleteSillyTavernBinding(conversationID)
	return nil
}

// preparePromptExecutionTarget pins an execution target onto a request before
// dispatch: a thread ID, allocated up front when the request does not already
// carry one, and the account about to serve the turn. Both are persisted on the
// conversation entry through the request's onThreadPrepared callback, so a
// crash mid-turn leaves a reconcilable record instead of a silently orphaned
// upstream thread, and a retry reuses the same thread rather than spawning a
// new one. Continuation turns carry their own scaffold and requests with an
// upstream thread are already pinned, so neither gets a new target.
func (a *App) preparePromptExecutionTarget(request *PromptRunRequest, accountEmail string, spaceID string) {
	if a == nil || a.State == nil || request == nil {
		return
	}
	if request.continuationScaffold != nil {
		return
	}
	if strings.TrimSpace(request.UpstreamThreadID) != "" {
		return
	}
	if strings.TrimSpace(request.preparedThreadID) == "" {
		request.preparedThreadID = randomUUID()
		if len(request.HistorySegments) > 1 || request.ForceLocalConversationContinue || request.continuationFailoverAttempted {
			inferenceActivity.Add("history_replays", 1)
		}
	}
	conversationID := strings.TrimSpace(request.ConversationID)
	if conversationID == "" {
		return
	}
	request.onThreadPrepared = func(threadID string) {
		a.State.conversations().SetExecutionTarget(conversationID, threadID, accountEmail, spaceID)
		a.State.persistConversationSnapshot(conversationID)
	}
	request.onThreadPrepared(request.preparedThreadID)
}
