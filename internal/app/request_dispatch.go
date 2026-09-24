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
	"sync/atomic"
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
	return fmt.Errorf("%w; check Business workspace eligibility, disabled state, cooldown, local artifacts, or login status", errNoEligibleAccounts)
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
	candidates, err := resolveDispatchWorkspaceCandidates(cfg, poolCandidates, request, now)
	if err != nil {
		return nil, err
	}
	filtered := make([]NotionAccount, 0, len(candidates))
	var autoFallback []NotionAccount
	var selectionErr error
	for _, candidate := range candidates {
		if selected, err := selectWorkspaceModel(cfg, candidate, request); err != nil {
			selectionErr = err
		} else if selected.ModelSelectionMode == "auto_fallback" {
			autoFallback = append(autoFallback, candidate)
		} else {
			filtered = append(filtered, candidate)
		}
	}
	filtered = append(filtered, autoFallback...)
	if len(filtered) == 0 && selectionErr != nil {
		return nil, selectionErr
	}
	return filtered, nil
}

func resolveDispatchWorkspaceCandidates(cfg AppConfig, poolCandidates []NotionAccount, request PromptRunRequest, now time.Time) ([]NotionAccount, error) {
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
	selectedWorkspace := pinnedWorkspace
	if selectedWorkspace == "" {
		if account, _, ok := cfg.FindAccount(pinnedEmail); ok {
			selectedWorkspace = preferredAccountWorkspaceID(cfg, account)
		}
	}
	if request.AllowPinnedAccountFallback {
		var preferred *NotionAccount
		if account, _, ok := cfg.FindAccountWorkspace(pinnedEmail, selectedWorkspace); ok {
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
	account, _, ok := cfg.FindAccountWorkspace(pinnedEmail, selectedWorkspace)
	if !ok {
		// The account existing but its pinned workspace being gone (a removed
		// or expired workspace) is a different problem from a missing account
		// and needs a different answer from the operator.
		if _, _, found := cfg.FindAccount(pinnedEmail); found {
			return nil, fmt.Errorf("workspace %s is no longer available for account %s; switch workspace or start a new conversation", pinnedWorkspace, pinnedEmail)
		}
		return nil, fmt.Errorf("account %s not found", pinnedEmail)
	}
	account = ensureAccountPaths(cfg, account)
	if eligible, reason := accountDispatchEligible(cfg, account, now); !eligible {
		if reason == "credential_cooldown" {
			return nil, &notionAPIError{
				StatusCode: http.StatusTooManyRequests,
				RetryAfter: parseOptionalRFC3339(account.CredentialCooldownUntil),
				Message:    "account is paused after upstream rate limiting or trust rejection",
			}
		}
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

func (a *App) runPromptWithAccountPool(r *http.Request, request PromptRunRequest, onDelta func(string) error) (InferenceResult, error) {
	return a.dispatchPromptThroughPool(r, request, dispatchOutput{
		sink:      InferenceStreamSink{Text: onDelta},
		deltaOnly: true,
		streaming: onDelta != nil,
	})
}

func (a *App) runPromptWithAccountPoolWithSink(r *http.Request, request PromptRunRequest, sink InferenceStreamSink) (InferenceResult, error) {
	return a.dispatchPromptThroughPool(r, request, dispatchOutput{sink: sink, streaming: true})
}

// dispatchOutput describes where one dispatched prompt delivers its output.
// Both public entry points share a single candidate loop; they only differ in
// how the per-session runner is invoked, which keeps test overrides and the
// non-streaming client selection exactly as the separate loops had them.
type dispatchOutput struct {
	sink InferenceStreamSink
	// deltaOnly routes through runPromptWithSession (text callback only)
	// instead of runPromptWithSessionWithSink.
	deltaOnly bool
	streaming bool
}

// dispatchEmitState records what a dispatch already sent to the client. Sink
// callbacks may run on the upstream reader and on the keepalive goroutine, so
// both flags are atomic.
type dispatchEmitState struct {
	emitted    atomic.Bool
	clientGone atomic.Bool
}

func (s *dispatchEmitState) emittedAny() bool { return s.emitted.Load() }

// clientWrite types a failed client write as clientGoneError and remembers it,
// so the dispatcher still recognises the disconnect if the upstream client
// later wraps the error without %w.
func (s *dispatchEmitState) clientWrite(err error) error {
	if err == nil {
		return nil
	}
	s.clientGone.Store(true)
	if isClientGoneError(err) {
		return err
	}
	return &clientGoneError{err: err}
}

// wrapSink tracks visible output and types every client write failure as a
// clientGoneError. Nil callbacks stay nil: runPromptWithSessionWithSink picks
// the non-streaming client when the sink is entirely empty.
func (s *dispatchEmitState) wrapSink(sink InferenceStreamSink) InferenceStreamSink {
	out := InferenceStreamSink{}
	if sink.Text != nil {
		out.Text = func(delta string) error {
			if delta != "" {
				s.emitted.Store(true)
			}
			return s.clientWrite(sink.Text(delta))
		}
	}
	if sink.Reasoning != nil {
		out.Reasoning = func(delta string) error {
			if delta != "" {
				s.emitted.Store(true)
			}
			return s.clientWrite(sink.Reasoning(delta))
		}
	}
	if sink.ReasoningWarmup != nil {
		out.ReasoningWarmup = func() error { return s.clientWrite(sink.ReasoningWarmup()) }
	}
	if sink.KeepAlive != nil {
		out.KeepAlive = func() error { return s.clientWrite(sink.KeepAlive()) }
	}
	return out
}

// dispatchAttempt is the outcome of trying one candidate workspace.
type dispatchAttempt struct {
	result InferenceResult
	// err is the attempt's error. Without stop it only feeds lastErr.
	err error
	// stop ends the candidate loop and returns result/err as they are.
	stop bool
	// accountFailure marks err as a recorded account failure (prefixed with
	// the account email); other non-stop errors merely explain a skip.
	accountFailure bool
	cfg            AppConfig
}

func (a *App) resolveRequestDispatchCandidates(cfg AppConfig, request PromptRunRequest, now time.Time) ([]NotionAccount, error) {
	if a != nil && a.State != nil {
		if snap := a.State.snap.Load(); snap != nil {
			return resolveDispatchCandidatesFromSnapshot(snap, request, now)
		}
	}
	return resolveDispatchCandidates(cfg, request, now)
}

func (a *App) dispatchPromptThroughPool(r *http.Request, request PromptRunRequest, output dispatchOutput) (InferenceResult, error) {
	cfg, _, _ := a.State.Snapshot()
	if len(cfg.Accounts) == 0 {
		return InferenceResult{}, noEligibleAccountsError()
	}

	timeout := requestTimeout(cfg)
	if output.streaming {
		timeout = streamRequestTimeout(cfg)
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	rerun := func(next PromptRunRequest) (InferenceResult, error) {
		return a.dispatchPromptThroughPool(r, next, output)
	}
	candidates, err := a.resolveRequestDispatchCandidates(cfg, request, time.Now())
	if err != nil {
		if isQuotaExhaustedError(err) && cfg.ResolveContinuationFailover() {
			return a.retryContinuationOnAnotherAccount(r, request, rerun, err)
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

	emit := &dispatchEmitState{}
	wrapped := emit.wrapSink(output.sink)
	run := func(ctx context.Context, cfg AppConfig, session SessionInfo, email string, attempt PromptRunRequest) (InferenceResult, error) {
		if output.deltaOnly {
			return a.runPromptWithSession(ctx, cfg, session, email, attempt, wrapped.Text)
		}
		return a.runPromptWithSessionWithSink(ctx, cfg, session, email, attempt, wrapped)
	}

	var lastErr error
	for _, candidate := range candidates {
		attempt := a.dispatchCandidate(ctx, cfg, request, candidate, emit, run)
		cfg = attempt.cfg
		if attempt.stop {
			return attempt.result, attempt.err
		}
		if attempt.err == nil {
			continue
		}
		if attempt.accountFailure || lastErr == nil {
			lastErr = attempt.err
		}
		if attempt.accountFailure && emit.emittedAny() {
			return InferenceResult{}, lastErr
		}
	}

	if lastErr != nil {
		if !emit.emittedAny() && isQuotaExhaustedError(lastErr) && cfg.ResolveContinuationFailover() {
			return a.retryContinuationOnAnotherAccount(r, request, rerun, lastErr)
		}
		return InferenceResult{}, lastErr
	}
	return InferenceResult{}, noDispatchCapacityError()
}

// promptAttemptRequest isolates per-attempt execution state. The thread
// prepared for one candidate belongs to that candidate's workspace; handing it
// to another workspace would target a thread that does not exist there (or
// was half-created by the failed attempt), so each candidate starts clean,
// exactly like buildContinuationFailoverRequest does for a failover.
func promptAttemptRequest(base PromptRunRequest) PromptRunRequest {
	attempt := base
	attempt.preparedThreadID = ""
	attempt.onThreadPrepared = nil
	attempt.attachmentThreadReady = false
	return attempt
}

// abortsDispatch reports errors that end the whole request without touching
// account health: the caller went away, the request itself is invalid, or it
// is pinned to a different workspace.
func abortsDispatch(ctx context.Context, emit *dispatchEmitState, err error) bool {
	return isDispatchContextAbort(ctx, err) ||
		emit.clientGone.Load() || isClientGoneError(err) ||
		isClientInputError(err) ||
		errors.Is(err, errConversationWorkspaceMismatch)
}

// dispatchCandidate runs one candidate workspace. Every slot it acquires is
// released through a deferred lease, so a panic anywhere below cannot leak
// the credential slot; account bookkeeping is always recorded before the
// deferred release runs.
func (a *App) dispatchCandidate(ctx context.Context, cfg AppConfig, base PromptRunRequest, original NotionAccount, emit *dispatchEmitState, run func(context.Context, AppConfig, SessionInfo, string, PromptRunRequest) (InferenceResult, error)) (out dispatchAttempt) {
	out.cfg = cfg
	workspaceID := accountWorkspaceID(original)
	request := promptAttemptRequest(base)

	// Check the model against live capabilities before the attempt consumes
	// the workspace's request window; an unusable model is not an account
	// failure, it only means this candidate cannot serve the request.
	live, _, _ := a.State.Snapshot()
	if liveAccount, _, ok := live.FindAccountWorkspace(original.Email, workspaceID); ok {
		if _, selErr := selectWorkspaceModel(live, liveAccount, request); selErr != nil {
			out.err = selErr
			return out
		}
	}

	lease, ok := a.State.acquireWorkspaceDispatchSlot(original.Email, workspaceID, false)
	if !ok {
		return out
	}
	defer lease.release()
	account, started, startErr := a.State.beginWorkspaceDispatch(original.Email, workspaceID, time.Now())
	if startErr != nil {
		out.err, out.stop = startErr, true
		return out
	}
	if !started {
		out.err = accountQuotaCooldownError(account, time.Now())
		return out
	}

	succeed := func(result InferenceResult, email string, session SessionInfo, held *dispatchSlotLease) dispatchAttempt {
		held.release()
		result.AccountEmail = email
		// The upstream answer already exists; failing to persist bookkeeping
		// must not turn it into an error for the client.
		if saveErr := a.State.finishWorkspaceDispatchSuccess(email, workspaceID, session, time.Now(), shouldPersistDispatchedAccountAsActive(out.cfg, request, email)); saveErr != nil {
			log.Printf("[dispatch] recording success for %s workspace=%s failed: %v", email, workspaceID, saveErr)
		}
		return dispatchAttempt{result: result, stop: true, cfg: out.cfg}
	}

	session, err := a.loadReadyDispatchSession(ctx, cfg, account)
	if err == nil {
		a.preparePromptExecutionTarget(&request, account.Email, session.SpaceID)
		result, runErr := run(ctx, cfg, session, account.Email, request)
		if runErr == nil {
			return succeed(result, account.Email, session, lease)
		}
		err = runErr
	}
	if abortsDispatch(ctx, emit, err) {
		out.err, out.stop = err, true
		return out
	}
	if isModelSelectionError(err) {
		out.err = err
		return out
	}
	if !credentialBackoff(err, time.Now()).IsZero() {
		// Publish account-wide backoff before releasing capacity (the deferred
		// lease release), so another workspace cannot start while this
		// credential is being paused.
		if saveErr := a.State.finishWorkspaceDispatchFailure(account.Email, workspaceID, time.Now(), err, false); saveErr != nil {
			out.err, out.stop = fmt.Errorf("%w; saving account cooldown: %v", err, saveErr), true
			return out
		}
		out.err, out.accountFailure = fmt.Errorf("%s: %w", account.Email, err), true
		return out
	}
	// The refresh below makes network calls; it must not hold capacity.
	lease.release()

	retryable := isSessionRetryableError(err)
	if retryable && cfg.ResolveSessionRefresh().Enabled && !emit.emittedAny() {
		refreshedCfg, refreshErr := a.State.tryRefreshAccount(ctx, cfg, account)
		if refreshErr == nil {
			committedCfg, saveErr := a.State.commitAccountRefresh(cfg, account, refreshedCfg)
			if saveErr == nil {
				a.invalidateDispatchProbeCache()
				cfg = committedCfg
				out.cfg = cfg
				if refreshedAccount, _, ok := cfg.FindAccountWorkspace(account.Email, workspaceID); ok {
					refreshedSession, loadErr := a.loadReadyDispatchSession(ctx, cfg, refreshedAccount)
					if loadErr == nil {
						retryLease, ok := a.State.acquireWorkspaceDispatchSlot(refreshedAccount.Email, workspaceID, true)
						if !ok {
							// The refreshed account is healthy, merely busy: a
							// capacity miss is not an account failure.
							out.err = noDispatchCapacityError()
							return out
						}
						defer retryLease.release()
						result, retryErr := run(ctx, cfg, refreshedSession, refreshedAccount.Email, request)
						if retryErr == nil {
							return succeed(result, refreshedAccount.Email, refreshedSession, retryLease)
						}
						err = retryErr
					} else {
						err = loadErr
					}
					retryable = isSessionRetryableError(err)
				}
			} else {
				err = saveErr
				retryable = isSessionRetryableError(err)
			}
		}
	}
	if abortsDispatch(ctx, emit, err) {
		out.err, out.stop = err, true
		return out
	}
	if isModelSelectionError(err) {
		out.err = err
		return out
	}

	if retryable {
		reloginCfg, _ := a.State.startAutoRelogin(ctx, cfg, account, "request_auth_failed")
		if committedCfg, commitErr := a.State.commitAccountRefresh(cfg, account, reloginCfg); commitErr == nil {
			cfg = committedCfg
		} else {
			cfg = reloginCfg
		}
		out.cfg = cfg
		if updated, _, ok := cfg.FindAccountWorkspace(account.Email, workspaceID); ok {
			account = updated
		}
	}

	if saveErr := a.State.finishWorkspaceDispatchFailure(account.Email, workspaceID, time.Now(), err, retryable); saveErr != nil {
		out.err, out.stop = fmt.Errorf("%w; saving account failure: %v", err, saveErr), true
		return out
	}
	out.err, out.accountFailure = fmt.Errorf("%s: %w", account.Email, err), true
	return out
}
