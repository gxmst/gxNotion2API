package app

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultStreamingRequestTimeoutSec  = 900
	dispatchProtocolProbeTimeoutCapSec = 20
)

var errDispatchCapacityExceeded = errors.New("dispatch capacity exceeded")

var transportClientNewTotalMetric = expvar.NewMap("notion2api_transport_client_new_total")

type probeCacheEntry struct {
	lastChecked time.Time
	lastOK      bool
}

type probeCache struct {
	mu      sync.Mutex
	entries map[string]probeCacheEntry
}

func newProbeCache() *probeCache {
	return &probeCache{
		entries: map[string]probeCacheEntry{},
	}
}

func (c *probeCache) shouldProbe(accountKey string, ttl time.Duration, now time.Time) bool {
	if c == nil {
		return true
	}
	if strings.TrimSpace(accountKey) == "" {
		return true
	}
	if ttl <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]probeCacheEntry{}
		return true
	}
	entry, ok := c.entries[accountKey]
	if !ok {
		return true
	}
	if !entry.lastOK {
		return true
	}
	return now.Sub(entry.lastChecked) >= ttl
}

func (c *probeCache) markSuccess(accountKey string, now time.Time) {
	if c == nil {
		return
	}
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]probeCacheEntry{}
	}
	c.entries[accountKey] = probeCacheEntry{lastChecked: now, lastOK: true}
}

func (c *probeCache) markFailure(accountKey string) {
	if c == nil {
		return
	}
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, accountKey)
}

func (c *probeCache) invalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]probeCacheEntry{}
}

func requestTimeout(cfg AppConfig) time.Duration {
	return time.Duration(maxInt(cfg.TimeoutSec, 10)) * time.Second
}

func streamRequestTimeout(cfg AppConfig) time.Duration {
	return time.Duration(maxInt(cfg.TimeoutSec, defaultStreamingRequestTimeoutSec)) * time.Second
}

var errNoEligibleAccounts = errors.New("no usable accounts available")

func noEligibleAccountsError() error {
	return fmt.Errorf("%w; check disabled state, local artifacts, or login status", errNoEligibleAccounts)
}

func isNoEligibleAccountsError(err error) bool {
	return errors.Is(err, errNoEligibleAccounts)
}

func noDispatchCapacityError() error {
	return fmt.Errorf("%w: too many concurrent requests for available accounts", errDispatchCapacityExceeded)
}

func isDispatchCapacityExceededError(err error) bool {
	return errors.Is(err, errDispatchCapacityExceeded)
}

func mergeDispatchCandidates(preferred *NotionAccount, candidates []NotionAccount) []NotionAccount {
	out := make([]NotionAccount, 0, len(candidates)+1)
	seen := map[string]struct{}{}
	appendCandidate := func(account NotionAccount) {
		key := dispatchWorkspaceKey(account)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, account)
	}
	if preferred != nil {
		appendCandidate(*preferred)
	}
	for _, account := range candidates {
		appendCandidate(account)
	}
	return out
}

func resolveDispatchCandidates(cfg AppConfig, request PromptRunRequest, now time.Time) ([]NotionAccount, error) {
	poolCandidates := buildDispatchCandidateOrder(cfg, now)
	return resolveDispatchCandidatesWithPool(cfg, poolCandidates, request, now)
}

func resolveDispatchCandidatesFromSnapshot(bundle *snapshotBundle, request PromptRunRequest, now time.Time) ([]NotionAccount, error) {
	if bundle == nil {
		return nil, noEligibleAccountsError()
	}
	return resolveDispatchCandidatesWithPool(bundle.Config, pickDispatchCandidatesFromSnapshot(bundle, now), request, now)
}

func resolveDispatchCandidatesWithPool(cfg AppConfig, poolCandidates []NotionAccount, request PromptRunRequest, now time.Time) ([]NotionAccount, error) {
	pinnedEmail := strings.TrimSpace(request.PinnedAccountEmail)
	pinnedWorkspace := firstNonEmpty(request.PinnedSpaceID, request.WorkspaceID)
	filterWorkspace := func(candidates []NotionAccount) []NotionAccount {
		if pinnedWorkspace == "" {
			return candidates
		}
		filtered := make([]NotionAccount, 0, len(candidates))
		for _, candidate := range candidates {
			if strings.TrimSpace(accountWorkspaceID(candidate)) == pinnedWorkspace {
				filtered = append(filtered, candidate)
			}
		}
		return filtered
	}
	if pinnedEmail == "" {
		poolCandidates = filterWorkspace(poolCandidates)
		if len(poolCandidates) == 0 {
			return nil, noEligibleAccountsError()
		}
		return poolCandidates, nil
	}
	if request.AllowPinnedAccountFallback {
		var preferred *NotionAccount
		if account, _, ok := cfg.FindAccountWorkspace(pinnedEmail, pinnedWorkspace); ok {
			account = ensureAccountPaths(cfg, account)
			if eligible, _ := accountDispatchEligible(cfg, account, now); eligible {
				preferred = &account
			}
		}
		candidates := mergeDispatchCandidates(preferred, filterWorkspace(poolCandidates))
		if len(candidates) == 0 {
			return nil, noEligibleAccountsError()
		}
		return candidates, nil
	}
	account, _, ok := cfg.FindAccountWorkspace(pinnedEmail, pinnedWorkspace)
	if !ok {
		return nil, fmt.Errorf("account %s not found", pinnedEmail)
	}
	account = ensureAccountPaths(cfg, account)
	if eligible, reason := accountDispatchEligible(cfg, account, now); !eligible {
		if reason == "cooldown" {
			if quotaErr := accountQuotaCooldownError(account, now); quotaErr != nil {
				return nil, quotaErr
			}
		}
		return nil, fmt.Errorf("account %s is not dispatchable: %s", account.Email, reason)
	}
	return []NotionAccount{account}, nil
}

// buildContinuationFailoverRequest rebuilds a pinned continuation as a
// fresh-thread turn. The original upstream thread belongs to the exhausted
// account's workspace, where no other account can write, so continuing the
// conversation means replaying its history into a new thread. The caller runs
// the returned request through the account pool again; the flag on the request
// keeps that retry from fanning out into a second failover.
func (a *App) buildContinuationFailoverRequest(request PromptRunRequest) (PromptRunRequest, bool) {
	if request.continuationFailoverAttempted {
		return PromptRunRequest{}, false
	}
	if strings.TrimSpace(request.UpstreamThreadID) == "" || strings.TrimSpace(request.ConversationID) == "" {
		return PromptRunRequest{}, false
	}
	conversation, ok := a.State.conversations().Get(strings.TrimSpace(request.ConversationID))
	if !ok {
		return PromptRunRequest{}, false
	}
	failover := request
	failover.UpstreamThreadID = ""
	failover.continuationDraft = nil
	failover.continuationScaffold = nil
	failover.PinnedAccountEmail = ""
	failover.PinnedSpaceID = ""
	failover.WorkspaceID = ""
	failover.HiddenPrompt = firstNonEmpty(request.HiddenPrompt, conversation.HiddenPrompt)
	failover.AllowPinnedAccountFallback = false
	failover.preparedThreadID = ""
	failover.onThreadPrepared = nil
	failover.SessionRepeatTurn = false
	failover.replayResult = nil
	failover.attachmentThreadReady = false
	failover.Prompt = buildFreshThreadReplayPromptFromConversation(conversation, request.LatestUserPrompt, request.Attachments, request.Prompt)
	if history := exactConversationSegments(request.HistorySegments); len(history) > 1 {
		latest := latestReplayPrompt(request.LatestUserPrompt, request.Attachments, "")
		last := history[len(history)-1]
		if latest != "" && (last.Role != "user" || last.Text != latest) {
			history = append(history, conversationPromptSegment{Role: "user", Text: latest})
		}
		failover.Prompt = buildConversationTranscriptPrompt(history)
	}
	failover.continuationFailoverAttempted = true
	return failover, true
}

// retryContinuationOnAnotherAccount runs the fresh-thread continuation through
// the account pool. When no other account can take the turn either, the
// original quota error is preserved: a capacity error would only obscure what
// actually went wrong.
func (a *App) retryContinuationOnAnotherAccount(r *http.Request, request PromptRunRequest, run func(PromptRunRequest) (InferenceResult, error), originalErr error) (InferenceResult, error) {
	failoverRequest, ok := a.buildContinuationFailoverRequest(request)
	if !ok {
		return InferenceResult{}, originalErr
	}
	log.Printf("[dispatch] upstream AI quota exhausted on the pinned account; retrying conversation %s on another account in a fresh thread", strings.TrimSpace(request.ConversationID))
	result, err := run(failoverRequest)
	// When no other account can take the turn either (all cooling down, gone,
	// or out of capacity), the original quota error is preserved: a pool-level
	// availability error would only obscure what actually went wrong.
	if err != nil && (isDispatchCapacityExceededError(err) || isNoEligibleAccountsError(err)) {
		return InferenceResult{}, originalErr
	}
	return result, err
}

func shouldPersistDispatchedAccountAsActive(cfg AppConfig, request PromptRunRequest, accountEmail string) bool {
	accountKey := canonicalEmailKey(accountEmail)
	if accountKey == "" {
		return false
	}
	activeKey := canonicalEmailKey(cfg.ActiveAccount)
	if activeKey == "" {
		return true
	}
	if _, _, ok := cfg.ResolveActiveAccount(); !ok {
		return true
	}
	return activeKey == accountKey
}

func dispatchProtocolProbeTimeout(cfg AppConfig) time.Duration {
	seconds := maxInt(minInt(cfg.TimeoutSec, dispatchProtocolProbeTimeoutCapSec), 5)
	return time.Duration(seconds) * time.Second
}

func dispatchProbeCacheTTL(cfg AppConfig) time.Duration {
	seconds := cfg.Dispatch.ProbeCacheTTLSeconds
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func isDispatchContextAbort(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

func (a *App) shouldProbeAccountProtocolHealth(accountKey string, ttl time.Duration, now time.Time) bool {
	if a == nil {
		return true
	}
	if a.State == nil || a.State.DispatchProbeCache == nil {
		return true
	}
	return a.State.DispatchProbeCache.shouldProbe(accountKey, ttl, now)
}

func (a *App) markAccountProtocolProbeSuccess(accountKey string, now time.Time) {
	if a == nil {
		return
	}
	if a.State == nil || a.State.DispatchProbeCache == nil {
		return
	}
	a.State.DispatchProbeCache.markSuccess(accountKey, now)
}

func (a *App) markAccountProtocolProbeFailure(accountKey string) {
	if a == nil {
		return
	}
	if a.State == nil || a.State.DispatchProbeCache == nil {
		return
	}
	a.State.DispatchProbeCache.markFailure(accountKey)
}

func (a *App) invalidateDispatchProbeCache() {
	if a == nil {
		return
	}
	if a.State == nil || a.State.DispatchProbeCache == nil {
		return
	}
	a.State.DispatchProbeCache.invalidateAll()
}

func (a *App) probeAccountProtocolHealth(ctx context.Context, cfg AppConfig, session SessionInfo, accountEmail string) error {
	accountKey := canonicalEmailKey(accountEmail)
	if accountKey == "" {
		accountKey = canonicalEmailKey(session.UserEmail)
	}
	if workspaceID := strings.TrimSpace(session.SpaceID); workspaceID != "" {
		accountKey += "\x00" + workspaceID
	}
	now := time.Now()
	ttl := dispatchProbeCacheTTL(cfg)
	if !a.shouldProbeAccountProtocolHealth(accountKey, ttl, now) {
		return nil
	}
	if a.accountProtocolProbeOverride != nil {
		err := a.accountProtocolProbeOverride(ctx, cfg, session)
		if err == nil {
			a.markAccountProtocolProbeSuccess(accountKey, now)
			return nil
		}
		if isDispatchContextAbort(ctx, err) {
			a.markAccountProtocolProbeSuccess(accountKey, now)
			return nil
		}
		a.markAccountProtocolProbeFailure(accountKey)
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, dispatchProtocolProbeTimeout(cfg))
	defer cancel()
	client := newNotionAIClient(session, cfg, accountEmail)
	_, err := client.listInferenceTranscripts(probeCtx)
	if isDispatchContextAbort(probeCtx, err) {
		a.markAccountProtocolProbeSuccess(accountKey, now)
		return nil
	}
	if err != nil {
		a.markAccountProtocolProbeFailure(accountKey)
		return err
	}
	a.markAccountProtocolProbeSuccess(accountKey, now)
	return err
}

func (a *App) loadReadyDispatchSession(ctx context.Context, cfg AppConfig, account NotionAccount) (SessionInfo, error) {
	session, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		return SessionInfo{}, err
	}
	if err := a.probeAccountProtocolHealth(ctx, cfg, session, account.Email); err != nil {
		return SessionInfo{}, err
	}
	return session, nil
}

func (a *App) loadPrimarySession(ctx context.Context, cfg AppConfig, snapshot SessionInfo, refreshReason string) (SessionInfo, error) {
	if strings.TrimSpace(snapshot.UserID) != "" && strings.TrimSpace(snapshot.SpaceID) != "" && len(snapshot.Cookies) > 0 {
		return snapshot, nil
	}
	if cfg.ResolveSessionRefresh().Enabled {
		if refreshErr := a.State.RefreshSession(ctx, refreshReason); refreshErr == nil {
			_, refreshed, _ := a.State.Snapshot()
			if strings.TrimSpace(refreshed.UserID) != "" && strings.TrimSpace(refreshed.SpaceID) != "" && len(refreshed.Cookies) > 0 {
				return refreshed, nil
			}
		}
	}
	probePath, userName, spaceName, activeEmail := cfg.ResolveSessionTarget()
	if strings.TrimSpace(probePath) == "" {
		return SessionInfo{}, fmt.Errorf("no active notion session configured; login or activate an account first")
	}
	if account, _, found := cfg.FindAccount(activeEmail); found {
		return loadSessionInfoForAccountRefresh(cfg, account)
	}
	return loadSessionInfo(probePath, userName, spaceName)
}

func (a *App) runPromptActiveFallback(r *http.Request, request PromptRunRequest, onDelta func(string) error) (InferenceResult, error) {
	cfg, snapshotSession, _ := a.State.Snapshot()
	_, _, _, activeEmail := cfg.ResolveSessionTarget()
	timeout := requestTimeout(cfg)
	if onDelta != nil {
		timeout = streamRequestTimeout(cfg)
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	session, err := a.loadPrimarySession(ctx, cfg, snapshotSession, "client_missing_fallback")
	if err != nil {
		return InferenceResult{}, err
	}
	if err := a.probeAccountProtocolHealth(ctx, cfg, session, activeEmail); err != nil {
		return InferenceResult{}, err
	}

	emittedAny := false
	wrappedDelta := func(delta string) error {
		if delta != "" {
			emittedAny = true
		}
		if onDelta == nil {
			return nil
		}
		return onDelta(delta)
	}

	a.preparePromptExecutionTarget(&request, activeEmail, session.SpaceID)
	result, err := a.runPromptWithSession(ctx, cfg, session, activeEmail, request, wrappedDelta)
	if err == nil {
		result.AccountEmail = firstNonEmpty(result.AccountEmail, activeEmail)
		return result, nil
	}
	if cfg.ResolveSessionRefresh().RetryOnAuthError && isSessionRetryableError(err) && !emittedAny {
		if refreshErr := a.State.RefreshSession(ctx, "prompt_retry_fallback"); refreshErr == nil {
			a.invalidateDispatchProbeCache()
			_, refreshed, _ := a.State.Snapshot()
			if strings.TrimSpace(refreshed.UserID) != "" && strings.TrimSpace(refreshed.SpaceID) != "" && len(refreshed.Cookies) > 0 {
				if probeErr := a.probeAccountProtocolHealth(ctx, cfg, refreshed, activeEmail); probeErr != nil {
					return InferenceResult{}, probeErr
				}
				result, retryErr := a.runPromptWithSession(ctx, cfg, refreshed, activeEmail, request, wrappedDelta)
				result.AccountEmail = firstNonEmpty(result.AccountEmail, activeEmail)
				return result, retryErr
			}
		}
	}
	return InferenceResult{}, err
}

func (a *App) runPromptActiveFallbackWithSink(r *http.Request, request PromptRunRequest, sink InferenceStreamSink) (InferenceResult, error) {
	cfg, snapshotSession, _ := a.State.Snapshot()
	_, _, _, activeEmail := cfg.ResolveSessionTarget()
	timeout := streamRequestTimeout(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	session, err := a.loadPrimarySession(ctx, cfg, snapshotSession, "client_missing_fallback")
	if err != nil {
		return InferenceResult{}, err
	}
	if err := a.probeAccountProtocolHealth(ctx, cfg, session, activeEmail); err != nil {
		return InferenceResult{}, err
	}

	emittedAny := false
	wrappedText := func(delta string) error {
		if delta != "" {
			emittedAny = true
		}
		return sink.EmitText(delta)
	}
	wrappedReasoning := func(delta string) error {
		if delta != "" {
			emittedAny = true
		}
		return sink.EmitReasoning(delta)
	}
	wrappedReasoningWarmup := func() error {
		return sink.EmitReasoningWarmup()
	}
	wrappedKeepAlive := func() error {
		return sink.EmitKeepAlive()
	}

	a.preparePromptExecutionTarget(&request, activeEmail, session.SpaceID)
	result, err := a.runPromptWithSessionWithSink(ctx, cfg, session, activeEmail, request, InferenceStreamSink{
		Text:            wrappedText,
		Reasoning:       wrappedReasoning,
		ReasoningWarmup: wrappedReasoningWarmup,
		KeepAlive:       wrappedKeepAlive,
	})
	if err == nil {
		result.AccountEmail = firstNonEmpty(result.AccountEmail, activeEmail)
		return result, nil
	}
	if cfg.ResolveSessionRefresh().RetryOnAuthError && isSessionRetryableError(err) && !emittedAny {
		if refreshErr := a.State.RefreshSession(ctx, "prompt_retry_fallback"); refreshErr == nil {
			a.invalidateDispatchProbeCache()
			_, refreshed, _ := a.State.Snapshot()
			if strings.TrimSpace(refreshed.UserID) != "" && strings.TrimSpace(refreshed.SpaceID) != "" && len(refreshed.Cookies) > 0 {
				if probeErr := a.probeAccountProtocolHealth(ctx, cfg, refreshed, activeEmail); probeErr != nil {
					return InferenceResult{}, probeErr
				}
				result, retryErr := a.runPromptWithSessionWithSink(ctx, cfg, refreshed, activeEmail, request, InferenceStreamSink{
					Text:            wrappedText,
					Reasoning:       wrappedReasoning,
					ReasoningWarmup: wrappedReasoningWarmup,
					KeepAlive:       wrappedKeepAlive,
				})
				result.AccountEmail = firstNonEmpty(result.AccountEmail, activeEmail)
				return result, retryErr
			}
		}
	}
	return InferenceResult{}, err
}

func (a *App) runPromptWithAccountPool(r *http.Request, request PromptRunRequest, onDelta func(string) error) (InferenceResult, error) {
	cfg, _, _ := a.State.Snapshot()
	if len(cfg.Accounts) == 0 {
		return a.runPromptActiveFallback(r, request, onDelta)
	}

	timeout := requestTimeout(cfg)
	if onDelta != nil {
		timeout = streamRequestTimeout(cfg)
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	now := time.Now()
	var candidates []NotionAccount
	var err error
	if a != nil && a.State != nil {
		if snap := a.State.snap.Load(); snap != nil {
			candidates, err = resolveDispatchCandidatesFromSnapshot(snap, request, now)
		} else {
			candidates, err = resolveDispatchCandidates(cfg, request, now)
		}
	} else {
		candidates, err = resolveDispatchCandidates(cfg, request, now)
	}
	if err != nil {
		if isQuotaExhaustedError(err) && cfg.ResolveContinuationFailover() {
			return a.retryContinuationOnAnotherAccount(r, request, func(next PromptRunRequest) (InferenceResult, error) {
				return a.runPromptWithAccountPool(r, next, onDelta)
			}, err)
		}
		return InferenceResult{}, err
	}
	candidateKeys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidateKeys = append(candidateKeys, dispatchWorkspaceKey(candidate))
	}
	if a.State.AvailableDispatchCapacityKeys(candidateKeys) <= 0 {
		return InferenceResult{}, noDispatchCapacityError()
	}

	emittedAny := false
	wrappedDelta := func(delta string) error {
		if delta != "" {
			emittedAny = true
		}
		if onDelta == nil {
			return nil
		}
		return onDelta(delta)
	}

	var lastErr error
	for _, original := range candidates {
		workspaceID := accountWorkspaceID(original)
		if !a.State.TryAcquireWorkspaceDispatchSlot(original.Email, workspaceID) {
			continue
		}
		slotAcquired := true
		account, started, startErr := a.State.beginWorkspaceDispatch(original.Email, workspaceID, time.Now())
		if startErr != nil {
			a.State.ReleaseWorkspaceDispatchSlot(original.Email, workspaceID)
			return InferenceResult{}, startErr
		}
		if !started {
			a.State.ReleaseWorkspaceDispatchSlot(original.Email, workspaceID)
			if quotaErr := accountQuotaCooldownError(account, time.Now()); quotaErr != nil {
				lastErr = quotaErr
			}
			continue
		}
		session, err := a.loadReadyDispatchSession(ctx, cfg, account)
		if err == nil {
			a.preparePromptExecutionTarget(&request, account.Email, session.SpaceID)
			result, runErr := a.runPromptWithSession(ctx, cfg, session, account.Email, request, wrappedDelta)
			if runErr == nil {
				if slotAcquired {
					a.State.ReleaseWorkspaceDispatchSlot(account.Email, workspaceID)
					slotAcquired = false
				}
				result.AccountEmail = account.Email
				if saveErr := a.State.finishWorkspaceDispatchSuccess(account.Email, workspaceID, session, time.Now(), shouldPersistDispatchedAccountAsActive(cfg, request, account.Email)); saveErr != nil {
					return InferenceResult{}, saveErr
				}
				return result, nil
			}
			err = runErr
		}
		if slotAcquired {
			a.State.ReleaseWorkspaceDispatchSlot(account.Email, workspaceID)
			slotAcquired = false
		}
		if isDispatchContextAbort(ctx, err) || errors.Is(err, errConversationWorkspaceMismatch) {
			return InferenceResult{}, err
		}

		retryable := isSessionRetryableError(err)
		if retryable && cfg.ResolveSessionRefresh().Enabled && !emittedAny {
			refreshedCfg, refreshErr := a.State.tryRefreshAccount(ctx, cfg, account)
			if refreshErr == nil {
				if committedCfg, saveErr := a.State.commitAccountRefresh(cfg, account, refreshedCfg); saveErr == nil {
					a.invalidateDispatchProbeCache()
					cfg = committedCfg
					refreshedAccount, _, ok := cfg.FindAccountWorkspace(account.Email, workspaceID)
					if ok {
						refreshedSession, loadErr := a.loadReadyDispatchSession(ctx, cfg, refreshedAccount)
						if loadErr == nil {
							if !a.State.TryAcquireWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID) {
								err = noDispatchCapacityError()
								retryable = false
							} else {
								retrySlotAcquired := true
								result, retryErr := a.runPromptWithSession(ctx, cfg, refreshedSession, refreshedAccount.Email, request, wrappedDelta)
								if retryErr == nil {
									if retrySlotAcquired {
										a.State.ReleaseWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID)
										retrySlotAcquired = false
									}
									result.AccountEmail = refreshedAccount.Email
									if saveErr := a.State.finishWorkspaceDispatchSuccess(refreshedAccount.Email, workspaceID, refreshedSession, time.Now(), shouldPersistDispatchedAccountAsActive(cfg, request, refreshedAccount.Email)); saveErr != nil {
										return InferenceResult{}, saveErr
									}
									return result, nil
								}
								if retrySlotAcquired {
									a.State.ReleaseWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID)
									retrySlotAcquired = false
								}
								err = retryErr
								retryable = isSessionRetryableError(err)
							}
						} else {
							err = loadErr
							retryable = isSessionRetryableError(err)
						}
					}
				} else {
					err = saveErr
					retryable = isSessionRetryableError(err)
				}
			}
		}
		if isDispatchContextAbort(ctx, err) || errors.Is(err, errConversationWorkspaceMismatch) {
			return InferenceResult{}, err
		}

		if retryable {
			reloginCfg, _ := a.State.startAutoRelogin(ctx, cfg, account, "request_auth_failed")
			if committedCfg, commitErr := a.State.commitAccountRefresh(cfg, account, reloginCfg); commitErr == nil {
				cfg = committedCfg
			} else {
				cfg = reloginCfg
			}
			if updated, _, ok := cfg.FindAccountWorkspace(account.Email, workspaceID); ok {
				account = updated
			}
		}

		_ = a.State.finishWorkspaceDispatchFailure(account.Email, workspaceID, time.Now(), err, retryable)
		lastErr = fmt.Errorf("%s: %w", account.Email, err)
		if emittedAny {
			return InferenceResult{}, lastErr
		}
	}

	if lastErr != nil {
		if !emittedAny && isQuotaExhaustedError(lastErr) && cfg.ResolveContinuationFailover() {
			return a.retryContinuationOnAnotherAccount(r, request, func(next PromptRunRequest) (InferenceResult, error) {
				return a.runPromptWithAccountPool(r, next, onDelta)
			}, lastErr)
		}
		return InferenceResult{}, lastErr
	}
	return InferenceResult{}, noDispatchCapacityError()
}

func (a *App) runPromptWithAccountPoolWithSink(r *http.Request, request PromptRunRequest, sink InferenceStreamSink) (InferenceResult, error) {
	cfg, _, _ := a.State.Snapshot()
	if len(cfg.Accounts) == 0 {
		return a.runPromptActiveFallbackWithSink(r, request, sink)
	}

	timeout := streamRequestTimeout(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	now := time.Now()
	var candidates []NotionAccount
	var err error
	if a != nil && a.State != nil {
		if snap := a.State.snap.Load(); snap != nil {
			candidates, err = resolveDispatchCandidatesFromSnapshot(snap, request, now)
		} else {
			candidates, err = resolveDispatchCandidates(cfg, request, now)
		}
	} else {
		candidates, err = resolveDispatchCandidates(cfg, request, now)
	}
	if err != nil {
		if isQuotaExhaustedError(err) && cfg.ResolveContinuationFailover() {
			return a.retryContinuationOnAnotherAccount(r, request, func(next PromptRunRequest) (InferenceResult, error) {
				return a.runPromptWithAccountPoolWithSink(r, next, sink)
			}, err)
		}
		return InferenceResult{}, err
	}
	candidateKeys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidateKeys = append(candidateKeys, dispatchWorkspaceKey(candidate))
	}
	if a.State.AvailableDispatchCapacityKeys(candidateKeys) <= 0 {
		return InferenceResult{}, noDispatchCapacityError()
	}

	emittedAny := false
	wrappedText := func(delta string) error {
		if delta != "" {
			emittedAny = true
		}
		return sink.EmitText(delta)
	}
	wrappedReasoning := func(delta string) error {
		if delta != "" {
			emittedAny = true
		}
		return sink.EmitReasoning(delta)
	}
	wrappedReasoningWarmup := func() error {
		return sink.EmitReasoningWarmup()
	}
	wrappedKeepAlive := func() error {
		return sink.EmitKeepAlive()
	}

	var lastErr error
	for _, original := range candidates {
		workspaceID := accountWorkspaceID(original)
		if !a.State.TryAcquireWorkspaceDispatchSlot(original.Email, workspaceID) {
			continue
		}
		slotAcquired := true
		account, started, startErr := a.State.beginWorkspaceDispatch(original.Email, workspaceID, time.Now())
		if startErr != nil {
			a.State.ReleaseWorkspaceDispatchSlot(original.Email, workspaceID)
			return InferenceResult{}, startErr
		}
		if !started {
			a.State.ReleaseWorkspaceDispatchSlot(original.Email, workspaceID)
			if quotaErr := accountQuotaCooldownError(account, time.Now()); quotaErr != nil {
				lastErr = quotaErr
			}
			continue
		}
		session, err := a.loadReadyDispatchSession(ctx, cfg, account)
		if err == nil {
			a.preparePromptExecutionTarget(&request, account.Email, session.SpaceID)
			result, runErr := a.runPromptWithSessionWithSink(ctx, cfg, session, account.Email, request, InferenceStreamSink{
				Text:            wrappedText,
				Reasoning:       wrappedReasoning,
				ReasoningWarmup: wrappedReasoningWarmup,
				KeepAlive:       wrappedKeepAlive,
			})
			if runErr == nil {
				if slotAcquired {
					a.State.ReleaseWorkspaceDispatchSlot(account.Email, workspaceID)
					slotAcquired = false
				}
				result.AccountEmail = account.Email
				if saveErr := a.State.finishWorkspaceDispatchSuccess(account.Email, workspaceID, session, time.Now(), shouldPersistDispatchedAccountAsActive(cfg, request, account.Email)); saveErr != nil {
					return InferenceResult{}, saveErr
				}
				return result, nil
			}
			err = runErr
		}
		if slotAcquired {
			a.State.ReleaseWorkspaceDispatchSlot(account.Email, workspaceID)
			slotAcquired = false
		}
		if isDispatchContextAbort(ctx, err) || errors.Is(err, errConversationWorkspaceMismatch) {
			return InferenceResult{}, err
		}

		retryable := isSessionRetryableError(err)
		if retryable && cfg.ResolveSessionRefresh().Enabled && !emittedAny {
			refreshedCfg, refreshErr := a.State.tryRefreshAccount(ctx, cfg, account)
			if refreshErr == nil {
				if committedCfg, saveErr := a.State.commitAccountRefresh(cfg, account, refreshedCfg); saveErr == nil {
					a.invalidateDispatchProbeCache()
					cfg = committedCfg
					if refreshedAccount, _, ok := cfg.FindAccountWorkspace(account.Email, workspaceID); ok {
						refreshedSession, loadErr := a.loadReadyDispatchSession(ctx, cfg, refreshedAccount)
						if loadErr == nil {
							if !a.State.TryAcquireWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID) {
								err = noDispatchCapacityError()
								retryable = false
							} else {
								retrySlotAcquired := true
								result, retryErr := a.runPromptWithSessionWithSink(ctx, cfg, refreshedSession, refreshedAccount.Email, request, InferenceStreamSink{
									Text:            wrappedText,
									Reasoning:       wrappedReasoning,
									ReasoningWarmup: wrappedReasoningWarmup,
									KeepAlive:       wrappedKeepAlive,
								})
								if retryErr == nil {
									if retrySlotAcquired {
										a.State.ReleaseWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID)
										retrySlotAcquired = false
									}
									result.AccountEmail = refreshedAccount.Email
									if saveErr := a.State.finishWorkspaceDispatchSuccess(refreshedAccount.Email, workspaceID, refreshedSession, time.Now(), shouldPersistDispatchedAccountAsActive(cfg, request, refreshedAccount.Email)); saveErr != nil {
										return InferenceResult{}, saveErr
									}
									return result, nil
								}
								if retrySlotAcquired {
									a.State.ReleaseWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID)
									retrySlotAcquired = false
								}
								err = retryErr
								retryable = isSessionRetryableError(err)
							}
						} else {
							err = loadErr
							retryable = isSessionRetryableError(err)
						}
					}
				} else {
					err = saveErr
					retryable = isSessionRetryableError(err)
				}
			}
		}
		if isDispatchContextAbort(ctx, err) || errors.Is(err, errConversationWorkspaceMismatch) {
			return InferenceResult{}, err
		}

		if retryable {
			reloginCfg, _ := a.State.startAutoRelogin(ctx, cfg, account, "request_auth_failed")
			if committedCfg, commitErr := a.State.commitAccountRefresh(cfg, account, reloginCfg); commitErr == nil {
				cfg = committedCfg
			} else {
				cfg = reloginCfg
			}
			if updated, _, ok := cfg.FindAccountWorkspace(account.Email, workspaceID); ok {
				account = updated
			}
		}

		_ = a.State.finishWorkspaceDispatchFailure(account.Email, workspaceID, time.Now(), err, retryable)
		lastErr = fmt.Errorf("%s: %w", account.Email, err)
		if emittedAny {
			return InferenceResult{}, lastErr
		}
	}

	if lastErr != nil {
		if !emittedAny && isQuotaExhaustedError(lastErr) && cfg.ResolveContinuationFailover() {
			return a.retryContinuationOnAnotherAccount(r, request, func(next PromptRunRequest) (InferenceResult, error) {
				return a.runPromptWithAccountPoolWithSink(r, next, sink)
			}, lastErr)
		}
		return InferenceResult{}, lastErr
	}
	return InferenceResult{}, noDispatchCapacityError()
}
