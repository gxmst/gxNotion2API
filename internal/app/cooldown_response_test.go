package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMalformedJSONDoesNotCoolDownHealthyWorkspace(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		a := newConversationRequestTestApp(t)
		var decoded any
		failure := json.Unmarshal([]byte(`{"incomplete":`), &decoded)
		calls := 0
		a.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
			calls++
			return InferenceResult{}, failure
		}
		r := httptest.NewRequest(http.MethodPost, "/admin/test", nil)
		request := PromptRunRequest{Prompt: "continue", PinnedAccountEmail: "primary@example.com", PinnedSpaceID: "space-primary", UpstreamThreadID: "existing-thread"}
		var err error
		if streaming {
			_, err = a.runPromptStreamWithSink(r, request, InferenceStreamSink{})
		} else {
			_, err = a.runPrompt(r, request)
		}
		cfg, _, _ := a.State.Snapshot()
		account, _, _ := cfg.FindAccountWorkspace("primary@example.com", "space-primary")
		if !errors.Is(err, failure) || calls != 1 || account.CooldownUntil != "" || account.ConsecutiveFailures != 0 {
			t.Fatalf("stream=%v calls=%d err=%v cooldown=%s failures=%d", streaming, calls, err, account.CooldownUntil, account.ConsecutiveFailures)
		}
	}
}

func TestPinnedContinuationCooldownReturnsRetryAfterWithoutInference(t *testing.T) {
	a := newConversationRequestTestApp(t)
	failure := errors.New("unexpected end of JSON input")
	if err := a.State.finishWorkspaceDispatchFailure("primary@example.com", "space-primary", time.Now(), failure, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
		calls++
		return InferenceResult{}, nil
	}
	_, err := a.runPrompt(httptest.NewRequest(http.MethodPost, "/admin/test", nil), PromptRunRequest{
		Prompt: "continue", PinnedAccountEmail: "primary@example.com", PinnedSpaceID: "space-primary", UpstreamThreadID: "existing-thread",
	})
	if err == nil {
		t.Fatal("expected cooldown")
	}
	rec := httptest.NewRecorder()
	a.writeUpstreamError(rec, err)
	if calls != 0 || rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "本次未向 Notion 发送请求") {
		t.Fatalf("calls=%d status=%d headers=%v body=%s", calls, rec.Code, rec.Header(), rec.Body.String())
	}
}
