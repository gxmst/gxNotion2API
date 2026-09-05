package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const syntheticQuotaExhaustedError = "thread 00000000-0000-4000-8000-0000000000aa failed: AI inference is not allowed. (sub_type=quota-exhausted trace_id=trace-synthetic)"

func TestIsQuotaExhaustedError(t *testing.T) {
	if !isQuotaExhaustedError(errors.New(syntheticQuotaExhaustedError)) {
		t.Fatal("upstream quota exhaustion marker was not recognized")
	}
	if !isQuotaExhaustedError(errors.New("SUB_TYPE=Quota-ExhaustED")) {
		t.Fatal("marker matching must be case insensitive")
	}
	for _, other := range []error{
		nil,
		errors.New("AI inference is not allowed. (sub_type=trust-rule-denied trace_id=trace-synthetic)"),
		errors.New("context canceled"),
		errors.New("connect: connection refused"),
	} {
		if isQuotaExhaustedError(other) {
			t.Fatalf("non-quota error misclassified: %v", other)
		}
	}
}

// A quota-exhausted failure must park the account far longer than the ordinary
// failure backoff and record when upstream made the confirmation, while a
// later success fully restores it.
func TestQuotaExhaustedFailureAppliesLongCooldown(t *testing.T) {
	cfg := defaultConfig()
	account := eligibleTestAccount(t, cfg)
	now := time.Now()

	failed := markAccountDispatchFailure(account, now, errors.New(syntheticQuotaExhaustedError), false)
	if failed.Status != "quota_exhausted" {
		t.Fatalf("status = %q, want quota_exhausted", failed.Status)
	}
	if failed.LastQuotaExhaustedAt == "" {
		t.Fatal("upstream confirmation timestamp was not recorded")
	}
	cooldown := parseOptionalRFC3339(failed.CooldownUntil)
	lower := now.Add(accountQuotaExhaustedCooldown - time.Minute)
	upper := now.Add(accountQuotaExhaustedCooldown + time.Minute)
	if cooldown.IsZero() || cooldown.Before(lower) || cooldown.After(upper) {
		t.Fatalf("cooldown_until = %q, want roughly the quota cooldown window [%s, %s]", failed.CooldownUntil, lower, upper)
	}
	if ok, reason := accountDispatchEligible(cfg, failed, now); ok || reason != "cooldown" {
		t.Fatalf("exhausted account still dispatchable: ok=%v reason=%q", ok, reason)
	}

	recovered := markAccountDispatchSuccess(failed, now.Add(accountQuotaExhaustedCooldown+time.Minute))
	if recovered.Status != "ready" || recovered.CooldownUntil != "" || recovered.ConsecutiveFailures != 0 {
		t.Fatalf("success did not restore the account: %+v", recovered)
	}
	if recovered.LastQuotaExhaustedAt == "" {
		t.Fatal("the last exhaustion timestamp is history and must survive a success")
	}
}

func TestBuildContinuationFailoverRequest(t *testing.T) {
	state := &ServerState{Conversations: newConversationStore()}
	app := &App{State: state}
	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "explain A"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer about A", ThreadID: "thread-1", AccountEmail: "a@example.com"})

	request := PromptRunRequest{
		Prompt:             "explain A",
		LatestUserPrompt:   "explain B",
		UpstreamThreadID:   "thread-1",
		ConversationID:     entry.ID,
		PinnedAccountEmail: "a@example.com",
		continuationDraft:  &continuationTurnDraft{SessionID: "sess-1"},
	}
	failover, ok := app.buildContinuationFailoverRequest(request)
	if !ok {
		t.Fatal("expected a failover request for a pinned continuation")
	}
	if failover.UpstreamThreadID != "" || failover.PinnedAccountEmail != "" || failover.continuationDraft != nil {
		t.Fatalf("failover request still pinned to the exhausted account: %+v", failover)
	}
	if !failover.continuationFailoverAttempted {
		t.Fatal("failover request is not marked as attempted")
	}
	for _, part := range []string{"explain A", "answer about A", "explain B"} {
		if !strings.Contains(failover.Prompt, part) {
			t.Fatalf("replay prompt missing %q:\n%s", part, failover.Prompt)
		}
	}
	if strings.Count(failover.Prompt, "explain B") != 1 {
		t.Fatalf("latest prompt duplicated in replay:\n%s", failover.Prompt)
	}
	if _, ok := app.buildContinuationFailoverRequest(failover); ok {
		t.Fatal("failover must not recurse")
	}
	if _, ok := app.buildContinuationFailoverRequest(PromptRunRequest{ConversationID: entry.ID}); ok {
		t.Fatal("non-continuations must not fail over")
	}
	if _, ok := app.buildContinuationFailoverRequest(PromptRunRequest{UpstreamThreadID: "thread-missing", ConversationID: "conv-missing"}); ok {
		t.Fatal("failover requires a known conversation record")
	}
}

func writeSyntheticProbeFile(t *testing.T, dir string, email string, spaceID string) string {
	t.Helper()
	payload := map[string]any{
		"email":          email,
		"user_id":        "synthetic-user-" + email,
		"space_id":       spaceID,
		"client_version": "synthetic-client-version",
		"cookies":        []map[string]string{{"name": "token_v2", "value": "synthetic-token-value"}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.ReplaceAll(strings.SplitN(email, "@", 2)[0], ".", "_") + ".json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// End to end through the dispatch pool: the pinned account's workspace is out
// of AI quota, so the continuation is rebuilt as a fresh-thread turn and
// served by another account, and the conversation is rebound to it.
func TestDispatchPoolFailsOverContinuationOnQuotaExhausted(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Accounts = []NotionAccount{
		{Email: "primary@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "primary@example.com", "space-primary"), SpaceID: "space-primary"},
		{Email: "backup@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "backup@example.com", "space-backup"), SpaceID: "space-backup"},
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	app.accountProtocolProbeOverride = func(context.Context, AppConfig, SessionInfo) error { return nil }

	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "first question"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "first answer", ThreadID: "thread-primary", AccountEmail: "primary@example.com"})
	if _, err := state.conversations().Continue(entry.ID, ConversationCreateRequest{Prompt: "second question"}); err != nil {
		t.Fatal(err)
	}

	var requests []PromptRunRequest
	app.runPromptWithSessionOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, _ func(string) error) (InferenceResult, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			return InferenceResult{}, errors.New(syntheticQuotaExhaustedError)
		}
		return InferenceResult{Text: "fresh answer", ThreadID: "thread-backup"}, nil
	}

	request := PromptRunRequest{
		Prompt:             "second question",
		LatestUserPrompt:   "second question",
		PublicModel:        "gpt-5.4",
		NotionModel:        "notion-model-synthetic",
		ConversationID:     entry.ID,
		UpstreamThreadID:   "thread-primary",
		PinnedAccountEmail: "primary@example.com",
	}
	result, err := app.runPrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "fresh answer" || result.AccountEmail != "backup@example.com" {
		t.Fatalf("unexpected failover result: %+v", result)
	}
	if len(requests) != 2 {
		t.Fatalf("dispatch attempts = %d, want 2", len(requests))
	}
	if requests[0].UpstreamThreadID != "thread-primary" {
		t.Fatalf("first attempt lost its thread: %+v", requests[0])
	}
	second := requests[1]
	if second.UpstreamThreadID != "" || second.PinnedAccountEmail != "" || !second.continuationFailoverAttempted {
		t.Fatalf("failover request was not rebuilt: %+v", second)
	}
	for _, part := range []string{"first question", "first answer", "second question"} {
		if !strings.Contains(second.Prompt, part) {
			t.Fatalf("replay prompt missing %q:\n%s", part, second.Prompt)
		}
	}
	rebound, ok := state.conversations().Get(entry.ID)
	if !ok {
		t.Fatal("conversation vanished")
	}
	if rebound.AccountEmail != "backup@example.com" || rebound.ThreadID == "thread-primary" {
		t.Fatalf("conversation was not rebound: thread=%q account=%q", rebound.ThreadID, rebound.AccountEmail)
	}
	liveCfg, _, _ := state.Snapshot()
	account, _, ok := liveCfg.FindAccount("primary@example.com")
	if !ok || account.Status != "quota_exhausted" || !accountCooldownActive(account, time.Now()) {
		t.Fatalf("exhausted account was not parked: %+v", account)
	}
}

func TestDispatchPoolFailoverDisabledByFeature(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	enabled := false
	cfg.Features.ContinuationFailover = &enabled
	cfg.Accounts = []NotionAccount{
		{Email: "primary@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "primary@example.com", "space-primary"), SpaceID: "space-primary"},
		{Email: "backup@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "backup@example.com", "space-backup"), SpaceID: "space-backup"},
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	app.accountProtocolProbeOverride = func(context.Context, AppConfig, SessionInfo) error { return nil }

	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "hello"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "thread-primary", AccountEmail: "primary@example.com"})

	attempts := 0
	app.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
		attempts++
		return InferenceResult{}, errors.New(syntheticQuotaExhaustedError)
	}
	request := PromptRunRequest{
		Prompt:             "next",
		LatestUserPrompt:   "next",
		PublicModel:        "gpt-5.4",
		ConversationID:     entry.ID,
		UpstreamThreadID:   "thread-primary",
		PinnedAccountEmail: "primary@example.com",
	}
	_, err = app.runPrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request)
	if err == nil || !isQuotaExhaustedError(err) {
		t.Fatalf("expected the original quota error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("dispatch attempts = %d, want 1 (failover disabled)", attempts)
	}
}

func TestDispatchPoolFailoverOnlyForQuotaErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Accounts = []NotionAccount{
		{Email: "primary@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "primary@example.com", "space-primary"), SpaceID: "space-primary"},
		{Email: "backup@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "backup@example.com", "space-backup"), SpaceID: "space-backup"},
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	app.accountProtocolProbeOverride = func(context.Context, AppConfig, SessionInfo) error { return nil }

	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "hello"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "thread-primary", AccountEmail: "primary@example.com"})

	attempts := 0
	app.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
		attempts++
		return InferenceResult{}, errors.New("upstream exploded: internal error")
	}
	request := PromptRunRequest{
		Prompt:             "next",
		LatestUserPrompt:   "next",
		PublicModel:        "gpt-5.4",
		ConversationID:     entry.ID,
		UpstreamThreadID:   "thread-primary",
		PinnedAccountEmail: "primary@example.com",
	}
	_, err = app.runPrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request)
	if err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("expected the original error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("dispatch attempts = %d, want 1 (non-quota errors must not fail over)", attempts)
	}
}

// With a single account there is nowhere to fail over to: the caller must see
// the upstream quota error, not a capacity error about missing accounts.
func TestDispatchPoolFailoverPreservesQuotaErrorWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Accounts = []NotionAccount{
		{Email: "primary@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "primary@example.com", "space-primary"), SpaceID: "space-primary"},
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	app.accountProtocolProbeOverride = func(context.Context, AppConfig, SessionInfo) error { return nil }

	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "hello"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "thread-primary", AccountEmail: "primary@example.com"})

	attempts := 0
	app.runPromptWithSessionOverride = func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error) {
		attempts++
		return InferenceResult{}, errors.New(syntheticQuotaExhaustedError)
	}
	request := PromptRunRequest{
		Prompt:             "next",
		LatestUserPrompt:   "next",
		PublicModel:        "gpt-5.4",
		ConversationID:     entry.ID,
		UpstreamThreadID:   "thread-primary",
		PinnedAccountEmail: "primary@example.com",
	}
	_, err = app.runPrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request)
	if err == nil || !isQuotaExhaustedError(err) {
		t.Fatalf("expected the original quota error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("dispatch attempts = %d, want 1", attempts)
	}
}

// The streaming dispatch variant must fail over the same way.
func TestDispatchPoolFailoverStreamVariant(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Accounts = []NotionAccount{
		{Email: "primary@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "primary@example.com", "space-primary"), SpaceID: "space-primary"},
		{Email: "backup@example.com", ProbeJSON: writeSyntheticProbeFile(t, dir, "backup@example.com", "space-backup"), SpaceID: "space-backup"},
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}
	app.accountProtocolProbeOverride = func(context.Context, AppConfig, SessionInfo) error { return nil }

	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "hello"})
	state.conversations().Complete(entry.ID, InferenceResult{Text: "answer", ThreadID: "thread-primary", AccountEmail: "primary@example.com"})

	attempts := 0
	var lastRequest PromptRunRequest
	app.runPromptWithSessionSinkOverride = func(_ context.Context, _ AppConfig, _ SessionInfo, request PromptRunRequest, sink InferenceStreamSink) (InferenceResult, error) {
		attempts++
		lastRequest = request
		if attempts == 1 {
			return InferenceResult{}, errors.New(syntheticQuotaExhaustedError)
		}
		if err := sink.EmitText("streamed"); err != nil {
			return InferenceResult{}, err
		}
		return InferenceResult{Text: "streamed", ThreadID: "thread-backup"}, nil
	}
	request := PromptRunRequest{
		Prompt:             "next",
		LatestUserPrompt:   "next",
		PublicModel:        "gpt-5.4",
		ConversationID:     entry.ID,
		UpstreamThreadID:   "thread-primary",
		PinnedAccountEmail: "primary@example.com",
	}
	result, err := app.runPromptStreamWithSink(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request, InferenceStreamSink{
		Text: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "streamed" || result.AccountEmail != "backup@example.com" {
		t.Fatalf("unexpected failover result: %+v", result)
	}
	if attempts != 2 || lastRequest.UpstreamThreadID != "" || lastRequest.PinnedAccountEmail != "" {
		t.Fatalf("stream failover did not rebuild the request: attempts=%d request=%+v", attempts, lastRequest)
	}
}

func TestGetAIUsageEligibilityMergesV2AndV1(t *testing.T) {
	var spaceIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		spaceIDs = append(spaceIDs, stringValue(payload["spaceId"]))
		switch r.URL.Path {
		case "/api/v3/getAIUsageEligibilityV2":
			writeJSON(w, http.StatusOK, map[string]any{
				"usage": map[string]any{
					"currentServicePeriod": map[string]any{"spaceUsage": 0, "userUsage": 0},
					"lifetime":             map[string]any{"spaceUsage": 40, "userUsage": 30},
					"totalCreditBalance":   25,
					"lastSpaceUsageAtMs":   1788000000000,
				},
				"basicCredits":   map[string]any{"spaceUsage": 40, "spaceLimit": 100, "userUsage": 30, "userLimit": 50},
				"premiumCredits": map[string]any{"totalCreditBalance": 25, "servicePeriodStartMs": 1787000000000},
			})
		case "/api/v3/getAIUsageEligibility":
			writeJSON(w, http.StatusOK, map[string]any{
				"isEligible": true,
				"type":       "metered",
				"spaceUsage": 40, "spaceLimit": 100, "userUsage": 30, "userLimit": 50,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := defaultConfig()
	cfg.UpstreamBaseURL = server.URL
	client := newNotionAIClient(SessionInfo{
		UserID:        "synthetic-user",
		SpaceID:       "space-x",
		ClientVersion: "synthetic-client-version",
		Cookies:       []ProbeCookie{{Name: "token_v2", Value: "synthetic-token-value"}},
	}, cfg, "user@example.com")

	usage, err := client.getAIUsageEligibility(context.Background(), "space-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(spaceIDs) != 2 || spaceIDs[0] != "space-x" || spaceIDs[1] != "space-x" {
		t.Fatalf("upstream requests did not carry the space id: %v", spaceIDs)
	}
	if !usage.IsEligibleKnown || !usage.IsEligible {
		t.Fatalf("eligibility flag lost: %+v", usage)
	}
	if usage.Type != "metered" || !usage.QuotaEnforced {
		t.Fatalf("allowance type handling wrong: %+v", usage)
	}
	if usage.SpaceUsage != 40 || usage.SpaceLimit != 100 || usage.UserUsage != 30 || usage.UserLimit != 50 {
		t.Fatalf("usage counters wrong: %+v", usage)
	}
	if !usage.PremiumCreditKnown || usage.PremiumCreditBalance != 25 {
		t.Fatalf("premium credits lost: %+v", usage)
	}
	if usage.LastUsageAtMs != 1788000000000 {
		t.Fatalf("last usage timestamp lost: %+v", usage)
	}

	// On an unlimited workspace the free-tier limits must not be presented as
	// an enforced quota.
	unlimited := usage
	unlimited.Type = "unlimited"
	unlimited.QuotaEnforced = false
	if unlimited.QuotaEnforced {
		t.Fatal("unlimited workspaces are not quota enforced")
	}
}

func TestGetAIUsageEligibilityFallsBackToV1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/getAIUsageEligibility" {
			writeJSON(w, http.StatusOK, map[string]any{
				"isEligible": false,
				"type":       "metered",
				"spaceUsage": 100, "spaceLimit": 100, "userUsage": 50, "userLimit": 50,
				"lastSpaceUsageAtMs": 1788000000000,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	cfg := defaultConfig()
	cfg.UpstreamBaseURL = server.URL
	client := newNotionAIClient(SessionInfo{
		UserID:        "synthetic-user",
		SpaceID:       "space-x",
		ClientVersion: "synthetic-client-version",
		Cookies:       []ProbeCookie{{Name: "token_v2", Value: "synthetic-token-value"}},
	}, cfg, "user@example.com")

	usage, err := client.getAIUsageEligibility(context.Background(), "space-x")
	if err != nil {
		t.Fatal(err)
	}
	if !usage.IsEligibleKnown || usage.IsEligible {
		t.Fatalf("v1 eligibility flag wrong: %+v", usage)
	}
	if !usage.QuotaEnforced || usage.UserUsage != 50 || usage.UserLimit != 50 {
		t.Fatalf("v1 usage numbers wrong: %+v", usage)
	}
	if usage.PremiumCreditKnown {
		t.Fatal("v2-only fields must stay unknown when v2 is unreachable")
	}
}

func TestGetAIUsageEligibilityFailsWhenBothEndpointsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	cfg := defaultConfig()
	cfg.UpstreamBaseURL = server.URL
	client := newNotionAIClient(SessionInfo{
		UserID:        "synthetic-user",
		SpaceID:       "space-x",
		ClientVersion: "synthetic-client-version",
		Cookies:       []ProbeCookie{{Name: "token_v2", Value: "synthetic-token-value"}},
	}, cfg, "user@example.com")

	if _, err := client.getAIUsageEligibility(context.Background(), "space-x"); err == nil {
		t.Fatal("both endpoints failing must surface an error")
	}
	if _, err := client.getAIUsageEligibility(context.Background(), "  "); err == nil {
		t.Fatal("an empty space id must be rejected before any request")
	}
}

func TestWorkspaceAIUsageReportCachesAndRefreshes(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Accounts = []NotionAccount{{Email: "usage@example.com", SpaceID: "space-u"}}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}

	fetches := 0
	app.workspaceAIUsageFetchOverride = func(context.Context, AppConfig, NotionAccount) (workspaceAIUsage, error) {
		fetches++
		return workspaceAIUsage{IsEligible: true, IsEligibleKnown: true, Type: "unlimited"}, nil
	}
	account := cfg.Accounts[0]
	first := app.workspaceAIUsageReport(context.Background(), cfg, account, false)
	if first.Status != "ok" || first.Usage == nil || first.Cached || fetches != 1 {
		t.Fatalf("first report wrong: %+v fetches=%d", first, fetches)
	}
	second := app.workspaceAIUsageReport(context.Background(), cfg, account, false)
	if !second.Cached || fetches != 1 {
		t.Fatalf("second report should come from the cache: %+v fetches=%d", second, fetches)
	}
	forced := app.workspaceAIUsageReport(context.Background(), cfg, account, true)
	if forced.Cached || fetches != 2 {
		t.Fatalf("forced refresh bypassed the cache: %+v fetches=%d", forced, fetches)
	}

	app.workspaceAIUsageFetchOverride = func(context.Context, AppConfig, NotionAccount) (workspaceAIUsage, error) {
		fetches++
		return workspaceAIUsage{}, errors.New("upstream denied")
	}
	failed := app.workspaceAIUsageReport(context.Background(), cfg, account, true)
	if failed.Status != "error" || failed.Usage != nil || failed.Detail == "" {
		t.Fatalf("error report wrong: %+v", failed)
	}
	cachedError := app.workspaceAIUsageReport(context.Background(), cfg, account, false)
	if !cachedError.Cached || cachedError.Status != "error" || fetches != 3 {
		t.Fatalf("errors should be cached within the ttl window: %+v fetches=%d", cachedError, fetches)
	}
}

func TestAdminAIUsageEndpoint(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Admin.Enabled = true
	cfg.Admin.Password = "admin-secret"
	cfg.Accounts = []NotionAccount{
		{Email: "withspace@example.com", SpaceID: "space-ok"},
		{Email: "nospace@example.com"},
		{Email: "off@example.com", Disabled: true, SpaceID: "space-off"},
	}
	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := &App{State: state}

	fetches := 0
	app.workspaceAIUsageFetchOverride = func(_ context.Context, _ AppConfig, account NotionAccount) (workspaceAIUsage, error) {
		fetches++
		if account.Email == "withspace@example.com" {
			return workspaceAIUsage{IsEligible: true, IsEligibleKnown: true, Type: "unlimited", UserUsage: 7, UserLimit: 75}, nil
		}
		return workspaceAIUsage{}, errors.New("upstream denied")
	}

	rec := httptest.NewRecorder()
	app.handleAdminAccountsAIUsage(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts/ai-usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Accounts   []workspaceAIUsageReport `json:"accounts"`
		TTLSeconds int                      `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 3 || payload.TTLSeconds != int(aiUsageCacheTTL.Seconds()) {
		t.Fatalf("unexpected payload: %s", rec.Body.String())
	}
	byEmail := map[string]workspaceAIUsageReport{}
	for _, report := range payload.Accounts {
		byEmail[report.Email] = report
	}
	if got := byEmail["withspace@example.com"]; got.Status != "ok" || got.Usage == nil || got.Usage.UserUsage != 7 {
		t.Fatalf("healthy account report wrong: %+v", got)
	}
	if got := byEmail["nospace@example.com"]; got.Status != "unknown" || got.Usage != nil || got.Detail == "" {
		t.Fatalf("workspace-less account must be unknown: %+v", got)
	}
	if got := byEmail["off@example.com"]; got.Status != "unknown" || got.Detail == "" {
		t.Fatalf("disabled account must be unknown without a fetch: %+v", got)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (only the eligible account is queried)", fetches)
	}

	rec = httptest.NewRecorder()
	app.handleAdminAccountsAIUsage(rec, httptest.NewRequest(http.MethodPut, "/admin/accounts/ai-usage", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("put status = %d, want 405", rec.Code)
	}
}
