package app

import (
	"strings"
	"testing"
)

func stepRecord(step map[string]any) map[string]any {
	return map[string]any{"value": map[string]any{"value": map[string]any{"step": step, "data": map[string]any{"completed": true}}}}
}

func TestIntermediateAgentStepsBecomeVisibleProcessEntries(t *testing.T) {
	// A tool/search step with no text part still has to survive extraction, or
	// the process trail Notion renders above the answer disappears entirely.
	message, ok := extractConversationMessageFromThreadRecord("tool-1", stepRecord(map[string]any{
		"type":  "agent-tool-result",
		"value": []any{map[string]any{"type": "tool", "name": "search"}},
	}))
	if !ok {
		t.Fatal("intermediate step was dropped")
	}
	if message.Role != "step" || message.StepType != "agent-tool-result" {
		t.Fatalf("step identity lost: %+v", message)
	}
	if message.Content != "" {
		t.Fatalf("tool payload leaked into step text: %q", message.Content)
	}
	if message.Status != "completed" {
		t.Fatalf("unexpected status: %q", message.Status)
	}
}

func TestStepTextIsLimitedToExplicitlyTypedTextParts(t *testing.T) {
	message, ok := extractConversationMessageFromThreadRecord("search-1", stepRecord(map[string]any{
		"type": "agent-tool-result",
		"value": []any{
			map[string]any{"type": "thinking", "content": "internal reasoning that must not surface"},
			map[string]any{"type": "text", "content": "searching the web"},
		},
	}))
	if !ok {
		t.Fatal("step was dropped")
	}
	if message.Content != "searching the web" {
		t.Fatalf("explicit text part lost: %q", message.Content)
	}
	if strings.Contains(message.Content, "internal reasoning") {
		t.Fatal("reasoning was promoted into visible step text")
	}
}

func TestStepTextIsTruncated(t *testing.T) {
	message, ok := extractConversationMessageFromThreadRecord("long-1", stepRecord(map[string]any{
		"type":  "agent-tool-result",
		"value": []any{map[string]any{"type": "text", "content": strings.Repeat("字", 500)}},
	}))
	if !ok {
		t.Fatal("step was dropped")
	}
	runes := []rune(message.Content)
	if len(runes) != 400 || runes[len(runes)-1] != '…' {
		t.Fatalf("step text not bounded: %d runes", len(runes))
	}
}

func TestStructuralStepsStayOutOfTheTranscript(t *testing.T) {
	for _, stepType := range []string{"config", "context", "workflow", "updated-config"} {
		if _, ok := extractConversationMessageFromThreadRecord("x", stepRecord(map[string]any{"type": stepType})); ok {
			t.Fatalf("structural step %q leaked into the transcript", stepType)
		}
	}
	if _, ok := extractConversationMessageFromThreadRecord("x", stepRecord(map[string]any{})); ok {
		t.Fatal("a step with no type leaked into the transcript")
	}
}

func TestAttachmentStepIsMarkedAndKeepsItsFile(t *testing.T) {
	message, ok := extractConversationMessageFromThreadRecord("file-1", stepRecord(map[string]any{
		"type":        "attachment",
		"fileName":    "hello.py",
		"contentType": "text/x-python",
		"fileUrl":     "https://example.test/hello.py",
	}))
	if !ok {
		t.Fatal("attachment step was dropped")
	}
	// An attachment is something the user sent, so it keeps the user role; the
	// marker is what lets the UI render it as a file card instead of an empty
	// chat bubble.
	if message.Role != "user" || message.StepType != "attachment" {
		t.Fatalf("attachment identity lost: %+v", message)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].URL != "https://example.test/hello.py" {
		t.Fatalf("attachment file lost: %+v", message.Attachments)
	}
}

func TestPreviewIgnoresProcessSteps(t *testing.T) {
	messages := []ConversationMessage{
		{Role: "user", Content: "整理一下这份文档"},
		{Role: "assistant", Content: "这是结论。"},
		{Role: "step", StepType: "agent-tool-result", Content: "searching the web"},
	}
	if got := conversationPreviewFromMessages(messages); got != "这是结论。" {
		t.Fatalf("preview picked up a process step: %q", got)
	}
}

func TestStepMessagesNeverBecomePromptHistory(t *testing.T) {
	entry := &ConversationEntry{Messages: []ConversationMessage{
		{Role: "user", Content: "问题"},
		{Role: "step", StepType: "agent-tool-result", Content: "searching the web"},
		{Role: "assistant", Content: "答案"},
	}}
	segments := conversationMessageSegments(entry)
	if len(segments) != 2 || segments[0].Role != "user" || segments[1].Role != "assistant" {
		t.Fatalf("process step leaked into prompt history: %+v", segments)
	}
}
