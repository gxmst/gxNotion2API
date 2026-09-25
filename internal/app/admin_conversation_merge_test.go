package app

import (
	"testing"
	"time"
)

// conversationMergeBase is a fixed instant so the merge tests never depend on
// the clock.
func conversationMergeBase() time.Time {
	return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
}

// TestRemoteMergeKeepsOperatorRename is the regression for the rename that the
// sidebar silently reverted. The admin list merges local SQLite rows with
// Notion's transcripts; Notion only knows the title it generated for the
// thread, so letting a non-empty remote title win reverted every hand rename on
// the next refresh while the rename itself reported success.
func TestRemoteMergeKeepsOperatorRename(t *testing.T) {
	editedAt := conversationMergeBase()
	local := ConversationSummary{
		ID:            "conv_thread-1",
		Title:         "我改过的标题",
		TitleEditedAt: &editedAt,
		ThreadID:      "thread-1",
		Status:        "completed",
	}
	remote := ConversationSummary{
		ID:       "conv_thread-1",
		Title:    "Notion 自动生成的标题",
		ThreadID: "thread-1",
		Status:   "completed",
	}

	merged := mergeConversationSummary(local, remote)
	if merged.Title != "我改过的标题" {
		t.Fatalf("merged title = %q, want the operator rename to win", merged.Title)
	}
	if merged.Origin != "merged" || merged.RemoteOnly {
		t.Fatalf("merge bookkeeping lost: origin=%q remote_only=%v", merged.Origin, merged.RemoteOnly)
	}
	if merged.TitleEditedAt == nil {
		t.Fatal("TitleEditedAt was dropped, so the next merge would revert the rename")
	}
}

// TestRemoteMergeAdoptsTitleWhenNotEditedByHand keeps the rename fix from
// freezing titles that no operator ever touched.
func TestRemoteMergeAdoptsTitleWhenNotEditedByHand(t *testing.T) {
	local := ConversationSummary{
		ID:       "conv_thread-2",
		Title:    "新对话",
		ThreadID: "thread-2",
		Status:   "completed",
	}
	remote := ConversationSummary{
		ID:       "conv_thread-2",
		Title:    "Notion 总结出的标题",
		ThreadID: "thread-2",
		Status:   "completed",
	}

	merged := mergeConversationSummary(local, remote)
	if merged.Title != "Notion 总结出的标题" {
		t.Fatalf("merged title = %q, want the remote title for an untouched conversation", merged.Title)
	}
}

// TestRemoteMergeKeepsEditedMessageBodyAndTruncation covers the other half of
// the same bug. The merge used to rebuild the message list purely from the
// remote transcript, which threw away a hand-edited body (Notion still holds
// the pre-edit text) and the locally-only truncation flag.
func TestRemoteMergeKeepsEditedMessageBodyAndTruncation(t *testing.T) {
	base := conversationMergeBase()
	editedAt := base.Add(time.Hour)
	local := ConversationEntry{
		ID:        "conv_thread-3",
		Title:     "对话",
		ThreadID:  "thread-3",
		Status:    "completed",
		Source:    "api",
		Transport: "chat_completions",
		Messages: []ConversationMessage{
			{ID: "msg_user", Role: "user", Status: "completed", Content: "最初的提问", CreatedAt: base, UpdatedAt: base},
			{ID: "msg_assistant", Role: "assistant", Status: "completed", Content: "手改之后的回答", CreatedAt: base, UpdatedAt: base, EditedAt: &editedAt, Truncated: true},
		},
	}
	remote := ConversationEntry{
		ID:       "conv_thread-3",
		Title:    "对话",
		ThreadID: "thread-3",
		Messages: []ConversationMessage{
			{ID: "msg_user", Role: "user", Status: "completed", Content: "最初的提问"},
			{ID: "msg_assistant", Role: "assistant", Status: "completed", Content: "模型原来写出的回答"},
		},
	}

	merged := mergeConversationEntry(local, remote)
	if len(merged.Messages) != 2 {
		t.Fatalf("merged %d messages, want 2", len(merged.Messages))
	}
	assistant := merged.Messages[1]
	if assistant.Content != "手改之后的回答" {
		t.Fatalf("assistant content = %q, want the hand-edited body to survive", assistant.Content)
	}
	if assistant.EditedAt == nil {
		t.Fatal("EditedAt was dropped, so the UI would stop marking the message as edited")
	}
	if !assistant.Truncated {
		t.Fatal("Truncated was dropped, so a reopened transcript would claim a complete answer")
	}
	// The untouched user message must still track the upstream copy.
	if merged.Messages[0].Content != "最初的提问" {
		t.Fatalf("user content = %q, want the remote text for an unedited message", merged.Messages[0].Content)
	}
}

// TestRemoteMergeRefreshesUneditedAssistantBody guards the inverse case: an
// assistant message the operator never edited must still be refreshed from the
// remote transcript, otherwise the fix above would freeze every answer.
func TestRemoteMergeRefreshesUneditedAssistantBody(t *testing.T) {
	base := conversationMergeBase()
	local := ConversationEntry{
		ID:       "conv_thread-4",
		ThreadID: "thread-4",
		Status:   "completed",
		Messages: []ConversationMessage{
			{ID: "msg_a", Role: "assistant", Status: "completed", Content: "本地不完整的回答", CreatedAt: base, UpdatedAt: base},
		},
	}
	remote := ConversationEntry{
		ID:       "conv_thread-4",
		ThreadID: "thread-4",
		Messages: []ConversationMessage{
			{ID: "msg_a", Role: "assistant", Status: "completed", Content: "Notion 上更完整的回答"},
		},
	}

	merged := mergeConversationEntry(local, remote)
	if merged.Messages[0].Content != "Notion 上更完整的回答" {
		t.Fatalf("assistant content = %q, want the remote text when nothing was edited", merged.Messages[0].Content)
	}
}

// TestMergeSummariesMarksMatchedLocalRowAsMerged covers the list-level pairing
// the rename fix depends on: only a matched thread becomes "merged", everything
// else stays either local or remote-only.
func TestMergeSummariesMarksMatchedLocalRowAsMerged(t *testing.T) {
	editedAt := conversationMergeBase()
	localItems := []ConversationSummary{
		{ID: "conv_thread-5", Title: "本地标题", TitleEditedAt: &editedAt, ThreadID: "thread-5", Status: "completed"},
		{ID: "conv_local_only", Title: "只在本地", Status: "completed"},
	}
	remoteItems := []InferenceTranscriptSummary{
		{ThreadID: "thread-5", Title: "Notion 标题", UpdatedAt: conversationMergeBase()},
		{ThreadID: "thread-6", Title: "只在 Notion", UpdatedAt: conversationMergeBase().Add(-time.Hour)},
	}

	merged := mergeAdminConversationSummaries(localItems, remoteItems)
	byID := make(map[string]ConversationSummary, len(merged))
	for _, item := range merged {
		byID[item.ID] = item
	}

	if got := byID["conv_thread-5"]; got.Origin != "merged" || got.RemoteOnly || got.Title != "本地标题" {
		t.Fatalf("matched row = %+v, want origin=merged remote_only=false title=本地标题", got)
	}
	if got := byID["conv_local_only"]; got.RemoteOnly || got.Origin != "local" {
		t.Fatalf("local-only row = %+v, want origin=local remote_only=false", got)
	}
	if got := byID[notionThreadConversationID("thread-6")]; !got.RemoteOnly || got.Origin != "notion" {
		t.Fatalf("remote-only row = %+v, want origin=notion remote_only=true", got)
	}
}
