package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminTestHandlerReleasesTheTurnWhenInferencePanics is the regression for a
// missing deferred release. handleAdminTest started a conversation turn but did
// not release it on the way out, so a panic (or any early return after the turn
// started) left the conversation "running" forever and every later test on that
// conversation was turned away as busy.
func TestAdminTestHandlerReleasesTheTurnWhenInferencePanics(t *testing.T) {
	app := newConversationRequestTestApp(t)
	app.runPromptOverride = func(*http.Request, PromptRunRequest) (InferenceResult, error) {
		panic("upstream exploded")
	}

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, conversationTestHTTPRequest(t, "/admin/test", map[string]any{
		"prompt": "boom", "conversation_id": "panic-chat",
	}))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from the recovered panic", recorder.Code)
	}

	entry, ok := app.State.conversations().Get("panic-chat")
	if !ok {
		t.Fatal("the turn never created a conversation")
	}
	if conversationStatusBusy(entry.Status) {
		t.Fatalf("conversation is still %q; the turn was never released", entry.Status)
	}
	if entry.Status != "failed" {
		t.Fatalf("status = %q, want the abandoned turn marked failed", entry.Status)
	}

	// Releasing the turn is what makes this work: the next test on the same
	// conversation must not be rejected as busy.
	app.runPromptOverride = func(*http.Request, PromptRunRequest) (InferenceResult, error) {
		return InferenceResult{Text: "second answer", ThreadID: "panic-thread"}, nil
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, conversationTestHTTPRequest(t, "/admin/test", map[string]any{
		"prompt": "again", "conversation_id": "panic-chat",
	}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("retry after the panic: status = %d, want 200 (body=%s)", recorder.Code, recorder.Body.String())
	}
}

// TestAdminTestHandlerReleasesTheTurnOnUpstreamFailure pins the same invariant
// on the ordinary error path. Unlike the panic case this one already held (the
// handler calls failConversation directly), so it is a companion guard rather
// than a regression: it keeps a later change to the error branch from
// reintroducing a conversation that stays busy forever.
func TestAdminTestHandlerReleasesTheTurnOnUpstreamFailure(t *testing.T) {
	app := newConversationRequestTestApp(t)
	app.runPromptOverride = func(*http.Request, PromptRunRequest) (InferenceResult, error) {
		return InferenceResult{}, errConversationTurnAbandoned
	}

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, conversationTestHTTPRequest(t, "/admin/test", map[string]any{
		"prompt": "will fail", "conversation_id": "fail-chat",
	}))
	if recorder.Code == http.StatusOK {
		t.Fatalf("status = %d, want a failure response", recorder.Code)
	}
	entry, ok := app.State.conversations().Get("fail-chat")
	if !ok {
		t.Fatal("the turn never created a conversation")
	}
	if conversationStatusBusy(entry.Status) {
		t.Fatalf("conversation is still %q after an upstream failure", entry.Status)
	}
}
