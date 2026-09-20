package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountConcurrencySharedAcrossWorkspaces(t *testing.T) {
	app := newConversationRequestTestApp(t)
	cfg, _, _ := app.State.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[0].Workspaces = append(cfg.Accounts[0].Workspaces, NotionWorkspace{ID: "second", SubscriptionTier: "business", MaxConcurrency: 4})
	if err := app.State.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if !app.State.TryAcquireWorkspaceDispatchSlot("primary@example.com", "") {
		t.Fatal("default workspace slot unavailable")
	}
	if app.State.TryAcquireWorkspaceDispatchSlot("primary@example.com", "second") {
		t.Fatal("second workspace bypassed shared account limit")
	}
	if !app.State.TryAcquireAccountDispatchSlot("backup@example.com") {
		t.Fatal("different account was blocked")
	}
	app.State.ReleaseAccountDispatchSlot("backup@example.com")
	app.State.ReleaseWorkspaceDispatchSlot("primary@example.com", "")
	if !app.State.TryAcquireWorkspaceDispatchSlot("primary@example.com", "second") {
		t.Fatal("default-workspace release leaked credential capacity")
	}
	app.State.ReleaseWorkspaceDispatchSlot("primary@example.com", "second")
	if got := app.State.loadAccountSlots()[credentialSlotKey("primary@example.com")].inflight.Load(); got != 0 {
		t.Fatalf("credential slots leaked: %d", got)
	}
	cfg.ActiveWorkspaceID = "second"
	if err := app.State.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if !app.State.TryAcquireAccountDispatchSlot("primary@example.com") {
		t.Fatal("active workspace slot unavailable")
	}
	app.State.ReleaseAccountDispatchSlot("primary@example.com")
	if got := app.State.loadAccountSlots()[credentialSlotKey("primary@example.com")].inflight.Load(); got != 0 {
		t.Fatal("active workspace release leaked shared capacity")
	}
}

func TestRateLimitAndTrustDenialPauseEveryWorkspaceWithoutReplay(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
			t.Run(fmt.Sprintf("status=%d/stream=%v", status, streaming), func(t *testing.T) {
				app := newConversationRequestTestApp(t)
				cfg, _, _ := app.State.Snapshot()
				cfg.Accounts = cloneAccounts(cfg.Accounts)
				cfg.Accounts[1].Disabled = true
				cfg.Accounts[0].Workspaces = append(cfg.Accounts[0].Workspaces, NotionWorkspace{ID: "second", SubscriptionTier: "business"})
				if err := app.State.SaveAndApply(cfg); err != nil {
					t.Fatal(err)
				}
				until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
				failure := &notionAPIError{StatusCode: status, RetryAfter: until, Message: "rate limited"}
				if status == http.StatusForbidden {
					failure.Message = `{"sub_type":"trust-rule-denied"}`
				}
				calls := 0
				app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, _ PromptRunRequest, _ func(string) error) (InferenceResult, error) {
					calls++
					return InferenceResult{}, failure
				}
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				var err error
				if streaming {
					_, err = app.runPromptStreamWithSink(r, PromptRunRequest{Prompt: "hello"}, InferenceStreamSink{})
				} else {
					_, err = app.runPrompt(r, PromptRunRequest{Prompt: "hello"})
				}
				if !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("calls=%d err=%v, want a single failed inference", calls, err)
				}
				live, _, _ := app.State.Snapshot()
				for _, workspace := range []string{"space-primary", "second"} {
					account, _, _ := live.FindAccountWorkspace("primary@example.com", workspace)
					if eligible, reason := accountDispatchEligible(live, account, time.Now()); eligible || reason != "credential_cooldown" {
						t.Fatalf("workspace %s was not paused: %s", workspace, reason)
					}
					if parseOptionalRFC3339(account.CredentialCooldownUntil).Before(until) {
						t.Fatal("Retry-After was shortened")
					}
				}
				_, err = resolveDispatchCandidates(live, PromptRunRequest{PinnedAccountEmail: "primary@example.com", WorkspaceID: "second"}, time.Now())
				rec := httptest.NewRecorder()
				app.writeUpstreamError(rec, err)
				if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
					t.Fatalf("cooldown lost retry metadata: %d %s", rec.Code, rec.Body.String())
				}
				if got := app.State.loadAccountSlots()[credentialSlotKey("primary@example.com")].inflight.Load(); got != 0 {
					t.Fatal("failure leaked credential slot")
				}
			})
		}
	}
}

func TestDefaultTrustDenialDoesNotUseBrowserFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(buildTrustRuleDeniedResponse(t, "thread-trust", "message-trust", "trust-rule-denied")))
	}))
	defer server.Close()
	client := newBrowserFallbackTestClient(server.URL)
	_, err := client.runInferenceTranscriptWithFallback(context.Background(), map[string]any{"threadId": "thread-trust"}, "thread-trust", InferenceStreamSink{})
	if !isTrustRuleDeniedInferenceError(err) || calls.Load() != 1 || isSessionRetryableError(err) {
		t.Fatalf("trust rejection replayed or lost: calls=%d err=%v", calls.Load(), err)
	}
}

func TestRetryAfterHeaderPreservedFromUpstream(t *testing.T) {
	for _, value := range []string{"600", time.Now().UTC().Add(time.Hour).Format(http.TimeFormat)} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", value)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			client := newBestEffortTestClient(server.URL)
			_, err := client.postJSONResponse(context.Background(), server.URL, map[string]any{}, "application/json")
			var apiErr *notionAPIError
			if !errors.As(err, &apiErr) || time.Until(apiErr.RetryAfter) < 9*time.Minute {
				t.Fatalf("retry header lost: %v", err)
			}
		})
	}
}

func TestRefusalRetryIsOptInAndBounded(t *testing.T) {
	for _, requested := range []int{0, -1, 1, 100} {
		for _, streaming := range []bool{false, true} {
			cfg := defaultConfig()
			cfg.Prompt.MaxRefusalRetries = requested
			calls := 0
			var emit func(string) error
			if streaming {
				emit = func(string) error { return nil }
			}
			_, err := runPromptWithPromptGuard(context.Background(), cfg, PromptRunRequest{Prompt: "hello"}, emit, func(context.Context, PromptRunRequest, func(string) error) (InferenceResult, error) {
				calls++
				return InferenceResult{Text: "I can only help with Notion workspace pages."}, nil
			})
			want := 1
			if requested > 0 {
				want = 2
			}
			if err != nil || calls != want {
				t.Fatalf("requested=%d streaming=%v calls=%d want=%d err=%v", requested, streaming, calls, want, err)
			}
		}
	}
	rec := httptest.NewRecorder()
	writePrometheusMetrics(rec)
	if !strings.Contains(rec.Body.String(), `notion2api_inference_activity_total{activity="continuation_calls"}`) {
		t.Fatal("reuse metric missing")
	}
}
