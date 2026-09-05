package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	accountCooldownBase        = 2 * time.Minute
	accountCooldownMax         = 30 * time.Minute
	accountAutoReloginInterval = 5 * time.Minute
	// accountQuotaExhaustedCooldown is how long an account sits out after
	// upstream confirmed its workspace AI allowance is spent. Notion's
	// allowance window is monthly, but an hour keeps a misclassification from
	// parking an account for a month while still stopping the every-two-minutes
	// retry storm the ordinary failure backoff would produce.
	accountQuotaExhaustedCooldown = time.Hour
)

func parseOptionalRFC3339(value string) time.Time {
	clean := strings.TrimSpace(value)
	if clean == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, clean)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func formatRFC3339OrEmpty(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func resetAccountUsageWindow(account *NotionAccount, now time.Time) {
	if account == nil || account.HourlyQuota <= 0 {
		account.WindowStartedAt = ""
		account.WindowRequestCount = 0
		return
	}
	startedAt := parseOptionalRFC3339(account.WindowStartedAt)
	if startedAt.IsZero() || now.Sub(startedAt) >= time.Hour {
		account.WindowStartedAt = formatRFC3339OrEmpty(now)
		account.WindowRequestCount = 0
	}
}

func accountRemainingQuota(account NotionAccount, now time.Time) (int, bool) {
	if account.HourlyQuota <= 0 {
		return 0, false
	}
	resetAccountUsageWindow(&account, now)
	remaining := account.HourlyQuota - account.WindowRequestCount
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

func accountCooldownActive(account NotionAccount, now time.Time) bool {
	until := parseOptionalRFC3339(account.CooldownUntil)
	return !until.IsZero() && now.Before(until)
}

func accountHasUsableArtifacts(cfg AppConfig, account NotionAccount) bool {
	account = ensureAccountPaths(cfg, account)
	return fileExists(account.ProbeJSON) || fileExists(account.StorageStatePath)
}

func accountDispatchEligible(cfg AppConfig, account NotionAccount, now time.Time) (bool, string) {
	account = ensureAccountPaths(cfg, account)
	if account.Disabled {
		return false, "disabled"
	}
	if !accountHasUsableArtifacts(cfg, account) {
		return false, "missing_artifacts"
	}
	if accountCooldownActive(account, now) {
		return false, "cooldown"
	}
	if remaining, limited := accountRemainingQuota(account, now); limited && remaining <= 0 {
		return false, "quota_exhausted"
	}
	return true, "ready"
}

func computeAccountCooldown(account NotionAccount, retryable bool) time.Duration {
	failures := account.ConsecutiveFailures
	if failures < 1 {
		failures = 1
	}
	wait := time.Duration(failures) * accountCooldownBase
	if !retryable {
		wait /= 2
	}
	if wait < 30*time.Second {
		wait = 30 * time.Second
	}
	if wait > accountCooldownMax {
		wait = accountCooldownMax
	}
	return wait
}

func markAccountDispatchStart(account NotionAccount, now time.Time) NotionAccount {
	resetAccountUsageWindow(&account, now)
	if account.HourlyQuota > 0 {
		if strings.TrimSpace(account.WindowStartedAt) == "" {
			account.WindowStartedAt = formatRFC3339OrEmpty(now)
		}
		account.WindowRequestCount++
	}
	account.LastUsedAt = formatRFC3339OrEmpty(now)
	if strings.TrimSpace(account.Status) == "" || strings.EqualFold(account.Status, "new") {
		account.Status = "ready"
	}
	return account
}

func markAccountDispatchSuccess(account NotionAccount, now time.Time) NotionAccount {
	account.Status = "ready"
	account.LastError = ""
	account.LastUsedAt = formatRFC3339OrEmpty(now)
	account.LastSuccessAt = formatRFC3339OrEmpty(now)
	account.CooldownUntil = ""
	account.ConsecutiveFailures = 0
	account.TotalSuccesses++
	return account
}

// isQuotaExhaustedError reports upstream AI-quota exhaustion. Notion surfaces
// it inside the inference error text as a sub_type marker; it is the one
// account-level failure where retrying after the ordinary backoff is pointless,
// because the workspace allowance itself is spent.
func isQuotaExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "quota-exhausted")
}

func markAccountDispatchFailure(account NotionAccount, now time.Time, err error, retryable bool) NotionAccount {
	account.TotalFailures++
	account.ConsecutiveFailures++
	account.LastUsedAt = formatRFC3339OrEmpty(now)
	account.LastError = strings.TrimSpace(err.Error())
	if isQuotaExhaustedError(err) {
		// Upstream confirmed the workspace allowance is spent. The ordinary
		// failure backoff (minutes) would just re-hit the wall, so sit out a
		// long cooldown and keep the confirmation timestamp for the admin view.
		account.Status = "quota_exhausted"
		account.LastQuotaExhaustedAt = formatRFC3339OrEmpty(now)
		account.CooldownUntil = formatRFC3339OrEmpty(now.Add(accountQuotaExhaustedCooldown))
		return account
	}
	if !strings.EqualFold(strings.TrimSpace(account.Status), "pending_code") {
		if retryable {
			account.Status = "expired"
		} else {
			account.Status = "failed"
		}
	}
	// The cooldown scales with the consecutive-failure count that was just
	// incremented; without it a failing account stayed immediately dispatchable
	// and the recorded status never matched the dispatch behaviour.
	account.CooldownUntil = formatRFC3339OrEmpty(now.Add(computeAccountCooldown(account, retryable)))
	return account
}

func markAccountReloginPending(account NotionAccount, now time.Time) NotionAccount {
	account.Status = "pending_code"
	account.LastReloginAt = formatRFC3339OrEmpty(now)
	return account
}

func accountReloginRecentlyStarted(account NotionAccount, now time.Time) bool {
	last := parseOptionalRFC3339(account.LastReloginAt)
	return !last.IsZero() && now.Sub(last) < accountAutoReloginInterval
}

func sortDispatchCandidates(cfg AppConfig, accounts []NotionAccount, now time.Time) {
	activeKey := canonicalEmailKey(cfg.ActiveAccount)
	sort.Slice(accounts, func(i, j int) bool {
		left := accounts[i]
		right := accounts[j]
		leftKey := getAccountEmailKey(left)
		rightKey := getAccountEmailKey(right)
		leftActive := leftKey == activeKey
		rightActive := rightKey == activeKey
		if leftActive != rightActive {
			return leftActive
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		leftRemaining, leftLimited := accountRemainingQuota(left, now)
		rightRemaining, rightLimited := accountRemainingQuota(right, now)
		if leftLimited != rightLimited {
			return !leftLimited
		}
		if leftLimited && rightLimited && leftRemaining != rightRemaining {
			return leftRemaining > rightRemaining
		}
		if left.ConsecutiveFailures != right.ConsecutiveFailures {
			return left.ConsecutiveFailures < right.ConsecutiveFailures
		}
		leftUsed := parseOptionalRFC3339(left.LastUsedAt)
		rightUsed := parseOptionalRFC3339(right.LastUsedAt)
		if leftUsed.IsZero() != rightUsed.IsZero() {
			return leftUsed.IsZero()
		}
		if !leftUsed.Equal(rightUsed) {
			return leftUsed.Before(rightUsed)
		}
		return leftKey < rightKey
	})
}

func buildDispatchCandidateOrder(cfg AppConfig, now time.Time) []NotionAccount {
	candidates := make([]NotionAccount, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		account = ensureAccountPaths(cfg, account)
		if ok, _ := accountDispatchEligible(cfg, account, now); ok {
			candidates = append(candidates, account)
		}
	}
	sortDispatchCandidates(cfg, candidates, now)
	return candidates
}

func pickDispatchCandidatesFromSnapshot(bundle *snapshotBundle, now time.Time) []NotionAccount {
	if bundle == nil {
		return nil
	}
	if len(bundle.DispatchOrder) > 0 {
		return bundle.DispatchOrder
	}
	return buildDispatchCandidateOrder(bundle.Config, now)
}

func applyAccountUpdate(cfg AppConfig, account NotionAccount, makeActive bool) AppConfig {
	account = ensureAccountPaths(cfg, account)
	// Callers here hand over a full record they just updated, so its runtime
	// counters are authoritative -- including the zeros a success writes.
	cfg.UpsertAccountRuntimeState(account)
	if makeActive {
		cfg.ActiveAccount = account.Email
		cfg.ProbeJSON = account.ProbeJSON
	}
	return cfg
}

func (s *ServerState) startAutoRelogin(ctx context.Context, cfg AppConfig, account NotionAccount, reason string) (AppConfig, error) {
	now := time.Now()
	account = ensureAccountPaths(cfg, account)
	if strings.TrimSpace(account.Email) == "" {
		return cfg, fmt.Errorf("account email missing for auto relogin")
	}
	if accountReloginRecentlyStarted(account, now) {
		return cfg, fmt.Errorf("auto relogin already started recently for %s", account.Email)
	}
	status, err := StartEmailLogin(ctx, cfg, LoginStartRequest{
		Email:            account.Email,
		ProfileDir:       account.ProfileDir,
		PendingPath:      account.PendingStatePath,
		StorageStatePath: account.StorageStatePath,
		AccountEmail:     account.Email,
	})
	account = mergeAccountWithStatus(cfg, account, status)
	account = markAccountReloginPending(account, now)
	if err != nil {
		account.LastError = firstNonEmpty(status.Error, status.Message, err.Error())
		cfg = applyAccountUpdate(cfg, account, false)
		return cfg, fmt.Errorf("auto relogin start failed for %s (%s): %w", account.Email, reason, err)
	}
	account.LastError = ""
	cfg = applyAccountUpdate(cfg, account, false)
	return cfg, fmt.Errorf("verification code required for %s; auto relogin started (%s)", account.Email, reason)
}

func (a *App) runPromptWithSession(ctx context.Context, cfg AppConfig, session SessionInfo, accountEmail string, request PromptRunRequest, onDelta func(string) error) (InferenceResult, error) {
	if a.runPromptWithSessionOverride != nil {
		return a.runPromptWithSessionOverride(ctx, cfg, session, request, onDelta)
	}
	transportClientNewTotalMetric.Add("standard", 1)
	client := newNotionAIClient(session, cfg, accountEmail)
	if onDelta != nil {
		transportClientNewTotalMetric.Add("streaming", 1)
		client = newNotionAIStreamingClient(session, cfg, accountEmail)
	}
	execute := func(ctx context.Context, current PromptRunRequest, forward func(string) error) (InferenceResult, error) {
		if forward == nil {
			return client.RunPrompt(ctx, current)
		}
		return client.RunPromptStream(ctx, current, forward)
	}
	return execute(ctx, request, onDelta)
}

func (a *App) runPromptWithSessionWithSink(ctx context.Context, cfg AppConfig, session SessionInfo, accountEmail string, request PromptRunRequest, sink InferenceStreamSink) (InferenceResult, error) {
	if a.runPromptWithSessionSinkOverride != nil {
		return a.runPromptWithSessionSinkOverride(ctx, cfg, session, request, sink)
	}
	if a.runPromptWithSessionOverride != nil {
		return a.runPromptWithSessionOverride(ctx, cfg, session, request, sink.Text)
	}
	transportClientNewTotalMetric.Add("streaming", 1)
	client := newNotionAIStreamingClient(session, cfg, accountEmail)
	if sink.Text == nil && sink.Reasoning == nil && sink.ReasoningWarmup == nil && sink.KeepAlive == nil {
		transportClientNewTotalMetric.Add("standard", 1)
		client = newNotionAIClient(session, cfg, accountEmail)
	}
	if sink.Reasoning != nil || sink.ReasoningWarmup != nil || sink.KeepAlive != nil {
		return client.RunPromptStreamWithSink(ctx, request, sink)
	}
	execute := func(ctx context.Context, current PromptRunRequest, forward func(string) error) (InferenceResult, error) {
		if forward == nil {
			return client.RunPrompt(ctx, current)
		}
		return client.RunPromptStreamWithSink(ctx, current, InferenceStreamSink{
			Text:            forward,
			Reasoning:       sink.Reasoning,
			ReasoningWarmup: sink.ReasoningWarmup,
			KeepAlive:       sink.KeepAlive,
		})
	}
	return execute(ctx, request, sink.Text)
}

// cloneAccounts copies an account slice before mutation. Config snapshots share
// their backing arrays, so writing an entry into a snapshot's slice in place
// would leak the change into every other reader of that snapshot.
func cloneAccounts(accounts []NotionAccount) []NotionAccount {
	return append([]NotionAccount(nil), accounts...)
}

// mergeSessionMetadataWithoutOverwritingConfig folds session-reported metadata
// into an account record without clobbering what is already configured. The
// workspace an operator picked must survive every dispatch, so only blanks are
// backfilled here; authoritative fresh session data reaches the account through
// the explicit session-refresh path, which owns that decision.
func mergeSessionMetadataWithoutOverwritingConfig(account NotionAccount, session SessionInfo) NotionAccount {
	account.UserID = firstNonEmpty(account.UserID, session.UserID)
	account.UserName = firstNonEmpty(account.UserName, session.UserName)
	account.SpaceID = firstNonEmpty(account.SpaceID, session.SpaceID)
	account.SpaceViewID = firstNonEmpty(account.SpaceViewID, session.SpaceViewID)
	account.SpaceName = firstNonEmpty(account.SpaceName, session.SpaceName)
	account.ClientVersion = firstNonEmpty(account.ClientVersion, session.ClientVersion)
	return account
}

// accountIdentityUnchanged reports whether an account kept the same identity and
// configuration between two snapshots. Runtime bookkeeping (windows, cooldowns,
// counters) is deliberately excluded: it changes on every dispatch, and only a
// concurrent edit to who the account is or where it points must block a commit.
func accountIdentityUnchanged(started NotionAccount, current NotionAccount) bool {
	return strings.TrimSpace(started.ProbeJSON) == strings.TrimSpace(current.ProbeJSON) &&
		strings.TrimSpace(started.ProfileDir) == strings.TrimSpace(current.ProfileDir) &&
		strings.TrimSpace(started.StorageStatePath) == strings.TrimSpace(current.StorageStatePath) &&
		strings.TrimSpace(started.UserID) == strings.TrimSpace(current.UserID) &&
		strings.TrimSpace(started.UserName) == strings.TrimSpace(current.UserName) &&
		strings.TrimSpace(started.SpaceID) == strings.TrimSpace(current.SpaceID) &&
		strings.TrimSpace(started.SpaceViewID) == strings.TrimSpace(current.SpaceViewID) &&
		strings.TrimSpace(started.SpaceName) == strings.TrimSpace(current.SpaceName) &&
		strings.TrimSpace(started.ClientVersion) == strings.TrimSpace(current.ClientVersion) &&
		strings.TrimSpace(started.PlanType) == strings.TrimSpace(current.PlanType)
}

// saveAndApplyCommitted is the persistence step shared by the dispatch state
// helpers below. It expects refreshMu to be held and routes through the
// test hook so tests can intercept saves.
func (s *ServerState) saveAndApplyCommitted(cfg AppConfig) error {
	save := s.saveAndApplyLocked
	if testHookSaveAndApply != nil {
		save = func(cfg AppConfig) error {
			return testHookSaveAndApply(s, cfg)
		}
	}
	return save(cfg)
}

// beginAccountDispatch re-checks dispatch eligibility against the live state and
// records the dispatch start on the account record before any upstream work
// happens. The read-modify-write is serialised on refreshMu so two concurrent
// dispatches cannot both start from the same stale snapshot and silently drop
// each other's window bookkeeping. A false `started` means "not dispatchable
// right now, try the next candidate"; an error means the account is gone
// entirely and the request cannot proceed.
func (s *ServerState) beginAccountDispatch(email string, now time.Time) (NotionAccount, bool, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cfg, _, _ := s.Snapshot()
	account, index, ok := cfg.FindAccount(email)
	if !ok {
		return NotionAccount{}, false, fmt.Errorf("account %s not found", email)
	}
	if eligible, _ := accountDispatchEligible(cfg, account, now); !eligible {
		// Not dispatchable right now (disabled, cooldown, exhausted local
		// quota): report it through `started=false` so the dispatch loop moves
		// on to the next candidate instead of failing the whole request.
		return account, false, nil
	}
	account = markAccountDispatchStart(account, now)
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[index] = account
	s.mu.Lock()
	s.Config = cfg
	s.updateSnapshotBundleLocked()
	s.rebuildStaticJSONCachesLocked()
	s.mu.Unlock()
	return account, true, nil
}

// finishAccountDispatchSuccess records a completed dispatch: session metadata is
// merged without clobbering the configured workspace, runtime counters clear,
// and the probe pointer follows the account when it is (or becomes) the active
// one. A vanished account is not an error — the request itself already
// succeeded, there is simply nothing left to update.
func (s *ServerState) finishAccountDispatchSuccess(email string, session SessionInfo, now time.Time, makeActive bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cfg, _, _ := s.Snapshot()
	account, index, ok := cfg.FindAccount(email)
	if !ok {
		return nil
	}
	account = mergeSessionMetadataWithoutOverwritingConfig(account, session)
	account = markAccountDispatchSuccess(account, now)
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[index] = account
	wasActive := false
	if active, _, activeOK := cfg.ResolveActiveAccount(); activeOK {
		wasActive = canonicalEmailKey(active.Email) == canonicalEmailKey(account.Email)
	}
	if makeActive || wasActive {
		cfg.ActiveAccount = account.Email
		cfg.ProbeJSON = account.ProbeJSON
	}
	return s.saveAndApplyCommitted(cfg)
}

// finishAccountDispatchFailure records a failed dispatch so cooldowns and
// failure counters actually persist. A vanished account is likewise not an
// error; there is nothing left to cool down.
func (s *ServerState) finishAccountDispatchFailure(email string, now time.Time, dispatchErr error, retryable bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cfg, _, _ := s.Snapshot()
	account, index, ok := cfg.FindAccount(email)
	if !ok {
		return nil
	}
	account = markAccountDispatchFailure(account, now, dispatchErr, retryable)
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	cfg.Accounts[index] = account
	return s.saveAndApplyCommitted(cfg)
}

// commitAccountRefresh merges a refreshed account record into the live state,
// but only when the account kept the same identity since the dispatch snapshot
// was taken: an admin edit or another refresh that landed in between must not be
// silently clobbered by this commit. On divergence the live configuration is
// returned with an error so the caller falls back instead of persisting.
func (s *ServerState) commitAccountRefresh(cfg AppConfig, account NotionAccount, refreshedCfg AppConfig) (AppConfig, error) {
	started, _, ok := cfg.FindAccount(account.Email)
	if !ok {
		return cfg, fmt.Errorf("account %s not found in dispatch snapshot", account.Email)
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	live, _, _ := s.Snapshot()
	current, index, ok := live.FindAccount(account.Email)
	if !ok {
		return live, fmt.Errorf("account %s not found", account.Email)
	}
	if !accountIdentityUnchanged(started, current) {
		return live, fmt.Errorf("account %s changed concurrently; refresh not committed", account.Email)
	}
	refreshed, _, ok := refreshedCfg.FindAccount(account.Email)
	if !ok {
		return live, fmt.Errorf("account %s missing from refreshed configuration", account.Email)
	}
	live.Accounts = cloneAccounts(live.Accounts)
	live.Accounts[index] = refreshed
	wasActive := canonicalEmailKey(live.ActiveAccount) == canonicalEmailKey(refreshed.Email)
	makeActive := canonicalEmailKey(refreshedCfg.ActiveAccount) == canonicalEmailKey(refreshed.Email)
	if wasActive || makeActive {
		live.ActiveAccount = refreshed.Email
		live.ProbeJSON = refreshed.ProbeJSON
	}
	if err := s.saveAndApplyCommitted(live); err != nil {
		return live, err
	}
	return live, nil
}
