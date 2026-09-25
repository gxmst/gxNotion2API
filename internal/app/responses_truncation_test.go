package app

import "testing"

// TestReplayKeepsTheTruncationMark covers the cached-answer path. A truncated
// answer is stored with its flag, and replaying it must not present the same
// half-finished text as a clean stop on the second request.
func TestReplayKeepsTheTruncationMark(t *testing.T) {
	conversation := ConversationEntry{
		ID:       "conv_replay",
		Status:   "completed",
		ThreadID: "replay-thread",
		Messages: []ConversationMessage{
			{ID: "m_user", Role: "user", Status: "completed", Content: "问题"},
			{ID: "m_answer", Role: "assistant", Status: "completed", Content: "半截回答", Truncated: true},
		},
	}

	replayed := replayResultFromConversation(conversation)
	if replayed == nil {
		t.Fatal("a completed conversation with an assistant message should replay")
	}
	if !replayed.Truncated {
		t.Fatal("the replayed result lost its truncation mark")
	}

	// Control: an answer that finished normally replays as complete.
	conversation.Messages[1].Truncated = false
	if replayed := replayResultFromConversation(conversation); replayed == nil || replayed.Truncated {
		t.Fatal("a complete answer was replayed as truncated")
	}
}

// TestResponsesReportsATruncatedAnswerAsIncomplete pins the client-visible
// contract for an answer the upstream cut short. The chat-completions surface
// reports this as finish_reason "length"; the Responses surface has its own
// vocabulary and must use "incomplete" plus a reason, not "completed".
func TestResponsesReportsATruncatedAnswerAsIncomplete(t *testing.T) {
	payload := buildResponsesOutputWithIDs(
		InferenceResult{Text: "半截回答", Truncated: true},
		"test-model", false, "resp_1", "msg_1", 1,
	)

	if got := payload["status"]; got != "incomplete" {
		t.Fatalf("status = %v, want incomplete", got)
	}
	details, ok := payload["incomplete_details"].(map[string]any)
	if !ok {
		t.Fatalf("incomplete_details = %#v, want a reason map", payload["incomplete_details"])
	}
	if got := details["reason"]; got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %v, want max_output_tokens", got)
	}

	// The output item carries its own status and has to agree with the envelope,
	// otherwise a client that reads only the item sees a finished answer.
	items, ok := payload["output"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("output = %#v, want one item", payload["output"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("output[0] = %#v, want a map", items[0])
	}
	if got := item["status"]; got != "incomplete" {
		t.Fatalf("output[0].status = %v, want incomplete", got)
	}
}

// TestResponsesReportsACompleteAnswerAsCompleted is the control case, so the
// truncation mapping cannot be satisfied by always reporting incomplete.
func TestResponsesReportsACompleteAnswerAsCompleted(t *testing.T) {
	payload := buildResponsesOutputWithIDs(
		InferenceResult{Text: "完整回答"},
		"test-model", false, "resp_2", "msg_2", 1,
	)

	if got := payload["status"]; got != "completed" {
		t.Fatalf("status = %v, want completed", got)
	}
	if got := payload["incomplete_details"]; got != nil {
		t.Fatalf("incomplete_details = %#v, want nil for a finished answer", got)
	}
	items := payload["output"].([]any)
	if got := items[0].(map[string]any)["status"]; got != "completed" {
		t.Fatalf("output[0].status = %v, want completed", got)
	}
}

// TestResponsesTerminalEventNameMatchesStatus covers the streaming side: the
// terminal event name is what a streaming client keys off, so it has to switch
// with the status.
func TestResponsesTerminalEventNameMatchesStatus(t *testing.T) {
	if got := responsesTerminalEventName(true); got != "response.incomplete" {
		t.Fatalf("terminal event for a truncated answer = %q, want response.incomplete", got)
	}
	if got := responsesTerminalEventName(false); got != "response.completed" {
		t.Fatalf("terminal event for a finished answer = %q, want response.completed", got)
	}
}
