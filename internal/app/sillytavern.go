package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

const (
	sillyTavernClientProfile  = "sillytavern"
	sillyTavernModeNormal     = "normal"
	sillyTavernModeContinue   = "continue"
	sillyTavernModeQuiet      = "quiet"
	sillyTavernModeImpersona  = "impersonate"
	sillyTavernContinuePrompt = "[Continue your last message without repeating its original content.]"
)

type SillyTavernBinding struct {
	ConversationID  string
	ProfileKey      string
	ThreadID        string
	AccountEmail    string
	Mode            string
	Transcript      []conversationPromptSegment
	RawMessageCount int
	UpdatedAt       time.Time
}

type sillyTavernContext struct {
	Mode            string
	ProfileKey      string
	Normalized      NormalizedInput
	LatestPrompt    string
	DisplayPrompt   string
	StableHidden    string
	RequestHidden   string
	RequestSegments []conversationPromptSegment
}

type sillyTavernContinuationMatch struct {
	Target            continuationTarget
	ForceRepeatTurn   bool
	SuppressPersist   bool
	ResolvedByBinding bool
}

func normalizeSillyTavernMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case sillyTavernModeContinue:
		return sillyTavernModeContinue
	case sillyTavernModeQuiet:
		return sillyTavernModeQuiet
	case sillyTavernModeImpersona:
		return sillyTavernModeImpersona
	default:
		return sillyTavernModeNormal
	}
}

func buildSillyTavernContext(payload map[string]any) (sillyTavernContext, error) {
	normalized, err := normalizeChatInput(payload)
	if err != nil {
		return sillyTavernContext{}, err
	}
	mode := inferSillyTavernMode(payload, normalized)
	summaryPrompts := collectSillyTavernSummaryPrompts(payload)
	normalized = stripSillyTavernSummarySegments(normalized, summaryPrompts)
	requestHidden := appendSillyTavernSummaryToHiddenPrompt(normalized.HiddenPrompt, summaryPrompts)
	stableHidden := normalizeSillyTavernHiddenPrompt(normalized.HiddenPrompt)
	latestPrompt := resolveRequestPromptForContinuation(normalized)
	return sillyTavernContext{
		Mode:            mode,
		ProfileKey:      buildSillyTavernProfileKey(payload, stableHidden),
		Normalized:      normalized,
		LatestPrompt:    latestPrompt,
		DisplayPrompt:   firstNonEmpty(strings.TrimSpace(normalized.DisplayPrompt), latestPrompt, strings.TrimSpace(normalized.Prompt)),
		StableHidden:    stableHidden,
		RequestHidden:   requestHidden,
		RequestSegments: normalizeConversationHistorySegments(normalized.Segments),
	}, nil
}

func sillyTavernWantsReasoning(payload map[string]any) bool {
	if payload == nil {
		return true
	}
	if value, ok := payload["include_reasoning"]; ok {
		return boolValue(value)
	}
	if value, ok := payload["show_thoughts"]; ok {
		return boolValue(value)
	}
	if reasoning := mapValue(payload["reasoning"]); reasoning != nil {
		if value, ok := reasoning["enabled"]; ok {
			return boolValue(value)
		}
	}
	return true
}

func sillyTavernContinuationPrompt(payload map[string]any) string {
	if payload != nil {
		if prefill := strings.TrimSpace(stringValue(payload["continue_prefill"])); prefill != "" {
			return prefill
		}
	}
	return sillyTavernContinuePrompt
}

func inferSillyTavernMode(payload map[string]any, normalized NormalizedInput) string {
	if explicit := strings.TrimSpace(stringValue(payload["type"])); explicit != "" {
		return normalizeSillyTavernMode(explicit)
	}

	segments := normalizeConversationHistorySegments(normalized.Segments)
	systemPrompts := collectSillyTavernSystemPrompts(payload)

	if looksLikeSillyTavernImpersonate(systemPrompts) {
		return sillyTavernModeImpersona
	}
	if looksLikeSillyTavernContinue(segments) {
		return sillyTavernModeContinue
	}
	if looksLikeSillyTavernQuiet(systemPrompts, segments) {
		return sillyTavernModeQuiet
	}
	return sillyTavernModeNormal
}

func isLikelySillyTavernPayload(payload map[string]any) bool {
	if explicit := strings.TrimSpace(stringValue(payload["type"])); explicit != "" {
		return true
	}
	if strings.TrimSpace(stringValue(payload["user_name"])) != "" && strings.TrimSpace(stringValue(payload["char_name"])) != "" {
		return true
	}
	if len(stringSliceValue(payload["group_names"])) > 0 {
		return true
	}
	if _, ok := payload["continue_prefill"]; ok {
		return true
	}
	if _, ok := payload["show_thoughts"]; ok {
		return true
	}
	systemPrompts := collectSillyTavernSystemPrompts(payload)
	if looksLikeSillyTavernImpersonate(systemPrompts) || looksLikeSillyTavernQuiet(systemPrompts, nil) {
		return true
	}
	for _, prompt := range systemPrompts {
		lower := strings.ToLower(collapseWhitespace(prompt))
		if strings.Contains(lower, "fictional chat between") ||
			strings.Contains(lower, "[start a new chat]") ||
			strings.Contains(lower, "[continue your last message without repeating its original content.]") {
			return true
		}
	}
	return false
}

func collectSillyTavernSystemPrompts(payload map[string]any) []string {
	items, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		msg, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(strings.ToLower(stringValue(msg["role"]))) != "system" {
			continue
		}
		text := collapseWhitespace(flattenContent(msg["content"]))
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

func collectSillyTavernSummaryPrompts(payload map[string]any) []string {
	items, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text := collapseWhitespace(flattenContent(msg["content"]))
		if !looksLikeSillyTavernSummaryPrompt(text) {
			continue
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out
}

func looksLikeSillyTavernSummaryPrompt(text string) bool {
	lower := strings.ToLower(collapseWhitespace(text))
	switch {
	case strings.HasPrefix(lower, "[summary:") && strings.HasSuffix(lower, "]"):
		return true
	case strings.HasPrefix(lower, "summary:"):
		return true
	case strings.HasPrefix(lower, "[current summary:") && strings.HasSuffix(lower, "]"):
		return true
	case strings.HasPrefix(lower, "current summary:"):
		return true
	default:
		return false
	}
}

func looksLikeSillyTavernImpersonate(systemPrompts []string) bool {
	for _, prompt := range systemPrompts {
		lower := strings.ToLower(collapseWhitespace(prompt))
		if strings.Contains(lower, "point of view of") &&
			strings.Contains(lower, "don't write as") {
			return true
		}
		if strings.Contains(lower, "using the chat history so far as a guideline") &&
			strings.Contains(lower, "don't describe actions of") {
			return true
		}
	}
	return false
}

func looksLikeSillyTavernContinue(segments []conversationPromptSegment) bool {
	if len(segments) == 0 {
		return false
	}
	return strings.TrimSpace(strings.ToLower(segments[len(segments)-1].Role)) == "assistant"
}

func looksLikeSillyTavernQuiet(systemPrompts []string, segments []conversationPromptSegment) bool {
	if segments != nil {
		if len(segments) != 1 || strings.TrimSpace(strings.ToLower(segments[0].Role)) != "user" {
			return false
		}
	}
	if len(systemPrompts) == 0 {
		return false
	}
	for _, prompt := range systemPrompts {
		lower := strings.ToLower(collapseWhitespace(prompt))
		if strings.Contains(lower, "fictional chat between") ||
			strings.Contains(lower, "write the next reply only as") ||
			strings.Contains(lower, "[start a new chat]") ||
			strings.Contains(lower, "continue your last message") {
			return false
		}
	}
	for _, prompt := range systemPrompts {
		lower := strings.ToLower(collapseWhitespace(prompt))
		if strings.Contains(lower, "summar") ||
			strings.Contains(lower, "classif") ||
			strings.Contains(lower, "translate") ||
			strings.Contains(lower, "describe") ||
			strings.Contains(lower, "caption") ||
			strings.Contains(lower, "不要续写") ||
			strings.Contains(lower, "总结") ||
			strings.Contains(lower, "概括") ||
			strings.Contains(lower, "提炼") ||
			strings.Contains(lower, "改写") {
			return true
		}
	}
	return false
}

func normalizeSillyTavernHiddenPrompt(hiddenPrompt string) string {
	if strings.TrimSpace(hiddenPrompt) == "" {
		return ""
	}
	lines := strings.Split(hiddenPrompt, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		clean := strings.TrimSpace(line)
		if clean == "" {
			continue
		}
		lower := strings.ToLower(clean)
		switch {
		case strings.Contains(lower, "[start a new chat]"):
			continue
		case strings.Contains(lower, "[start a new group chat"):
			continue
		case strings.Contains(lower, "[example chat]"):
			continue
		case strings.Contains(lower, "[continue your last message without repeating its original content.]"):
			continue
		}
		kept = append(kept, clean)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func stripSillyTavernSummarySegments(normalized NormalizedInput, summaryPrompts []string) NormalizedInput {
	if len(summaryPrompts) == 0 || len(normalized.Segments) == 0 {
		return normalized
	}
	summarySet := make(map[string]struct{}, len(summaryPrompts))
	for _, prompt := range summaryPrompts {
		clean := collapseWhitespace(prompt)
		if clean == "" {
			continue
		}
		summarySet[clean] = struct{}{}
	}
	if len(summarySet) == 0 {
		return normalized
	}
	filtered := make([]conversationPromptSegment, 0, len(normalized.Segments))
	removed := false
	for _, segment := range normalized.Segments {
		if _, ok := summarySet[collapseWhitespace(segment.Text)]; ok {
			removed = true
			continue
		}
		filtered = append(filtered, segment)
	}
	if !removed {
		return normalized
	}
	normalized.Segments = cloneConversationPromptSegments(filtered)
	normalized.Prompt = rebuildSillyTavernPrompt(filtered, normalized.Attachments)
	normalized.DisplayPrompt = firstNonEmpty(latestUserConversationSegmentText(filtered), normalized.Prompt)
	return normalized
}

func rebuildSillyTavernPrompt(segments []conversationPromptSegment, attachments []InputAttachment) string {
	hasNonUserHistory := false
	for _, segment := range segments {
		if strings.TrimSpace(strings.ToLower(segment.Role)) != "user" {
			hasNonUserHistory = true
			break
		}
	}
	prompt := buildConversationPrompt(segments, hasNonUserHistory)
	if prompt == "" && len(attachments) > 0 {
		if hasNonUserHistory && len(segments) > 0 {
			segmentsWithAttachment := append(append([]conversationPromptSegment(nil), segments...), conversationPromptSegment{
				Role: "user",
				Text: defaultUploadedAttachmentPrompt,
			})
			prompt = buildConversationTranscriptPrompt(segmentsWithAttachment)
		} else {
			prompt = defaultUploadedAttachmentPrompt
		}
	}
	return prompt
}

func appendSillyTavernSummaryToHiddenPrompt(hiddenPrompt string, summaryPrompts []string) string {
	parts := make([]string, 0, len(summaryPrompts)+1)
	if clean := strings.TrimSpace(hiddenPrompt); clean != "" {
		parts = append(parts, clean)
	}
	for _, prompt := range summaryPrompts {
		if clean := strings.TrimSpace(prompt); clean != "" {
			parts = append(parts, clean)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func buildSillyTavernProfileKey(payload map[string]any, stableHidden string) string {
	h := sha256.New()
	write := func(prefix string, value string, limit int) {
		value = collapseWhitespace(value)
		if value == "" {
			return
		}
		if limit > 0 && len([]rune(value)) > limit {
			value = truncateRunes(value, limit)
		}
		h.Write([]byte(prefix))
		h.Write([]byte(value))
		h.Write([]byte{'\n'})
	}
	write("mode:", normalizeSillyTavernMode(stringValue(payload["type"])), 32)
	write("user:", stringValue(payload["user_name"]), 120)
	write("char:", stringValue(payload["char_name"]), 120)
	write("group:", strings.Join(stringSliceValue(payload["group_names"]), ","), 240)
	write("hidden:", stableHidden, 2400)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func stringSliceValue(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if clean := strings.TrimSpace(stringValue(item)); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func stripTrailingRole(segments []conversationPromptSegment, role string) []conversationPromptSegment {
	segments = cloneConversationPromptSegments(normalizeConversationHistorySegments(segments))
	if len(segments) == 0 {
		return nil
	}
	if strings.TrimSpace(strings.ToLower(segments[len(segments)-1].Role)) != strings.TrimSpace(strings.ToLower(role)) {
		return segments
	}
	return cloneConversationPromptSegments(segments[:len(segments)-1])
}

func conversationSegmentSuffixMatch(longer []conversationPromptSegment, shorter []conversationPromptSegment) bool {
	longer = normalizeConversationHistorySegments(longer)
	shorter = normalizeConversationHistorySegments(shorter)
	if len(longer) == 0 || len(shorter) == 0 || len(shorter) > len(longer) {
		return false
	}
	offset := len(longer) - len(shorter)
	for idx := range shorter {
		if longer[offset+idx].Role != shorter[idx].Role || longer[offset+idx].Text != shorter[idx].Text {
			return false
		}
	}
	return true
}

func conversationSegmentsEqual(a []conversationPromptSegment, b []conversationPromptSegment) bool {
	a = normalizeConversationHistorySegments(a)
	b = normalizeConversationHistorySegments(b)
	if len(a) != len(b) {
		return false
	}
	for idx := range a {
		if a[idx].Role != b[idx].Role || a[idx].Text != b[idx].Text {
			return false
		}
	}
	return true
}

func (a *App) resolveSillyTavernContinuation(r *http.Request, payload map[string]any, ctx sillyTavernContext, fingerprint string, clientScope string) (sillyTavernContinuationMatch, bool) {
	if ctx.Mode == sillyTavernModeQuiet || ctx.Mode == sillyTavernModeImpersona {
		return sillyTavernContinuationMatch{SuppressPersist: true}, true
	}

	if ctx.Mode == sillyTavernModeContinue {
		if explicitConversationID := requestedConversationID(r, payload); explicitConversationID != "" {
			target := continuationTarget{}
			if entry, ok := a.State.conversations().Get(explicitConversationID); ok && strings.TrimSpace(entry.ThreadID) != "" {
				target.Conversation = entry
			}
			if state, err := a.State.loadConversationContinuationStateByConversationID(explicitConversationID); err == nil && state != nil {
				target.Session = state
				if strings.TrimSpace(target.Conversation.ID) == "" {
					target.Conversation = ConversationEntry{
						ID:           strings.TrimSpace(state.Session.ConversationID),
						ThreadID:     strings.TrimSpace(state.Session.ThreadID),
						AccountEmail: strings.TrimSpace(state.Session.AccountEmail),
					}
				}
			}
			if strings.TrimSpace(target.Conversation.ThreadID) != "" {
				return sillyTavernContinuationMatch{
					Target:            target,
					ForceRepeatTurn:   true,
					ResolvedByBinding: target.Session != nil,
				}, true
			}
		}
		if explicitThreadID := requestedThreadID(r, payload); explicitThreadID != "" {
			target := continuationTarget{
				Conversation: ConversationEntry{ThreadID: strings.TrimSpace(explicitThreadID)},
			}
			if entry, ok := a.State.conversations().FindByThreadID(explicitThreadID); ok {
				target.Conversation = entry
			}
			if state, err := a.State.loadConversationContinuationStateByThreadID(explicitThreadID); err == nil && state != nil {
				target.Session = state
				if strings.TrimSpace(target.Conversation.ID) == "" {
					target.Conversation = ConversationEntry{
						ID:           strings.TrimSpace(state.Session.ConversationID),
						ThreadID:     strings.TrimSpace(state.Session.ThreadID),
						AccountEmail: strings.TrimSpace(state.Session.AccountEmail),
					}
				}
			}
			if strings.TrimSpace(target.Conversation.ThreadID) != "" {
				return sillyTavernContinuationMatch{
					Target:            target,
					ForceRepeatTurn:   true,
					ResolvedByBinding: target.Session != nil,
				}, true
			}
		}
	}

	general, ok := a.resolveContinuationConversationWithExplicit("", fingerprint, clientScope, ctx.RequestSegments, requestedConversationID(r, payload), requestedThreadID(r, payload))
	if ok {
		match := sillyTavernContinuationMatch{Target: general}
		if ctx.Mode == sillyTavernModeContinue {
			match.ForceRepeatTurn = true
		}
		return match, true
	}

	bindings, err := a.State.loadRecentSillyTavernBindings(ctx.ProfileKey, 16)
	if err != nil || len(bindings) == 0 {
		return sillyTavernContinuationMatch{}, false
	}

	requestHistory := ctx.RequestSegments
	requestBeforeLatestUser := stripTrailingRole(requestHistory, "user")
	for _, binding := range bindings {
		storedTranscript := normalizeConversationHistorySegments(binding.Transcript)
		if len(storedTranscript) == 0 || strings.TrimSpace(binding.ThreadID) == "" {
			continue
		}

		forceRepeat := false
		switch ctx.Mode {
		case sillyTavernModeContinue:
			forceRepeat = conversationSegmentsEqual(storedTranscript, requestHistory) || conversationSegmentSuffixMatch(storedTranscript, requestHistory)
		default:
			if len(requestBeforeLatestUser) > 0 && conversationSegmentSuffixMatch(storedTranscript, requestBeforeLatestUser) {
				forceRepeat = false
			} else if conversationSegmentSuffixMatch(stripTrailingRole(storedTranscript, "assistant"), requestHistory) {
				forceRepeat = true
			} else {
				continue
			}
		}

		target := continuationTarget{
			Conversation: ConversationEntry{
				ID:           strings.TrimSpace(binding.ConversationID),
				ThreadID:     strings.TrimSpace(binding.ThreadID),
				AccountEmail: strings.TrimSpace(binding.AccountEmail),
			},
		}
		if state, stateErr := a.State.loadConversationContinuationStateByConversationID(binding.ConversationID); stateErr == nil && state != nil {
			target.Session = state
		}
		if target.Conversation.ID != "" {
			if existing, found := a.State.conversations().Get(target.Conversation.ID); found {
				target.Conversation = existing
			}
		}
		return sillyTavernContinuationMatch{
			Target:            target,
			ForceRepeatTurn:   forceRepeat,
			ResolvedByBinding: true,
		}, true
	}

	return sillyTavernContinuationMatch{}, false
}

func (s *ServerState) loadRecentSillyTavernBindings(profileKey string, limit int) ([]SillyTavernBinding, error) {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := sillyTavernBindingsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled || strings.TrimSpace(profileKey) == "" {
		return nil, nil
	}
	return store.LoadRecentSillyTavernBindings(profileKey, limit)
}

func (s *ServerState) deleteSillyTavernBinding(conversationID string) {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := sillyTavernBindingsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled || strings.TrimSpace(conversationID) == "" {
		return
	}
	if err := store.DeleteSillyTavernBinding(conversationID); err != nil {
		// Silent best-effort cleanup.
	}
}

func (a *App) persistSillyTavernBinding(conversationID string, profileKey string, mode string) {
	conversationID = strings.TrimSpace(conversationID)
	profileKey = strings.TrimSpace(profileKey)
	if conversationID == "" || profileKey == "" {
		return
	}
	a.State.mu.RLock()
	store := a.State.Store
	storeEnabled := store != nil && sillyTavernBindingsPersistenceEnabled(a.State.Config)
	a.State.mu.RUnlock()
	if !storeEnabled {
		return
	}
	entry, ok := a.State.conversations().Get(conversationID)
	if !ok || strings.TrimSpace(entry.ThreadID) == "" {
		return
	}
	segments := conversationMessageSegments(&entry)
	if len(segments) == 0 {
		return
	}
	_ = store.SaveSillyTavernBinding(SillyTavernBinding{
		ConversationID:  conversationID,
		ProfileKey:      profileKey,
		ThreadID:        strings.TrimSpace(entry.ThreadID),
		AccountEmail:    strings.TrimSpace(entry.AccountEmail),
		Mode:            normalizeSillyTavernMode(mode),
		Transcript:      segments,
		RawMessageCount: len(segments),
		UpdatedAt:       time.Now().UTC(),
	})
}
