package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	testHookTryRefreshAccount func(context.Context, AppConfig, NotionAccount) (AppConfig, error)
	testHookSaveAndApply      func(*ServerState, AppConfig) error
)

func sessionRefreshNowISO() string {
	return time.Now().Format(time.RFC3339)
}

func isSessionRetryableError(err error) bool {
	if err == nil || isTrustRuleDeniedInferenceError(err) {
		return false
	}
	var apiErr *notionAPIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusTooManyRequests {
			return false
		}
		if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
			return true
		}
		message := strings.ToLower(strings.TrimSpace(apiErr.Message))
		// A stale client version is an explicit, actionable upstream signal at
		// any status. Bare "session"/"login" mentions are not: operational 5xx
		// bodies often contain them in unrelated text, and replaying an
		// inference request on that basis re-sends a turn that may already have
		// landed upstream.
		return strings.Contains(message, "client version") ||
			strings.Contains(message, "notion-client-version")
	}
	var loginErr *notionLoginAPIError
	if errors.As(err, &loginErr) {
		return loginErr.StatusCode == http.StatusUnauthorized || loginErr.StatusCode == http.StatusForbidden
	}
	// Free-form error text is deliberately not inspected: a client attachment
	// URL answering "403 Forbidden" used to read as an expired session and
	// refresh, re-login and cool down every account in the pool. Only typed
	// upstream statuses and the explicit internal marker count.
	return errors.Is(err, errSessionInvalid)
}

func loadSessionInfoForAccountRefresh(cfg AppConfig, account NotionAccount) (SessionInfo, error) {
	account = ensureAccountPaths(cfg, account)
	session, err := loadSessionInfo(account.ProbeJSON, firstNonEmpty(account.UserName, cfg.UserName), firstNonEmpty(account.SpaceName, cfg.SpaceName))
	if err == nil {
		if spaceID := strings.TrimSpace(account.SpaceID); spaceID != "" {
			if spaceID != session.SpaceID {
				session.SpaceViewID = ""
				session.SpaceName = ""
			}
			session.SpaceID = spaceID
			session.SpaceViewID = firstNonEmpty(account.SpaceViewID, session.SpaceViewID)
			session.SpaceName = firstNonEmpty(account.SpaceName, session.SpaceName)
		}
		return session, nil
	}
	storage, storageErr := readLoginStorageState(account.StorageStatePath)
	if storageErr != nil {
		return SessionInfo{}, err
	}
	if len(storage.Cookies) == 0 {
		return SessionInfo{}, err
	}
	userName := strings.TrimSpace(account.UserName)
	if userName == "" {
		userName = accountPathSlug(firstNonEmpty(storage.Email, account.Email))
	}
	spaceName := strings.TrimSpace(account.SpaceName)
	if spaceName == "" {
		spaceName = userName + "'s Space"
	}
	if strings.TrimSpace(account.UserID) == "" || strings.TrimSpace(account.SpaceID) == "" {
		return SessionInfo{}, err
	}
	return SessionInfo{
		ProbePath:     account.ProbeJSON,
		ClientVersion: firstNonEmpty(storage.ClientVersion, account.ClientVersion),
		UserID:        strings.TrimSpace(account.UserID),
		UserEmail:     firstNonEmpty(storage.Email, account.Email),
		UserName:      userName,
		SpaceID:       strings.TrimSpace(account.SpaceID),
		SpaceViewID:   strings.TrimSpace(account.SpaceViewID),
		SpaceName:     spaceName,
		Cookies:       storage.Cookies,
	}, nil
}

func buildRefreshedSession(ctx context.Context, cfg AppConfig, account NotionAccount, prior SessionInfo) (SessionInfo, error) {
	upstream := cfg.NotionUpstream()
	resolver := NewProxyResolver(cfg)
	session, err := newNotionLoginSession(helperTimeout(cfg), upstream, resolver, account.Email, cfg)
	if err != nil {
		return SessionInfo{}, err
	}
	restoreProbeCookies(session.Jar, upstream.HomeURL(), prior.Cookies)
	restoreProbeCookies(session.Jar, upstream.LoginURL(), prior.Cookies)

	bootstrap, err := fetchLoginBootstrap(ctx, session, upstream)
	if err != nil {
		return SessionInfo{}, err
	}
	clientVersion := firstNonEmpty(bootstrap.ClientVersion, prior.ClientVersion, account.ClientVersion)
	userID := firstNonEmpty(
		prior.UserID,
		account.UserID,
		probeCookieValue(probeCookiesFromJar(session.Jar, upstream.HomeURL()), "notion_user_id"),
		probeCookieValue(probeCookiesFromJar(session.Jar, upstream.LoginURL()), "notion_user_id"),
	)
	if userID == "" {
		return SessionInfo{}, fmt.Errorf("%w: notion_user_id missing during session refresh", errSessionInvalid)
	}
	spaces, err := getSpacesInitial(ctx, session, upstream, clientVersion, userID)
	if err != nil {
		return SessionInfo{}, err
	}
	cookies := probeCookiesFromJar(session.Jar, upstream.HomeURL())
	if len(cookies) == 0 {
		cookies = probeCookiesFromJar(session.Jar, upstream.LoginURL())
	}
	if len(cookies) == 0 {
		return SessionInfo{}, fmt.Errorf("%w: cookie jar empty after session refresh", errSessionInvalid)
	}
	userName := firstNonEmpty(spaces.UserName, prior.UserName, account.UserName)
	if userName == "" {
		userName = accountPathSlug(firstNonEmpty(spaces.Email, prior.UserEmail, account.Email))
	}
	spaceName := firstNonEmpty(prior.SpaceName, account.SpaceName)
	if spaceName == "" {
		spaceName = userName + "'s Space"
	}
	spaceID, spaceViewID := resolveRefreshedSpace(account, prior, spaces)
	return SessionInfo{
		ProbePath:     account.ProbeJSON,
		ClientVersion: clientVersion,
		UserID:        userID,
		UserEmail:     firstNonEmpty(spaces.Email, prior.UserEmail, account.Email),
		UserName:      userName,
		SpaceID:       spaceID,
		SpaceViewID:   spaceViewID,
		SpaceName:     spaceName,
		Cookies:       cookies,
	}, nil
}

// resolveRefreshedSpace picks the workspace a refreshed session should keep using.
//
// The workspace is an operator decision: Notion bills AI per workspace, so an
// account deliberately pointed at a paid space must stay there. getSpacesInitial
// reports whichever space happens to sit first in space_view_pointers, which for
// a multi-workspace account is usually the free personal one, so preferring the
// freshly reported space silently moved inference into a space with no AI credit
// on every refresh. Only fall back to discovery when nothing has been chosen yet.
//
// The view id travels with the space id it belongs to instead of being resolved
// independently: pairing a space with another space's view id would be worse
// than having no view id at all.
func resolveRefreshedSpace(account NotionAccount, prior SessionInfo, spaces loginSpaceBootstrap) (string, string) {
	for _, candidate := range []struct{ id, viewID string }{
		{account.SpaceID, account.SpaceViewID},
		{prior.SpaceID, prior.SpaceViewID},
		{spaces.SpaceID, spaces.SpaceViewID},
	} {
		if id := strings.TrimSpace(candidate.id); id != "" {
			return id, strings.TrimSpace(candidate.viewID)
		}
	}
	return "", ""
}

func writeSessionArtifacts(account NotionAccount, session SessionInfo) error {
	if strings.TrimSpace(account.StorageStatePath) != "" {
		if err := writeLoginStorageState(account.StorageStatePath, loginStorageState{
			Email:         session.UserEmail,
			ClientVersion: session.ClientVersion,
			Cookies:       session.Cookies,
		}); err != nil {
			return err
		}
	}
	if strings.TrimSpace(account.ProbeJSON) != "" {
		if err := writePrivatePrettyJSONFile(account.ProbeJSON, probePayload{
			Email:         session.UserEmail,
			UserID:        session.UserID,
			UserName:      session.UserName,
			SpaceID:       session.SpaceID,
			SpaceViewID:   session.SpaceViewID,
			SpaceName:     session.SpaceName,
			ClientVersion: session.ClientVersion,
			Cookies:       session.Cookies,
		}); err != nil {
			return err
		}
	}
	status := loginPendingState{
		LoginStatusFile: LoginStatusFile{
			Success:          true,
			Status:           "ready",
			Email:            session.UserEmail,
			ProfileDir:       account.ProfileDir,
			PendingStatePath: account.PendingStatePath,
			StorageStatePath: account.StorageStatePath,
			ProbePath:        account.ProbeJSON,
			UserID:           session.UserID,
			UserName:         session.UserName,
			SpaceID:          session.SpaceID,
			SpaceViewID:      session.SpaceViewID,
			SpaceName:        session.SpaceName,
			ClientVersion:    session.ClientVersion,
			Title:            "Notion",
			Message:          "session refreshed",
			UpdatedAt:        sessionRefreshNowISO(),
			LastLoginAt:      sessionRefreshNowISO(),
		},
	}
	return writeLoginPendingState(account.PendingStatePath, status)
}

func writeSessionRefreshFailure(account NotionAccount, err error) {
	if err == nil {
		return
	}
	status := loginPendingState{
		LoginStatusFile: LoginStatusFile{
			Success:          false,
			Status:           "expired",
			Email:            account.Email,
			ProfileDir:       account.ProfileDir,
			PendingStatePath: account.PendingStatePath,
			StorageStatePath: account.StorageStatePath,
			ProbePath:        account.ProbeJSON,
			UserID:           account.UserID,
			UserName:         account.UserName,
			SpaceID:          account.SpaceID,
			SpaceViewID:      account.SpaceViewID,
			SpaceName:        account.SpaceName,
			ClientVersion:    account.ClientVersion,
			Message:          strings.TrimSpace(err.Error()),
			Error:            strings.TrimSpace(err.Error()),
			UpdatedAt:        sessionRefreshNowISO(),
			LastLoginAt:      account.LastLoginAt,
		},
	}
	_ = writeLoginPendingState(account.PendingStatePath, status)
}

func (s *ServerState) setSessionRefreshRuntime(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastSessionRefresh = time.Now()
	if err != nil {
		s.LastSessionRefreshError = strings.TrimSpace(err.Error())
		return
	}
	s.LastSessionRefreshError = ""
}

func (s *ServerState) tryRefreshAccount(ctx context.Context, cfg AppConfig, account NotionAccount) (AppConfig, error) {
	if until := parseOptionalRFC3339(account.CredentialCooldownUntil); until.After(time.Now()) {
		return cfg, &notionAPIError{
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: until,
			Message:    "account is cooling down; session refresh deferred",
		}
	}
	account = ensureAccountPaths(cfg, account)
	prior, err := loadSessionInfoForAccountRefresh(cfg, account)
	if err != nil {
		account.Status = "expired"
		account.LastError = err.Error()
		cfg.UpsertAccount(account)
		writeSessionRefreshFailure(account, err)
		return cfg, err
	}
	refreshedSession, err := buildRefreshedSession(ctx, cfg, account, prior)
	if err != nil {
		account.Status = "expired"
		account.LastError = err.Error()
		cfg.UpsertAccount(account)
		writeSessionRefreshFailure(account, err)
		return cfg, err
	}
	if err := writeSessionArtifacts(account, refreshedSession); err != nil {
		account.Status = "failed"
		account.LastError = err.Error()
		cfg.UpsertAccount(account)
		writeSessionRefreshFailure(account, err)
		return cfg, err
	}
	account.UserID = refreshedSession.UserID
	account.UserName = refreshedSession.UserName
	account.SpaceID = refreshedSession.SpaceID
	account.SpaceViewID = refreshedSession.SpaceViewID
	account.SpaceName = firstNonEmpty(refreshedSession.SpaceName, account.SpaceName)
	account.ClientVersion = refreshedSession.ClientVersion
	account.Status = "ready"
	account.LastError = ""
	account.LastLoginAt = sessionRefreshNowISO()
	account.LastRefreshAt = sessionRefreshNowISO()
	account.CooldownUntil = ""
	account.ConsecutiveFailures = 0
	// A completed refresh means the account is healthy again, so the cleared
	// counters must survive persistence rather than being merged back.
	// The active account is deliberately left alone: a refresh triggered by
	// dispatch must not silently move the global active account. Only
	// RefreshSession, which refreshes the active account on purpose, may switch.
	cfg.UpsertAccountRuntimeState(account)
	return cfg, nil
}

func (s *ServerState) RefreshSession(ctx context.Context, reason string) error {
	// sessionRefreshMu only keeps two explicit/periodic refreshes from running
	// at once. refreshMu is taken solely for the commit, so dispatch
	// bookkeeping and admin saves are never blocked behind the network calls
	// a refresh (and its AutoSwitch fallbacks) make.
	s.sessionRefreshMu.Lock()
	defer s.sessionRefreshMu.Unlock()

	cfg, _, _ := s.Snapshot()
	refreshCfg := cfg.ResolveSessionRefresh()
	if !refreshCfg.Enabled {
		return fmt.Errorf("session refresh disabled")
	}
	account, _, ok := cfg.ResolveActiveWorkspace()
	if !ok {
		return fmt.Errorf("no active account configured for session refresh")
	}

	tryRefresh := s.tryRefreshAccount
	if testHookTryRefreshAccount != nil {
		tryRefresh = testHookTryRefreshAccount
	}
	// attempt refreshes one account without holding refreshMu and then commits
	// the outcome against the live state with an identity check. A failed
	// refresh still commits its status (expired/failed) so the admin view
	// reflects it; only a successful one makes the account active.
	attempt := func(candidate NotionAccount) (refreshErr error, saveErr error) {
		updatedCfg, err := tryRefresh(ctx, cfg, candidate)
		if _, commitErr := s.commitAccountRefreshResult(cfg, candidate, updatedCfg, err == nil); commitErr != nil && err == nil {
			return nil, commitErr
		}
		return err, nil
	}

	err, saveErr := attempt(account)
	if saveErr != nil {
		s.setSessionRefreshRuntime(saveErr)
		return saveErr
	}
	if err == nil {
		if s.DispatchProbeCache != nil {
			s.DispatchProbeCache.invalidateAll()
		}
		s.setSessionRefreshRuntime(nil)
		return nil
	}

	if !refreshCfg.AutoSwitch {
		s.setSessionRefreshRuntime(err)
		return fmt.Errorf("refresh active account %s failed (%s): %w", account.Email, reason, err)
	}

	lastErr := err
	for _, candidate := range cfg.Accounts {
		if getAccountEmailKey(candidate) == getAccountEmailKey(account) {
			continue
		}
		if !fileExists(ensureAccountPaths(cfg, candidate).ProbeJSON) {
			continue
		}
		nextErr, saveErr := attempt(candidate)
		if saveErr != nil {
			s.setSessionRefreshRuntime(saveErr)
			return saveErr
		}
		if nextErr != nil {
			lastErr = nextErr
			continue
		}
		if s.DispatchProbeCache != nil {
			s.DispatchProbeCache.invalidateAll()
		}
		s.setSessionRefreshRuntime(nil)
		return nil
	}

	s.setSessionRefreshRuntime(lastErr)
	return fmt.Errorf("session refresh failed after trying active account and fallbacks (%s): %w", reason, lastErr)
}

func (s *ServerState) StartSessionRefreshLoop(parent context.Context) {
	cfg, _, _ := s.Snapshot()
	refreshCfg := cfg.ResolveSessionRefresh()
	if !refreshCfg.Enabled {
		return
	}
	if refreshCfg.StartupCheck {
		go func() {
			ctx, cancel := context.WithTimeout(parent, helperTimeout(cfg))
			defer cancel()
			_ = s.RefreshSession(ctx, "startup_check")
		}()
	}
	go func() {
		for {
			currentCfg, _, _ := s.Snapshot()
			currentRefresh := currentCfg.ResolveSessionRefresh()
			wait := time.Duration(currentRefresh.IntervalSec) * time.Second
			if wait <= 0 {
				wait = 15 * time.Minute
			}
			timer := time.NewTimer(wait)
			select {
			case <-parent.Done():
				timer.Stop()
				return
			case <-timer.C:
				if !currentRefresh.Enabled {
					continue
				}
				ctx, cancel := context.WithTimeout(parent, helperTimeout(currentCfg))
				_ = s.RefreshSession(ctx, "periodic_check")
				cancel()
			}
		}
	}()
}
