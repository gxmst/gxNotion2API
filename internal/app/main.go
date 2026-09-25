package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type StoredResponse struct {
	Payload        map[string]any
	CreatedAt      time.Time
	ConversationID string
	ThreadID       string
	AccountEmail   string
}

type snapshotBundle struct {
	Config        AppConfig
	Session       SessionInfo
	ModelRegistry ModelRegistry
	DispatchOrder []NotionAccount
}

type ServerState struct {
	mu                         sync.RWMutex
	refreshMu                  sync.Mutex
	sessionRefreshMu           sync.Mutex
	modelPolicyMu              sync.Mutex
	modelPolicyStateMu         sync.Mutex
	modelPolicyRestorePoints   map[string]*modelPolicyRestorePoint
	Config                     AppConfig
	Session                    SessionInfo
	Client                     *NotionAIClient
	Store                      *SQLiteStore
	ModelRegistry              ModelRegistry
	ResponseStore              *responseStore
	Conversations              *ConversationStore
	AdminTokens                map[string]time.Time
	AdminLoginAttempts         map[string]AdminLoginAttempt
	DispatchProbeCache         *probeCache
	LastSessionRefresh         time.Time
	LastSessionRefreshError    string
	responseStoreCleanupCancel context.CancelFunc
	sqliteWriter               *SQLiteWriter
	snap                       atomic.Pointer[snapshotBundle]
	slots                      atomic.Pointer[map[string]*accountSlot]
	cachedHealthzStaticJSON    atomic.Pointer[[]byte]
	cachedModelsListJSON       atomic.Pointer[[]byte]
	cachedModelByIDJSON        atomic.Pointer[map[string][]byte]
	aiUsageMu                  sync.Mutex
	aiUsageCache               map[string]workspaceAIUsageReport
	aiUsageLastForced          time.Time
}

type accountDispatchState struct {
	MaxConcurrency int
	InFlight       int
}

type accountSlot struct {
	max      atomic.Int32
	inflight atomic.Int32
}

// healthzStaticPayload is the cached part of /healthz that any caller may see.
//
// /healthz is answered before authOK so that a liveness probe needs no
// credentials. That makes it the wrong place for account identity: user_email,
// space_id and active_account used to live here, which handed the Notion account
// address to anyone who could reach the port. serveHealthz now attaches those
// three only for a caller holding the admin session or the API key, and because
// they are caller-dependent they cannot be part of this shared cache.
type healthzStaticPayload struct {
	OK                   bool   `json:"ok"`
	DefaultModel         string `json:"default_model"`
	ModelCount           int    `json:"model_count"`
	SessionRefreshEnable bool   `json:"session_refresh_enabled"`
}

type publicModelPayload struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int    `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	Group       string `json:"group"`
	Beta        bool   `json:"beta"`
	NotionModel string `json:"notion_model"`
}

type publicModelsListPayload struct {
	Object string               `json:"object"`
	Data   []publicModelPayload `json:"data"`
}

type App struct {
	State                            *ServerState
	runPromptOverride                func(*http.Request, PromptRunRequest) (InferenceResult, error)
	runPromptStreamOverride          func(*http.Request, PromptRunRequest, func(string) error) (InferenceResult, error)
	runPromptStreamSinkOverride      func(*http.Request, PromptRunRequest, InferenceStreamSink) (InferenceResult, error)
	runPromptWithSessionOverride     func(context.Context, AppConfig, SessionInfo, PromptRunRequest, func(string) error) (InferenceResult, error)
	runPromptWithSessionSinkOverride func(context.Context, AppConfig, SessionInfo, PromptRunRequest, InferenceStreamSink) (InferenceResult, error)
	accountProtocolProbeOverride     func(context.Context, AppConfig, SessionInfo) error
	workspaceAIUsageFetchOverride    func(context.Context, AppConfig, NotionAccount) (workspaceAIUsage, error)
}

const (
	ephemeralConversationCleanupInterval  = time.Minute
	ephemeralConversationCleanupBatchSize = 24
	sillyTavernQuietConversationTTL       = 10 * time.Minute
	defaultConfigEphemeralConversationTTL = 2 * time.Minute
	// Conversations idle this long are swept along with their upstream thread.
	// Keep ordinary conversations until explicitly deleted. Operators can opt
	// into idle cleanup; ephemeral conversations retain their separate policy.
	defaultConversationIdleTTLHours = 0
	corsAllowOrigin                 = "*"
	corsAllowHeaders                = "Authorization, Content-Type, X-Admin-Token"
	corsAllowMethods                = "GET, POST, PUT, DELETE, OPTIONS"
)

var errRequestTooLarge = errors.New("request body too large")
var responseStorePruneTotalMetric = expvar.NewMap("notion2api_response_store_prune_total")
var testHookResponseStoreCleanupInterval time.Duration

type continuationTarget struct {
	Conversation ConversationEntry
	Session      *conversationContinuationState
}

type panicSafeResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *panicSafeResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *panicSafeResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *panicSafeResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func normalizeAccountMaxConcurrency(raw int) int {
	if raw <= 0 {
		return 1
	}
	return raw
}

// The caller holds s.mu exclusively, including against slot acquisition.
func (s *ServerState) rebuildAccountSlotsLocked() {
	if s == nil {
		return
	}
	var previous map[string]*accountSlot
	if loaded := s.slots.Load(); loaded != nil {
		previous = *loaded
	}
	next := make(map[string]*accountSlot, len(s.Config.Accounts))
	for _, account := range s.Config.Accounts {
		credentialKey := credentialSlotKey(account.Email)
		credential := previous[credentialKey]
		if credential == nil {
			credential = &accountSlot{}
		}
		credential.max.Store(int32(normalizeAccountMaxConcurrency(s.Config.Dispatch.AccountMaxConcurrency)))
		next[credentialKey] = credential
		for _, candidate := range accountWorkspaceCandidates(account) {
			key := dispatchWorkspaceKey(candidate)
			if key == "" {
				continue
			}
			maxConcurrency := int32(normalizeAccountMaxConcurrency(candidate.MaxConcurrency))
			if existing := previous[key]; existing != nil {
				// Lowering the limit blocks new work until real in-flight requests drain.
				existing.max.Store(maxConcurrency)
				next[key] = existing
				continue
			}
			slot := &accountSlot{}
			slot.max.Store(maxConcurrency)
			next[key] = slot
		}
	}
	// Slots for keys that vanish while requests are still in flight on them
	// must survive the rebuild: the late release has to decrement the same
	// object it acquired, and a workspace that is added back later inherits
	// its real in-flight count instead of a fresh zero. Idle leftovers are
	// dropped so the map does not grow with removed workspaces.
	for key, slot := range previous {
		if _, ok := next[key]; ok {
			continue
		}
		if slot.inflight.Load() > 0 {
			next[key] = slot
		}
	}
	s.slots.Store(&next)
	syncDispatchSlotInflightFromSlots(next)
}

func (s *ServerState) loadAccountSlots() map[string]*accountSlot {
	if s == nil {
		return nil
	}
	loaded := s.slots.Load()
	if loaded == nil {
		return nil
	}
	return *loaded
}

// workspaceSlotKey resolves an account/workspace pair from a config snapshot.
// Acquisition uses the live config under s.mu together with the slot update.
func (s *ServerState) workspaceSlotKey(email string, workspaceID string) (string, bool) {
	cfg, _, _ := s.Snapshot()
	if account, _, ok := cfg.FindAccountWorkspace(email, workspaceID); ok {
		return dispatchWorkspaceKey(account), true
	}
	return "", false
}

// dispatchSlotLease is the exact set of slots one acquisition incremented.
// Releasing through the lease decrements those same slot objects, so a config
// change between acquire and release (a workspace renamed, a blank workspace
// id now resolving elsewhere) can neither leak the credential slot nor release
// somebody else's. release is idempotent and safe to defer.
type dispatchSlotLease struct {
	state *ServerState
	keys  []string
	slots []*accountSlot
	once  sync.Once
}

func (l *dispatchSlotLease) release() {
	if l == nil || l.state == nil {
		return
	}
	l.once.Do(func() {
		l.state.mu.RLock()
		defer l.state.mu.RUnlock()
		for i := len(l.slots) - 1; i >= 0; i-- {
			releaseAccountSlot(l.keys[i], l.slots[i])
		}
	})
}

func (s *ServerState) TryAcquireWorkspaceDispatchSlot(email string, workspaceID string) bool {
	_, ok := s.acquireWorkspaceDispatchSlot(email, workspaceID, false)
	return ok
}

func (s *ServerState) acquireWorkspaceDispatchSlot(email string, workspaceID string, forRetry bool) (*dispatchSlotLease, bool) {
	if s == nil {
		return nil, false
	}
	// Keep lookup and acquisition in the same read-side critical section so a
	// rebuild cannot retire an idle slot before its in-flight count is raised.
	s.mu.RLock()
	defer s.mu.RUnlock()
	account, _, ok := s.Config.FindAccountWorkspace(email, workspaceID)
	if !ok {
		return nil, false
	}
	if forRetry {
		// The first attempt already consumed the logical request's quota.
		// Recheck mutable admission rules without charging it a second time.
		eligible, _ := accountWorkspaceEligibility(account)
		now := time.Now()
		if account.Disabled || !eligible || accountCooldownActive(account, now) || parseOptionalRFC3339(account.CredentialCooldownUntil).After(now) {
			return nil, false
		}
	}
	lease := &dispatchSlotLease{state: s}
	for _, key := range []string{credentialSlotKey(email), dispatchWorkspaceKey(account)} {
		key = strings.TrimSpace(key)
		slot := s.loadAccountSlots()[key]
		if key == "" || slot == nil || !tryAcquireAccountSlot(key, slot) {
			for i := len(lease.slots) - 1; i >= 0; i-- {
				releaseAccountSlot(lease.keys[i], lease.slots[i])
			}
			return nil, false
		}
		lease.keys = append(lease.keys, key)
		lease.slots = append(lease.slots, slot)
	}
	return lease, true
}

func tryAcquireAccountSlot(key string, slot *accountSlot) bool {
	for {
		maxConcurrency := slot.max.Load()
		if maxConcurrency <= 0 {
			maxConcurrency = 1
		}
		inflight := slot.inflight.Load()
		if inflight >= maxConcurrency {
			return false
		}
		if slot.inflight.CompareAndSwap(inflight, inflight+1) {
			setDispatchSlotInflight(key, int(inflight+1))
			return true
		}
	}
}

func releaseAccountSlot(key string, slot *accountSlot) bool {
	if slot == nil {
		return false
	}
	for {
		inflight := slot.inflight.Load()
		if inflight <= 0 {
			return false
		}
		if slot.inflight.CompareAndSwap(inflight, inflight-1) {
			setDispatchSlotInflight(key, int(inflight-1))
			return true
		}
	}
}

// ReleaseWorkspaceDispatchSlot releases by account/workspace lookup. The
// dispatcher itself releases through dispatchSlotLease; this remains for
// callers that hold no lease.
func (s *ServerState) ReleaseWorkspaceDispatchSlot(email string, workspaceID string) {
	if s == nil || canonicalEmailKey(email) == "" {
		return
	}
	key := canonicalEmailKey(email)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if workspaceID = strings.TrimSpace(workspaceID); workspaceID == "" {
		if account, _, ok := s.Config.FindAccountWorkspace(email, ""); ok {
			workspaceID = accountWorkspaceID(account)
		}
	}
	if workspaceID != "" {
		key += "\x00" + workspaceID
	}
	if s.releaseDispatchSlotKeyLocked(key) {
		s.releaseDispatchSlotKeyLocked(credentialSlotKey(email))
	}
}

func (s *ServerState) releaseDispatchSlotKeyLocked(key string) bool {
	return releaseAccountSlot(key, s.loadAccountSlots()[key])
}

func (s *ServerState) RemainingWorkspaceDispatchSlots(email string, workspaceID string) int {
	key, ok := s.workspaceSlotKey(email, workspaceID)
	if !ok {
		return 0
	}
	return s.remainingDispatchSlotKey(key)
}

func (s *ServerState) remainingDispatchSlotKey(key string) int {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	slot := s.loadAccountSlots()[key]
	if slot == nil {
		return 0
	}
	maxConcurrency := slot.max.Load()
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	inflight := slot.inflight.Load()
	remaining := int(maxConcurrency - inflight)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (s *ServerState) AvailableDispatchCapacityKeys(keys []string) int {
	slots := s.loadAccountSlots()
	if len(slots) == 0 {
		return 0
	}
	total := 0
	seen := map[string]struct{}{}
	for _, key := range keys {
		emailKey := strings.TrimSpace(key)
		if emailKey == "" {
			continue
		}
		if _, exists := seen[emailKey]; exists {
			continue
		}
		seen[emailKey] = struct{}{}
		slot := slots[emailKey]
		if slot == nil {
			continue
		}
		maxConcurrency := slot.max.Load()
		if maxConcurrency <= 0 {
			maxConcurrency = 1
		}
		inflight := slot.inflight.Load()
		remaining := int(maxConcurrency - inflight)
		if remaining > 0 {
			total += remaining
		}
	}
	return total
}

func (s *ServerState) AccountDispatchSnapshot() map[string]accountDispatchState {
	slots := s.loadAccountSlots()
	out := make(map[string]accountDispatchState, len(slots))
	for key, slot := range slots {
		if slot == nil {
			continue
		}
		maxConcurrency := int(slot.max.Load())
		if maxConcurrency <= 0 {
			maxConcurrency = 1
		}
		inflight := int(slot.inflight.Load())
		if inflight < 0 {
			inflight = 0
		}
		out[key] = accountDispatchState{
			MaxConcurrency: maxConcurrency,
			InFlight:       inflight,
		}
	}
	return out
}

func maxFloat(a float64, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func formatTimeOrEmpty(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func validateConfiguredAPIKey(cfg AppConfig) error {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return fmt.Errorf("api key is required")
	}
	// The shipped example configs use change-me placeholders; running with them
	// publishes a well-known credential.
	if isPlaceholderSecret(cfg.APIKey) {
		return fmt.Errorf("api key is still the example placeholder; set a real value")
	}
	if cfg.Admin.Enabled && isPlaceholderSecret(cfg.Admin.Password) {
		return fmt.Errorf("admin password is still the example placeholder; set a real value")
	}
	return nil
}

func isPlaceholderSecret(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "change-me")
}

func newServerState(cfg AppConfig) (*ServerState, error) {
	cfg = normalizeConfig(cfg)
	if err := validateConfiguredAPIKey(cfg); err != nil {
		return nil, err
	}
	store, err := openSQLiteStore(cfg)
	if err != nil {
		return nil, err
	}
	state := &ServerState{
		Conversations:      newConversationStore(),
		AdminTokens:        map[string]time.Time{},
		AdminLoginAttempts: map[string]AdminLoginAttempt{},
		DispatchProbeCache: newProbeCache(),
		Store:              store,
	}
	state.ResponseStore = newResponseStore(time.Duration(maxInt(cfg.Responses.StoreTTLSeconds, 1)) * time.Second)
	persistedAccountsLoaded := false
	if store != nil {
		accounts, activeAccount, activeWorkspaceID, ok, loadErr := store.LoadAccountsWithWorkspace()
		if loadErr != nil {
			_ = store.Close()
			return nil, loadErr
		}
		if ok {
			cfg.Accounts = accounts
			cfg.ActiveAccount = strings.TrimSpace(activeAccount)
			cfg.ActiveWorkspaceID = strings.TrimSpace(activeWorkspaceID)
			if cfg.ActiveAccount != "" {
				if account, _, found := cfg.FindAccount(cfg.ActiveAccount); found {
					cfg.ProbeJSON = account.ProbeJSON
				}
			} else if len(cfg.Accounts) > 0 {
				cfg.ProbeJSON = ""
			}
			persistedAccountsLoaded = true
		}
	}
	if err := state.ApplyConfig(cfg); err != nil {
		if store != nil {
			_ = store.Close()
		}
		return nil, err
	}
	if store != nil {
		if responsesPersistenceEnabled(state.Config) {
			responses, loadErr := store.LoadResponses(time.Duration(state.Config.Responses.StoreTTLSeconds) * time.Second)
			if loadErr != nil {
				_ = store.Close()
				return nil, loadErr
			}
			if state.ResponseStore == nil {
				state.ResponseStore = newResponseStore(time.Duration(maxInt(state.Config.Responses.StoreTTLSeconds, 1)) * time.Second)
			}
			state.ResponseStore.replaceAll(responses)
		}
		if conversationSnapshotsPersistenceEnabled(state.Config) {
			conversations, loadErr := store.LoadConversations()
			if loadErr != nil {
				_ = store.Close()
				return nil, loadErr
			}
			state.Conversations = newConversationStoreFromEntries(conversations)
			// A turn that was mid-flight when the process died would look
			// "running" forever. Fail those entries on startup, keeping the
			// thread and account pointers so the next request can resume from
			// the same thread instead of orphaning it.
			for _, entry := range conversations {
				if !conversationStatusBusy(entry.Status) {
					continue
				}
				state.Conversations.Fail(entry.ID, errors.New("turn was interrupted by a restart"))
				state.persistConversationSnapshot(entry.ID)
			}
		}
		if !persistedAccountsLoaded && (len(state.Config.Accounts) > 0 || strings.TrimSpace(state.Config.ActiveAccount) != "") {
			if saveErr := store.SaveAccounts(state.Config); saveErr != nil {
				_ = store.Close()
				return nil, saveErr
			}
		}
		state.sqliteWriter = newSQLiteWriter(store, time.Duration(maxInt(state.Config.Responses.StoreTTLSeconds, 1))*time.Second)
	}
	state.startResponseStoreCleanupLoop(context.Background())
	return state, nil
}

func (s *ServerState) ApplyConfig(cfg AppConfig) error {
	cfg = normalizeConfig(cfg)
	if err := validateConfiguredAPIKey(cfg); err != nil {
		return err
	}
	registry := buildModelRegistry(cfg)
	probePath, userName, spaceName, activeEmail := cfg.ResolveSessionTarget()
	session := SessionInfo{}
	var client *NotionAIClient
	if strings.TrimSpace(probePath) != "" {
		loadedSession, err := loadSessionInfo(probePath, userName, spaceName)
		if account, _, found := cfg.ResolveActiveWorkspace(); found {
			loadedSession, err = loadSessionInfoForAccountRefresh(cfg, account)
		}
		if err != nil {
			log.Printf("[startup] session bootstrap skipped for probe=%s active=%s: %v", probePath, activeEmail, err)
		} else {
			session = loadedSession
			client = newNotionAIClient(loadedSession, cfg, activeEmail)
			if activeEmail != "" {
				cfg.ProbeJSON = loadedSession.ProbePath
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Config = cfg
	s.Session = session
	s.ModelRegistry = registry
	s.Client = client
	if s.sqliteWriter != nil {
		s.sqliteWriter.SetTTL(time.Duration(maxInt(cfg.Responses.StoreTTLSeconds, 1)) * time.Second)
	}
	s.rebuildAccountSlotsLocked()
	s.updateSnapshotBundleLocked()
	s.rebuildStaticJSONCachesLocked()
	// Push the ephemeral TTL override into the live store here rather than from
	// conversations(), which sits on the streaming hot path.
	if s.Conversations != nil {
		s.Conversations.SetEphemeralTTL(configuredEphemeralTTLOverride(cfg))
	}
	return nil
}

func (s *ServerState) Snapshot() (AppConfig, SessionInfo, ModelRegistry) {
	if s == nil {
		return AppConfig{}, SessionInfo{}, ModelRegistry{}
	}
	if snap := s.snap.Load(); snap != nil {
		return snap.Config, snap.Session, snap.ModelRegistry
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Config, s.Session, s.ModelRegistry
}

func (s *ServerState) updateSnapshotBundleLocked() {
	if s == nil {
		return
	}
	now := time.Now()
	dispatchOrder := buildDispatchCandidateOrder(s.Config, now)
	bundle := &snapshotBundle{
		Config:        s.Config,
		Session:       s.Session,
		ModelRegistry: s.ModelRegistry,
		DispatchOrder: dispatchOrder,
	}
	s.snap.Store(bundle)
}

// SaveAndApply normalizes and persists cfg, then swaps the live state to it.
// refreshMu is the same lock the dispatch state helpers hold across their
// read-modify-write cycles, so an admin save and a concurrent dispatch cannot
// interleave their account bookkeeping and lose an update.
func (s *ServerState) SaveAndApply(cfg AppConfig) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.saveAndApplyLocked(cfg)
}

// Mutate runs a read-modify-write of the live config under refreshMu. Handlers
// that read a snapshot, edit it and then call SaveAndApply would otherwise
// overwrite cooldowns, counters or capabilities that dispatch committed in
// between. fn must not block on the network; it receives a private copy whose
// Accounts slice is already cloned.
func (s *ServerState) Mutate(fn func(cfg *AppConfig) error) (AppConfig, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cfg, _, _ := s.Snapshot()
	cfg.Accounts = cloneAccounts(cfg.Accounts)
	if err := fn(&cfg); err != nil {
		return AppConfig{}, err
	}
	if err := s.saveAndApplyLocked(cfg); err != nil {
		return AppConfig{}, err
	}
	committed, _, _ := s.Snapshot()
	return committed, nil
}

// saveAndApplyLocked is SaveAndApply's body for callers that already hold
// refreshMu (the dispatch state helpers in account_pool.go).
func (s *ServerState) saveAndApplyLocked(cfg AppConfig) error {
	cfg = normalizeConfig(cfg)
	if err := validateConfiguredAPIKey(cfg); err != nil {
		return err
	}
	current, _, _ := s.Snapshot()
	if strings.TrimSpace(cfg.ConfigPath) != "" {
		if strings.TrimSpace(current.ConfigPath) != strings.TrimSpace(cfg.ConfigPath) || !persistedConfigEqual(current, cfg) {
			if err := saveConfigFile(cfg); err != nil {
				return err
			}
		}
	}
	if err := s.ApplyConfig(cfg); err != nil {
		return err
	}
	if s.Store != nil {
		if err := s.Store.SaveAccounts(cfg); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.ResponseStore == nil {
		s.ResponseStore = newResponseStore(time.Duration(maxInt(cfg.Responses.StoreTTLSeconds, 1)) * time.Second)
	} else {
		s.ResponseStore.setTTL(time.Duration(maxInt(cfg.Responses.StoreTTLSeconds, 1)) * time.Second)
	}
	s.updateSnapshotBundleLocked()
	s.mu.Unlock()
	if canonicalEmailKey(current.ActiveAccount) != canonicalEmailKey(cfg.ActiveAccount) && s.DispatchProbeCache != nil {
		s.DispatchProbeCache.invalidateAll()
	}
	return nil
}

func (s *ServerState) conversationPersistenceStore() *SQLiteStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Store == nil || !sqliteBackedConversationStorageAvailable(s.Config) {
		return nil
	}
	return s.Store
}

func (s *ServerState) saveResponse(responseID string, payload map[string]any, conversationID string, threadID string) {
	s.saveResponseWithAccount(responseID, payload, conversationID, threadID, "")
}

func (s *ServerState) saveResponseWithAccount(responseID string, payload map[string]any, conversationID string, threadID string, accountEmail string) {
	now := time.Now().UTC()
	s.mu.Lock()
	store := s.ResponseStore
	if store == nil {
		store = newResponseStore(time.Duration(maxInt(s.Config.Responses.StoreTTLSeconds, 1)) * time.Second)
		s.ResponseStore = store
	}
	store.save(responseID, StoredResponse{
		Payload:        payload,
		CreatedAt:      now,
		ConversationID: strings.TrimSpace(conversationID),
		ThreadID:       strings.TrimSpace(threadID),
		AccountEmail:   strings.TrimSpace(accountEmail),
	}, now)
	sqliteWriter := s.sqliteWriter
	sqliteStore := s.Store
	ttl := time.Duration(maxInt(s.Config.Responses.StoreTTLSeconds, 1)) * time.Second
	storeEnabled := sqliteStore != nil && responsesPersistenceEnabled(s.Config)
	s.mu.Unlock()
	if storeEnabled {
		if sqliteWriter != nil {
			sqliteWriter.EnqueueSaveResponse(responseID, payload, now, conversationID, threadID, accountEmail)
			return
		}
		if err := sqliteStore.SaveResponse(responseID, payload, now, conversationID, threadID, accountEmail); err != nil {
			log.Printf("[sqlite] save response %s failed: %v", responseID, err)
			return
		}
		if err := sqliteStore.DeleteExpiredResponses(ttl); err != nil {
			log.Printf("[sqlite] cleanup responses failed: %v", err)
		}
	}
}

func (s *ServerState) getResponse(responseID string) (map[string]any, bool) {
	record, ok := s.getStoredResponse(responseID)
	if !ok {
		return nil, false
	}
	return record.Payload, true
}

func (s *ServerState) getStoredResponse(responseID string) (StoredResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ResponseStore == nil {
		return StoredResponse{}, false
	}
	return s.ResponseStore.get(responseID, time.Now().UTC())
}

func (s *ServerState) loadConversationContinuationStateByConversationID(conversationID string) (*conversationContinuationState, error) {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := continuationSessionsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled || strings.TrimSpace(conversationID) == "" {
		return nil, nil
	}
	session, ok, err := store.LoadConversationSessionByConversationID(conversationID)
	if err != nil || !ok {
		return nil, err
	}
	updatedConfigIDs, err := store.LoadConversationSessionStepIDs(session.ID)
	if err != nil {
		return nil, err
	}
	return &conversationContinuationState{
		Session:          session,
		UpdatedConfigIDs: updatedConfigIDs,
	}, nil
}

func (s *ServerState) loadConversationContinuationStateByThreadID(threadID string) (*conversationContinuationState, error) {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := continuationSessionsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled || strings.TrimSpace(threadID) == "" {
		return nil, nil
	}
	session, ok, err := store.LoadConversationSessionByThreadID(threadID)
	if err != nil || !ok {
		return nil, err
	}
	updatedConfigIDs, err := store.LoadConversationSessionStepIDs(session.ID)
	if err != nil {
		return nil, err
	}
	return &conversationContinuationState{
		Session:          session,
		UpdatedConfigIDs: updatedConfigIDs,
	}, nil
}

func (s *ServerState) loadConversationContinuationStateByFingerprint(fingerprint string) (*conversationContinuationState, error) {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := continuationSessionsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled || strings.TrimSpace(fingerprint) == "" {
		return nil, nil
	}
	session, ok, err := store.LoadConversationSessionByFingerprint(fingerprint)
	if err != nil || !ok {
		return nil, err
	}
	updatedConfigIDs, err := store.LoadConversationSessionStepIDs(session.ID)
	if err != nil {
		return nil, err
	}
	return &conversationContinuationState{
		Session:          session,
		UpdatedConfigIDs: updatedConfigIDs,
	}, nil
}

// deleteConversationSessionByConversationOrThread drops the continuation
// sessions of a conversation or thread. It reports the store error instead of
// swallowing it: callers must not remove the conversation row while a session
// that could revive it is still there.
func (s *ServerState) deleteConversationSessionByConversationOrThread(conversationID string, threadID string) error {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := continuationSessionsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled {
		return nil
	}
	if err := store.DeleteConversationSessionByConversationOrThread(conversationID, threadID); err != nil {
		log.Printf("[sqlite] delete continuation session conversation=%s thread=%s failed: %v", conversationID, threadID, err)
		return err
	}
	return nil
}

func (s *ServerState) invalidateConversationSession(sessionID string, status string) {
	store := s.conversationPersistenceStore()
	s.mu.RLock()
	enabled := continuationSessionsPersistenceEnabled(s.Config)
	s.mu.RUnlock()
	if store == nil || !enabled || strings.TrimSpace(sessionID) == "" {
		return
	}
	if err := store.MarkConversationSessionStatus(sessionID, status); err != nil {
		log.Printf("[sqlite] update continuation session status session=%s status=%s failed: %v", sessionID, status, err)
	}
}

func (s *ServerState) Close() error {
	s.mu.RLock()
	store := s.Store
	cancelCleanup := s.responseStoreCleanupCancel
	sqliteWriter := s.sqliteWriter
	s.mu.RUnlock()
	if cancelCleanup != nil {
		cancelCleanup()
	}
	// Streaming turns persist at most once a second; write out whatever the
	// throttle held back before the store goes away.
	s.flushConversationSnapshots()
	if sqliteWriter != nil {
		sqliteWriter.Close()
	}
	if store == nil {
		return nil
	}
	return store.Close()
}

func (s *ServerState) startResponseStoreCleanupLoop(parent context.Context) {
	if s == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	interval := responseStoreCleanupInterval
	if testHookResponseStoreCleanupInterval > 0 {
		interval = testHookResponseStoreCleanupInterval
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.responseStoreCleanupCancel = cancel
	s.mu.Unlock()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runResponseStoreCleanupOnce(time.Now().UTC())
			}
		}
	}()
}

func (s *ServerState) runResponseStoreCleanupOnce(now time.Time) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	if s.ResponseStore == nil {
		s.mu.Unlock()
		return 0
	}
	removed := s.ResponseStore.pruneExpired(now)
	s.mu.Unlock()
	if removed > 0 {
		responseStorePruneTotalMetric.Add("expired_entries", int64(removed))
	}
	if links := s.pruneOrphanedResponseLinks(); links > 0 {
		responseStorePruneTotalMetric.Add("orphaned_links", int64(links))
	}
	return removed
}

func buildPublicModelPayload(entry ModelDefinition) publicModelPayload {
	return publicModelPayload{
		ID:          entry.ID,
		Object:      "model",
		Created:     0,
		OwnedBy:     "notion2api",
		Name:        entry.Name,
		Family:      entry.Family,
		Group:       entry.Group,
		Beta:        entry.Beta,
		NotionModel: entry.NotionModel,
	}
}

func buildPublicModelsListPayload(registry ModelRegistry) publicModelsListPayload {
	items := make([]publicModelPayload, 0, len(registry.Entries))
	for _, entry := range registry.Entries {
		if !entry.Enabled {
			continue
		}
		items = append(items, buildPublicModelPayload(entry))
	}
	return publicModelsListPayload{
		Object: "list",
		Data:   items,
	}
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneBytesMap(src map[string][]byte) map[string][]byte {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string][]byte, len(src))
	for key, value := range src {
		dst[key] = cloneBytes(value)
	}
	return dst
}

func (s *ServerState) rebuildStaticJSONCachesLocked() {
	healthPayload := healthzStaticPayload{
		OK:                   true,
		DefaultModel:         s.Config.DefaultPublicModel(),
		ModelCount:           len(s.ModelRegistry.Entries),
		SessionRefreshEnable: s.Config.ResolveSessionRefresh().Enabled,
	}
	healthBody, err := json.Marshal(healthPayload)
	if err == nil {
		healthBodyCopy := cloneBytes(healthBody)
		s.cachedHealthzStaticJSON.Store(&healthBodyCopy)
	} else {
		s.cachedHealthzStaticJSON.Store(nil)
	}

	publicRegistry := publicWorkspaceModelRegistry(s.Config, s.ModelRegistry)
	modelsPayload := buildPublicModelsListPayload(publicRegistry)
	modelsBody, err := json.Marshal(modelsPayload)
	if err == nil {
		modelsBodyCopy := cloneBytes(modelsBody)
		s.cachedModelsListJSON.Store(&modelsBodyCopy)
	} else {
		s.cachedModelsListJSON.Store(nil)
	}

	modelByID := make(map[string][]byte, len(s.ModelRegistry.Entries))
	for _, entry := range publicRegistry.Entries {
		if !entry.Enabled {
			continue
		}
		body, marshalErr := json.Marshal(buildPublicModelPayload(entry))
		if marshalErr != nil {
			continue
		}
		modelByID[normalizeLookupKey(entry.ID)] = cloneBytes(body)
	}
	modelByIDCopy := cloneBytesMap(modelByID)
	s.cachedModelByIDJSON.Store(&modelByIDCopy)
}

func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("X-Notion2API", "1")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func appendHealthzRuntimeFields(body []byte, sessionReady bool, lastRefresh time.Time, lastRefreshError string) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		trimmed = []byte(`{"ok":true}`)
	}
	trimmed = bytes.TrimSuffix(trimmed, []byte("}"))
	tail := map[string]any{
		"session_ready":              sessionReady,
		"last_session_refresh":       formatTimeOrEmpty(lastRefresh),
		"last_session_refresh_error": lastRefreshError,
	}
	tailBody, err := json.Marshal(tail)
	if err != nil {
		return body
	}
	tailBody = bytes.TrimPrefix(tailBody, []byte("{"))
	out := make([]byte, 0, len(trimmed)+1+len(tailBody))
	out = append(out, trimmed...)
	if len(trimmed) > 1 {
		out = append(out, ',')
	}
	out = append(out, tailBody...)
	return out
}

// applyCORSHeadersForPath applies the wildcard CORS policy everywhere except
// the admin surface: admin auth is cookie-based and same-origin by design, and
// advertising wildcard CORS there would let any web page fire admin requests
// from an operator's browser. Applied once at the ServeHTTP entry so every
// writer below inherits the same decision.
func applyCORSHeadersForPath(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, "/admin") {
		return
	}
	applyCORSHeaders(w)
}

func applyCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Notion2API", "1")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeOpenAIError(w http.ResponseWriter, status int, message string, errorType string, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"param":   nil,
			"code":    code,
		},
	})
}

func writeInvalidBodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errRequestTooLarge) {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body exceeds configured limit", "invalid_request_error", "request_too_large")
		return
	}
	writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", nilString())
}

func nilString() string {
	return ""
}

func decodeBodyWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64) (map[string]any, error) {
	raw, err := decodeBodyRawWithLimit(w, r, maxBytes)
	if err != nil {
		return nil, err
	}
	return decodeBodyMapFromRaw(raw)
}

func decodeBodyMapFromRaw(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, nil
}

func decodeBodyRawWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	if maxBytes > 0 && w != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errRequestTooLarge
		}
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []byte("{}"), nil
	}
	var raw json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errRequestTooLarge
		}
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	normalized := bytes.TrimSpace(raw)
	if len(normalized) == 0 {
		return []byte("{}"), nil
	}
	return normalized, nil
}

func (a *App) decodeBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	raw, err := a.decodeBodyRaw(w, r)
	if err != nil {
		return nil, err
	}
	return decodeBodyMapFromRaw(raw)
}

func (a *App) decodeBodyRaw(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	maxBytes := int64(0)
	if a != nil && a.State != nil {
		cfg, _, _ := a.State.Snapshot()
		maxBytes = cfg.Limits.MaxRequestBodyBytes
	}
	return decodeBodyRawWithLimit(w, r, maxBytes)
}

func decodeTypedBodyFromRaw[T any](raw []byte) (T, error) {
	var typed T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&typed); err != nil {
		return typed, fmt.Errorf("invalid json: %w", err)
	}
	return typed, nil
}

func (a *App) authOK(w http.ResponseWriter, r *http.Request) bool {
	cfg, _, _ := a.State.Snapshot()
	expected := strings.TrimSpace(cfg.APIKey)
	if expected == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server api key is not configured", "server_error", "api_key_required")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("Authorization"))), []byte("Bearer "+expected)) == 1 {
		return true
	}
	writeOpenAIError(w, http.StatusUnauthorized, "invalid api key", "authentication_error", "invalid_api_key")
	return false
}

// healthzCallerIsOperator reports whether the caller has proved it is the
// operator, and may therefore see the account identity fields. Either the admin
// session or the API key counts; both are operator-held secrets.
//
// This deliberately does not write an error response: /healthz answers either
// way, just with less detail.
func (a *App) healthzCallerIsOperator(r *http.Request) bool {
	if a.adminTokenValid(adminTokenFromRequest(r)) {
		return true
	}
	cfg, _, _ := a.State.Snapshot()
	expected := strings.TrimSpace(cfg.APIKey)
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("Authorization"))
	return subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+expected)) == 1
}

func (a *App) serveHealthz(w http.ResponseWriter, r *http.Request) {
	a.State.mu.RLock()
	sessionReady := a.State.Client != nil
	lastRefresh := a.State.LastSessionRefresh
	lastRefreshError := a.State.LastSessionRefreshError
	cached := a.State.cachedHealthzStaticJSON.Load()
	a.State.mu.RUnlock()

	operator := a.healthzCallerIsOperator(r)
	if !operator && lastRefreshError != "" {
		// The raw error can carry upstream URLs, response bodies and file paths.
		lastRefreshError = "session refresh failed"
	}

	if cached != nil && !operator {
		body := appendHealthzRuntimeFields(*cached, sessionReady, lastRefresh, lastRefreshError)
		writeJSONBytes(w, http.StatusOK, body)
		return
	}

	cfg, session, registry := a.State.Snapshot()
	payload := map[string]any{
		"ok":                         true,
		"default_model":              cfg.DefaultPublicModel(),
		"model_count":                len(registry.Entries),
		"session_ready":              sessionReady,
		"session_refresh_enabled":    cfg.ResolveSessionRefresh().Enabled,
		"last_session_refresh":       formatTimeOrEmpty(lastRefresh),
		"last_session_refresh_error": lastRefreshError,
	}
	if operator {
		payload["user_email"] = session.UserEmail
		payload["space_id"] = session.SpaceID
		payload["active_account"] = cfg.ActiveAccount
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *App) serveModels(w http.ResponseWriter) {
	cached := a.State.cachedModelsListJSON.Load()
	if cached != nil {
		writeJSONBytes(w, http.StatusOK, *cached)
		return
	}
	cfg, _, registry := a.State.Snapshot()
	writeJSON(w, http.StatusOK, buildPublicModelsListPayload(publicWorkspaceModelRegistry(cfg, registry)))
}

func (a *App) serveModelByID(w http.ResponseWriter, path string) {
	cfg, _, registry := a.State.Snapshot()
	modelID := strings.TrimSpace(strings.TrimPrefix(path, "/v1/models/"))
	entry, err := registry.Resolve(modelID, cfg.DefaultPublicModel())
	available := false
	for _, public := range publicWorkspaceModelRegistry(cfg, registry).Entries {
		if public.ID == entry.ID {
			available = true
			break
		}
	}
	if err != nil || !available {
		writeOpenAIError(w, http.StatusNotFound, "model not found", "invalid_request_error", "model_not_found")
		return
	}
	if cached := a.State.cachedModelByIDJSON.Load(); cached != nil {
		if body, ok := (*cached)[normalizeLookupKey(entry.ID)]; ok && len(body) > 0 {
			writeJSONBytes(w, http.StatusOK, body)
			return
		}
	}
	writeJSON(w, http.StatusOK, buildPublicModelPayload(entry))
}

func (a *App) serveResponseByID(w http.ResponseWriter, path string) {
	responseID := strings.TrimSpace(strings.TrimPrefix(path, "/v1/responses/"))
	payload, ok := a.State.getResponse(responseID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "response not found", "invalid_request_error", "response_not_found")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func requestedModel(payload map[string]any, fallback string) string {
	modelID := strings.TrimSpace(stringValue(payload["model"]))
	if modelID == "" {
		return fallback
	}
	return modelID
}

func parseBoolField(value any) (bool, bool) {
	switch raw := value.(type) {
	case bool:
		return raw, true
	case string:
		clean := strings.TrimSpace(strings.ToLower(raw))
		switch clean {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	return false, false
}

func requestedWebSearch(payload map[string]any, fallback bool) bool {
	if value, ok := parseBoolField(payload["use_web_search"]); ok {
		return value
	}
	if meta := mapValue(payload["metadata"]); meta != nil {
		if value, ok := parseBoolField(meta["use_web_search"]); ok {
			return value
		}
		if value, ok := parseBoolField(meta["notion_use_web_search"]); ok {
			return value
		}
	}
	for _, rawTool := range sliceValue(payload["tools"]) {
		tool := mapValue(rawTool)
		toolType := strings.TrimSpace(stringValue(tool["type"]))
		if strings.Contains(toolType, "web_search") {
			return true
		}
	}
	return fallback
}

func firstRequestValue(r *http.Request, keys ...string) string {
	if r != nil {
		for _, key := range keys {
			if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
				return value
			}
		}
	}
	return ""
}

// Without an explicit client identity, connection traits must also scope the
// history fallback. Otherwise ordinary clients all share an empty client key.
func requestClientContinuationScope(r *http.Request, profile string, session string, transport string, model string, account string, workspace string) string {
	parts := []string{
		"profile=" + strings.TrimSpace(profile),
		"session=" + strings.TrimSpace(session),
		"transport=" + strings.TrimSpace(transport),
		"model=" + strings.TrimSpace(model),
		"account=" + canonicalEmailKey(account),
	}
	if workspace = strings.TrimSpace(workspace); workspace != "" {
		parts = append(parts, "workspace="+workspace)
	}
	if r != nil {
		clientID := firstRequestValue(r, "X-Client-ID", "X-Session-ID", "OpenAI-Organization")
		parts = append(parts, "client="+clientID)
		if clientID == "" {
			parts = append(parts, requestClientConnectionScope(r))
		}
	}
	return strings.Join(parts, "\n")
}

// Explicit client identities can recover through the history fallback after a
// connection change; fingerprints still include the current connection traits.
func requestClientFingerprintScope(r *http.Request, profile string, session string, transport string, model string, account string, workspace string) string {
	scope := requestClientContinuationScope(r, profile, session, transport, model, account, workspace)
	if r == nil || firstRequestValue(r, "X-Client-ID", "X-Session-ID", "OpenAI-Organization") == "" {
		return scope
	}
	return scope + "\n" + requestClientConnectionScope(r)
}

func requestClientConnectionScope(r *http.Request) string {
	peer := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	return "ua=" + strings.TrimSpace(r.Header.Get("User-Agent")) + "\nremote=" + peer
}

func requestedConversationID(r *http.Request, payload map[string]any) string {
	if conversationID := firstRequestValue(r, "X-Conversation-ID", "X-Notion-Conversation-ID"); conversationID != "" {
		return conversationID
	}
	if conversationID := strings.TrimSpace(stringValue(payload["conversation_id"])); conversationID != "" {
		return conversationID
	}
	if conversationID := strings.TrimSpace(stringValue(payload["conversation"])); conversationID != "" {
		return conversationID
	}
	if meta := mapValue(payload["metadata"]); meta != nil {
		for _, key := range []string{"conversation_id", "notion_conversation_id"} {
			if conversationID := strings.TrimSpace(stringValue(meta[key])); conversationID != "" {
				return conversationID
			}
		}
	}
	return ""
}

func requestedThreadID(r *http.Request, payload map[string]any) string {
	if threadID := firstRequestValue(r, "X-Thread-ID", "X-Notion-Thread-ID"); threadID != "" {
		return threadID
	}
	for _, key := range []string{"thread_id", "thread", "notion_thread_id"} {
		if threadID := strings.TrimSpace(stringValue(payload[key])); threadID != "" {
			return threadID
		}
	}
	if meta := mapValue(payload["metadata"]); meta != nil {
		for _, key := range []string{"thread_id", "notion_thread_id"} {
			if threadID := strings.TrimSpace(stringValue(meta[key])); threadID != "" {
				return threadID
			}
		}
	}
	return ""
}

func requestedAccountEmail(r *http.Request, payload map[string]any) string {
	if accountEmail := firstRequestValue(r, "X-Account-Email", "X-Notion-Account-Email"); accountEmail != "" {
		return accountEmail
	}
	for _, key := range []string{"account_email", "notion_account_email"} {
		if accountEmail := strings.TrimSpace(stringValue(payload[key])); accountEmail != "" {
			return accountEmail
		}
	}
	if meta := mapValue(payload["metadata"]); meta != nil {
		for _, key := range []string{"account_email", "notion_account_email"} {
			if accountEmail := strings.TrimSpace(stringValue(meta[key])); accountEmail != "" {
				return accountEmail
			}
		}
	}
	return ""
}

func preferActiveAccountForRequest(cfg AppConfig, request *PromptRunRequest) {
	if request == nil || strings.TrimSpace(request.PinnedAccountEmail) != "" {
		return
	}
	if account, _, ok := cfg.ResolveActiveAccount(); ok {
		if email := strings.TrimSpace(account.Email); email != "" {
			request.PinnedAccountEmail = email
			request.AllowPinnedAccountFallback = true
		}
	}
}

func resolveRequestPromptForContinuation(normalized NormalizedInput) string {
	return firstNonEmpty(strings.TrimSpace(normalized.DisplayPrompt), strings.TrimSpace(normalized.Prompt))
}

func forceFreshThreadPerRequest(cfg AppConfig) bool {
	return cfg.Features.ForceFreshThreadPerRequest
}

func latestReplayPrompt(latestPrompt string, attachments []InputAttachment, fallback string) string {
	clean := strings.TrimSpace(latestPrompt)
	if clean == "" && len(attachments) > 0 {
		clean = defaultUploadedAttachmentPrompt
	}
	return firstNonEmpty(clean, strings.TrimSpace(fallback))
}

func buildFreshThreadReplayPromptFromConversation(conversation ConversationEntry, latestPrompt string, attachments []InputAttachment, fallback string) string {
	segments := conversationMessageSegments(&conversation)
	if len(segments) == 0 {
		return latestReplayPrompt(latestPrompt, attachments, fallback)
	}
	cleanLatest := latestReplayPrompt(latestPrompt, attachments, "")
	if cleanLatest != "" {
		last := segments[len(segments)-1]
		if last.Role != "user" || collapseWhitespace(last.Text) != collapseWhitespace(cleanLatest) {
			segments = append(segments, conversationPromptSegment{
				Role: "user",
				Text: cleanLatest,
			})
		}
	}
	if prompt := buildConversationTranscriptPrompt(segments); strings.TrimSpace(prompt) != "" {
		return prompt
	}
	return latestReplayPrompt(latestPrompt, attachments, fallback)
}

func buildFreshThreadReplayPromptFromStoredResponse(previousResponsePrompt string, latestPrompt string, attachments []InputAttachment, fallback string) string {
	cleanPrevious := strings.TrimSpace(previousResponsePrompt)
	if cleanPrevious == "" {
		return latestReplayPrompt(latestPrompt, attachments, fallback)
	}
	parts := []string{
		"Continue the conversation using the transcript below. Reply as the assistant to the final [user] message only. Do not mention or repeat the role labels in your reply.",
		cleanPrevious,
	}
	if cleanLatest := latestReplayPrompt(latestPrompt, attachments, ""); cleanLatest != "" {
		parts = append(parts, formatPromptSection("user", cleanLatest))
	}
	if prompt := strings.TrimSpace(strings.Join(parts, "\n\n")); prompt != "" {
		return prompt
	}
	return latestReplayPrompt(latestPrompt, attachments, fallback)
}

func setConversationIDHeader(w http.ResponseWriter, conversationID string) {
	if w == nil {
		return
	}
	if conversationID = strings.TrimSpace(conversationID); conversationID != "" {
		w.Header().Set("X-Conversation-ID", conversationID)
	}
}

func setThreadIDHeader(w http.ResponseWriter, threadID string) {
	if w == nil {
		return
	}
	if threadID = strings.TrimSpace(threadID); threadID != "" {
		w.Header().Set("X-Notion-Thread-ID", threadID)
	}
}

func attachConversationResponseMetadata(payload map[string]any, conversationID string, threadID string) {
	if payload == nil {
		return
	}
	conversationID = strings.TrimSpace(conversationID)
	threadID = strings.TrimSpace(threadID)
	if conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	if threadID != "" {
		payload["thread_id"] = threadID
	}
	if trace := mapValue(payload["notion_trace"]); trace != nil {
		if conversationID != "" {
			trace["conversation_id"] = conversationID
		}
		if threadID != "" {
			trace["thread_id"] = threadID
		}
	}
}

// latestUserSegmentText returns the trailing user message of a normalized
// request history.
func latestUserSegmentText(segments []conversationPromptSegment) string {
	normalized := exactConversationSegments(segments)
	for i := len(normalized) - 1; i >= 0; i-- {
		if normalized[i].Role == "user" {
			return strings.TrimSpace(normalized[i].Text)
		}
	}
	return ""
}

func conversationFinalUserTurn(conversation ConversationEntry) (ConversationMessage, bool) {
	for i := len(conversation.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(conversation.Messages[i].Role), "user") {
			return conversation.Messages[i], true
		}
	}
	return ConversationMessage{}, false
}

// requestMatchesConversationFinalTurn reports whether the request re-sends the
// exact final user turn that produced the conversation's latest completed
// answer. Message counts alone cannot tell a repeated request from an edited
// final message ("explain A" -> "explain B"), and only the former may replay
// the cached answer.
func requestMatchesConversationFinalTurn(request PromptRunRequest, segments []conversationPromptSegment, conversation ConversationEntry) bool {
	if !conversationHistoryCompatible(conversation, segments, true) {
		return false
	}
	if conversation.RequestFingerprint != "" {
		request.HistorySegments = segments
		if conversation.RequestFingerprint != conversationRequestFingerprint(request) {
			return false
		}
	} else if (request.PublicModel != "" && conversation.Model != "" && request.PublicModel != conversation.Model) ||
		request.UseWebSearch != conversation.UseWebSearch || strings.TrimSpace(request.HiddenPrompt) != strings.TrimSpace(conversation.HiddenPrompt) {
		return false
	}
	turn, ok := conversationFinalUserTurn(conversation)
	if !ok {
		return false
	}
	if latestUserSegmentText(segments) != strings.TrimSpace(turn.Content) {
		return false
	}
	requestAttachments := summarizeInputAttachments(request.Attachments)
	if len(requestAttachments) != len(turn.Attachments) {
		return false
	}
	for i := range requestAttachments {
		// URLs and paths can change in place, and legacy records have no digest.
		// Only inline bytes with a persisted content digest can be replayed.
		if requestAttachments[i].ContentSHA256 == "" || requestAttachments[i].ContentSHA256 != turn.Attachments[i].ContentSHA256 {
			return false
		}
		if strings.TrimSpace(requestAttachments[i].Name) != strings.TrimSpace(turn.Attachments[i].Name) ||
			strings.TrimSpace(requestAttachments[i].ContentType) != strings.TrimSpace(turn.Attachments[i].ContentType) {
			return false
		}
	}
	return true
}

func isConversationTurnConflict(err error) bool {
	return errors.Is(err, errConversationInProgress) || errors.Is(err, errConversationDeleting)
}

func (a *App) resolveContinuationConversation(r *http.Request, payload map[string]any, previousResponseID string, fingerprint string, segments []conversationPromptSegment) (continuationTarget, bool) {
	explicitConversationID := requestedConversationID(r, payload)
	explicitThreadID := requestedThreadID(r, payload)
	return a.resolveContinuationConversationWithExplicit(previousResponseID, fingerprint, "", segments, explicitConversationID, explicitThreadID)
}

// resolveContinuationAccount decides which account may serve a continuation.
// A conversation that knows its owner keeps it: another account must not take
// over its thread and its billing. When the owner is unknown (an old entry, or
// a bare thread id), an explicit request account is honoured, and in
// multi-account mode its absence is an error — guessing would silently spend
// the wrong workspace's AI credit.
func resolveContinuationAccount(cfg AppConfig, threadID string, requestedAccount string, entry ConversationEntry) (string, error) {
	owner := strings.TrimSpace(entry.AccountEmail)
	requested := strings.TrimSpace(requestedAccount)
	threadID = strings.TrimSpace(threadID)
	if owner != "" {
		if _, _, ok := cfg.FindAccountWorkspace(owner, entry.SpaceID); !ok && strings.TrimSpace(entry.SpaceID) != "" {
			return "", errConversationWorkspaceMismatch
		}
		if requested != "" && canonicalEmailKey(owner) != canonicalEmailKey(requested) {
			return "", fmt.Errorf("conversation %s belongs to %s, not %s", threadID, owner, requested)
		}
		return owner, nil
	}
	if requested != "" {
		return requested, nil
	}
	if len(cfg.Accounts) > 1 {
		return "", fmt.Errorf("conversation %s has no recorded account; pass an explicit account to continue it", threadID)
	}
	if len(cfg.Accounts) == 1 {
		return cfg.Accounts[0].Email, nil
	}
	return strings.TrimSpace(cfg.ActiveAccount), nil
}

// replayResultFromConversation builds the cached-answer replay for a repeated
// final turn from a completed conversation. It returns nil when the
// conversation holds no completed assistant answer, leaving the turn to run
// upstream as usual.
//
// An operator edit is served as-is, deliberately. RequestFingerprint describes
// the request, not the answer, so editing the stored answer leaves the
// fingerprint matching and a repeat of the same request replays the edited
// text rather than the model's original output. That is the intended reading of
// an edit: the stored transcript is the record, and a correction to it should
// not be quietly reverted to text the model produced. Note the asymmetry with
// editing the final *user* turn, which does invalidate the replay, because
// requestMatchesConversationFinalTurn compares that text against the request.
func replayResultFromConversation(conversation ConversationEntry) *InferenceResult {
	if !strings.EqualFold(strings.TrimSpace(conversation.Status), "completed") {
		return nil
	}
	for i := len(conversation.Messages) - 1; i >= 0; i-- {
		message := &conversation.Messages[i]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(message.Status), "completed") {
			continue
		}
		if strings.TrimSpace(message.Content) == "" {
			continue
		}
		return &InferenceResult{
			Text:         message.Content,
			ThreadID:     strings.TrimSpace(conversation.ThreadID),
			AccountEmail: strings.TrimSpace(conversation.AccountEmail),
			SpaceID:      conversation.SpaceID,
			SpaceViewID:  conversation.SpaceViewID,
			// A replayed answer that was cut short upstream must not come back
			// looking like a clean stop.
			Truncated:    message.Truncated,
			cachedReplay: true,
		}
	}
	return nil
}

// resolveContinuationConversationWithExplicit resolves the conversation a
// request continues. `fingerprint` must be the already-computed scoped
// fingerprint the caller will also persist on the session; recomputing it here
// from a hidden prompt would produce a different key and silently kill the
// fingerprint hit path.
func (a *App) resolveContinuationConversationWithExplicit(previousResponseID string, fingerprint string, clientScope string, segments []conversationPromptSegment, explicitConversationID string, explicitThreadID string) (continuationTarget, bool) {
	rawCount := sessionRawMessageCount(segments)
	validateState := func(state *conversationContinuationState) bool {
		if state == nil {
			return true
		}
		if rawCount > 1 && shouldInvalidateConversationSession(state.Session, rawCount) {
			return false
		}
		return true
	}
	if explicitConversationID != "" {
		if entry, ok := a.State.conversations().Get(explicitConversationID); ok && strings.TrimSpace(entry.ThreadID) != "" {
			if !conversationHistoryCompatible(entry, segments, true) {
				return continuationTarget{}, false
			}
			state, err := a.State.loadConversationContinuationStateByConversationID(entry.ID)
			if err == nil && !validateState(state) {
				return continuationTarget{Conversation: entry}, true
			}
			if err == nil {
				return continuationTargetWithSession(entry, state), true
			}
			return continuationTarget{Conversation: entry}, true
		}
		if state, err := a.State.loadConversationContinuationStateByConversationID(explicitConversationID); err == nil && state != nil {
			if a.conversationDeleted(explicitConversationID) {
				// The conversation row is gone and only a session row survived
				// a partial delete. Reviving it would run the next turn against
				// a thread the conversation no longer owns.
				return continuationTarget{}, false
			}
			if !validateState(state) {
				entry := ConversationEntry{
					ID:           strings.TrimSpace(state.Session.ConversationID),
					ThreadID:     strings.TrimSpace(state.Session.ThreadID),
					AccountEmail: strings.TrimSpace(state.Session.AccountEmail),
				}
				return continuationTarget{Conversation: entry}, true
			}
			entry := ConversationEntry{
				ID:           strings.TrimSpace(state.Session.ConversationID),
				ThreadID:     strings.TrimSpace(state.Session.ThreadID),
				AccountEmail: strings.TrimSpace(state.Session.AccountEmail),
			}
			return continuationTargetWithSession(entry, state), true
		}
		return continuationTarget{}, false
	}
	if previousResponseID != "" {
		if stored, ok := a.State.getContinuationResponse(previousResponseID); ok {
			if stored.ConversationID != "" {
				if entry, found := a.State.conversations().Get(stored.ConversationID); found && strings.TrimSpace(entry.ThreadID) != "" {
					if strings.TrimSpace(entry.AccountEmail) == "" {
						entry.AccountEmail = strings.TrimSpace(stored.AccountEmail)
					}
					state, err := a.State.loadConversationContinuationStateByConversationID(entry.ID)
					if err == nil && !validateState(state) {
						return continuationTarget{}, false
					}
					if err == nil {
						return continuationTargetWithSession(entry, state), true
					}
					return continuationTarget{Conversation: entry}, true
				}
			}
			if stored.ThreadID != "" {
				if stored.ConversationID != "" && a.conversationDeleted(stored.ConversationID) {
					// The response link outlived the conversation it belonged
					// to; continuing its thread would resurrect deleted state.
					return continuationTarget{}, false
				}
				target := continuationTarget{Conversation: ConversationEntry{
					ThreadID:     stored.ThreadID,
					AccountEmail: strings.TrimSpace(stored.AccountEmail),
				}}
				if state, err := a.State.loadConversationContinuationStateByThreadID(stored.ThreadID); err == nil {
					if !validateState(state) {
						return continuationTarget{}, false
					}
					target = continuationTargetWithSession(target.Conversation, state)
				}
				// The link may carry only a thread id, leaving the session as the
				// only thing that names the conversation. Re-check on the
				// resolved target so that path cannot revive deleted state.
				if a.conversationDeleted(target.Conversation.ID) {
					return continuationTarget{}, false
				}
				return target, true
			}
		}
	}
	if explicitThreadID != "" {
		if entry, ok := a.State.conversations().FindByThreadID(explicitThreadID); ok {
			if !conversationHistoryCompatible(entry, segments, true) {
				return continuationTarget{}, false
			}
			state, err := a.State.loadConversationContinuationStateByThreadID(explicitThreadID)
			if err == nil && !validateState(state) {
				return continuationTarget{}, false
			}
			if err == nil {
				return continuationTargetWithSession(entry, state), true
			}
			return continuationTarget{Conversation: entry}, true
		}
		target := continuationTarget{Conversation: ConversationEntry{
			ThreadID: explicitThreadID,
		}}
		if state, err := a.State.loadConversationContinuationStateByThreadID(explicitThreadID); err == nil {
			if !validateState(state) {
				return continuationTarget{}, false
			}
			target = continuationTargetWithSession(target.Conversation, state)
		}
		// No live conversation owns this thread. If the session that matched it
		// points at a deleted conversation, adopting its thread would revive the
		// very state the delete removed.
		if a.conversationDeleted(target.Conversation.ID) {
			return continuationTarget{}, false
		}
		return target, true
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint != "" {
		if state, err := a.State.loadConversationContinuationStateByFingerprint(fingerprint); err == nil && state != nil {
			if a.conversationDeleted(state.Session.ConversationID) {
				// The fingerprint still matches a session whose conversation
				// was deleted; start a fresh thread instead of reviving it.
				return continuationTarget{}, false
			}
			if !validateState(state) {
				return continuationTarget{}, false
			}
			if rawCount >= state.Session.RawMessageCount && strings.TrimSpace(state.Session.ThreadID) != "" {
				entry := ConversationEntry{
					ID:           strings.TrimSpace(state.Session.ConversationID),
					ThreadID:     strings.TrimSpace(state.Session.ThreadID),
					AccountEmail: strings.TrimSpace(state.Session.AccountEmail),
				}
				if existing, ok := a.State.conversations().Get(entry.ID); ok {
					entry = existing
				}
				if conversationHistoryCompatible(entry, segments, false) {
					return continuationTargetWithSession(entry, state), true
				}
			}
		}
	}
	history := exactConversationSegments(segments)
	if len(history) > 0 && history[len(history)-1].Role == "user" {
		history = history[:len(history)-1]
	}
	if len(history) > 1 {
		if entry, ok := a.State.conversations().FindContinuationBySegments(history, clientScope); ok {
			state, err := a.State.loadConversationContinuationStateByConversationID(entry.ID)
			if err == nil && !validateState(state) {
				return continuationTarget{}, false
			}
			if err == nil {
				return continuationTargetWithSession(entry, state), true
			}
			return continuationTarget{Conversation: entry}, true
		}
	}
	return continuationTarget{}, false
}

// startConversationTurn starts (or continues) the local conversation record for
// a turn. A busy or deleting conversation is reported as an error instead of
// being silently re-created: the request already carries the upstream thread of
// the existing record, so a fresh local entry would run a second turn against
// the same thread in parallel.
func (a *App) startConversationTurn(existingConversationID string, preferredConversationID string, source string, transport string, displayPrompt string, request PromptRunRequest) (string, error) {
	if existingConversationID != "" && (strings.TrimSpace(request.UpstreamThreadID) != "" || request.ForceLocalConversationContinue) {
		conversationID, err := a.continueConversation(existingConversationID, source, transport, displayPrompt, request)
		if err == nil {
			return conversationID, nil
		}
		if isConversationTurnConflict(err) {
			return "", err
		}
	}
	return a.beginConversation(preferredConversationID, source, transport, displayPrompt, request), nil
}

func (a *App) markEphemeralConversationRequest(request *PromptRunRequest) {
	if request == nil {
		return
	}
	if a.markConfigEphemeralConversationRequest(request) {
		return
	}
	if request.ClientProfile != sillyTavernClientProfile {
		return
	}
	if request.EphemeralConversation {
		return
	}
	switch request.ClientMode {
	case sillyTavernModeQuiet:
		request.EphemeralConversation = true
		request.EphemeralReason = "sillytavern_quiet"
	case sillyTavernModeImpersona:
		request.EphemeralConversation = true
		request.EphemeralReason = "sillytavern_impersonate"
	default:
		return
	}
}

// markConfigEphemeralConversationRequest applies features.ephemeral_all_conversations,
// which makes every request's upstream thread disposable regardless of client profile.
// It reports whether the request was marked here.
func (a *App) markConfigEphemeralConversationRequest(request *PromptRunRequest) bool {
	if a == nil || a.State == nil || request == nil {
		return false
	}
	cfg, _, _ := a.State.Snapshot()
	if !cfg.Features.EphemeralAllConversations {
		return false
	}
	request.EphemeralConversation = true
	request.EphemeralReason = firstNonEmpty(strings.TrimSpace(request.EphemeralReason), "config_ephemeral_all")
	ttl := configuredEphemeralTTL(cfg)
	deadline := time.Now().UTC().Add(ttl)
	if request.EphemeralDeleteAfter.IsZero() || request.EphemeralDeleteAfter.After(deadline) {
		request.EphemeralDeleteAfter = deadline
	}
	return true
}

// configuredEphemeralTTL is the lifetime granted to a conversation that
// ephemeral_all_conversations marked. It is only consulted on that path.
func configuredEphemeralTTL(cfg AppConfig) time.Duration {
	if seconds := cfg.Features.EphemeralTTLSeconds; seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultConfigEphemeralConversationTTL
}

// configuredEphemeralTTLOverride is what the conversation store applies to every
// ephemeral conversation. Zero means "do not override", which leaves paths that
// pick their own lifetime (SillyTavern quiet/impersonate side-requests, whose
// built-in TTL is sillyTavernQuietConversationTTL) alone. Returning the
// ephemeral_all default here instead would silently shorten those.
func configuredEphemeralTTLOverride(cfg AppConfig) time.Duration {
	if seconds := cfg.Features.EphemeralTTLSeconds; seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

// configuredConversationIdleTTL is how long a conversation may sit without a new
// turn before it is swept. Absent config means defaultConversationIdleTTLHours;
// an explicit 0 disables idle sweeping.
func configuredConversationIdleTTL(cfg AppConfig) time.Duration {
	hours := defaultConversationIdleTTLHours
	if cfg.Features.ConversationIdleTTLHours != nil {
		hours = *cfg.Features.ConversationIdleTTLHours
	}
	if hours <= 0 {
		return 0
	}
	return time.Duration(hours) * time.Hour
}

func (a *App) cleanupExpiredEphemeralConversations() {
	if a == nil || a.State == nil {
		return
	}
	now := time.Now().UTC()
	expired := a.State.conversations().ListExpiredEphemeral(now, ephemeralConversationCleanupBatchSize)
	// Conversations evicted from memory still sit in SQLite; sweep those too.
	for _, entry := range a.persistedSweepCandidates(now, true, ephemeralConversationCleanupBatchSize-len(expired)) {
		if entry.AutoDeleteAt != nil && !entry.AutoDeleteAt.After(now) {
			expired = append(expired, entry)
		}
	}
	for _, entry := range expired {
		if err := a.deleteConversation(entry.ID); err != nil {
			a.deferConversationSweep(entry.ID)
			log.Printf("[cleanup] delete expired ephemeral conversation=%s thread=%s reason=%s failed: %v", entry.ID, entry.ThreadID, entry.EphemeralReason, err)
			continue
		}
		log.Printf("[cleanup] deleted expired ephemeral conversation=%s thread=%s reason=%s", entry.ID, entry.ThreadID, entry.EphemeralReason)
	}
	a.cleanupIdleConversations()
}

// cleanupIdleConversations sweeps conversations that have gone untouched for
// features.conversation_idle_ttl_hours, deleting the upstream thread with them.
// Threads are deliberately kept until then so that resuming a conversation still
// reuses its thread and hits the upstream prompt cache.
func (a *App) cleanupIdleConversations() {
	if a == nil || a.State == nil {
		return
	}
	cfg, _, _ := a.State.Snapshot()
	idleTTL := configuredConversationIdleTTL(cfg)
	if idleTTL <= 0 {
		return
	}
	now := time.Now().UTC()
	idle := a.State.conversations().ListIdleConversations(now, idleTTL, ephemeralConversationCleanupBatchSize)
	idle = append(idle, a.persistedSweepCandidates(now.Add(-idleTTL), false, ephemeralConversationCleanupBatchSize-len(idle))...)
	for _, entry := range idle {
		if err := a.deleteConversation(entry.ID); err != nil {
			a.deferConversationSweep(entry.ID)
			log.Printf("[cleanup] delete idle conversation=%s thread=%s idle_ttl=%s failed: %v", entry.ID, entry.ThreadID, idleTTL, err)
			continue
		}
		log.Printf("[cleanup] deleted idle conversation=%s thread=%s idle_ttl=%s", entry.ID, entry.ThreadID, idleTTL)
	}
}

// persistedSweepCandidates lists conversations that exist only in SQLite
// (evicted from the in-memory store) and were last updated before the cutoff.
// Resident conversations are skipped: the in-memory sweep owns them.
func (a *App) persistedSweepCandidates(updatedBefore time.Time, ephemeral bool, limit int) []ConversationEntry {
	if limit <= 0 {
		return nil
	}
	store := a.State.conversationPersistenceStore()
	if store == nil {
		return nil
	}
	// Over-fetch: resident and deferred rows are filtered out below.
	rows, err := store.ListConversationsForSweep(updatedBefore, ephemeral, limit*4)
	if err != nil {
		log.Printf("[cleanup] list persisted conversations failed: %v", err)
		return nil
	}
	conversations := a.State.conversations()
	now := time.Now().UTC()
	out := make([]ConversationEntry, 0, limit)
	for _, entry := range rows {
		if conversations.Contains(entry.ID) || conversations.SweepDeferred(entry.ID, now) {
			continue
		}
		out = append(out, entry)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// deferConversationSweep keeps a conversation whose cleanup failed out of the
// following batches for a while, so a handful of undeletable entries cannot
// occupy every batch forever.
func (a *App) deferConversationSweep(conversationID string) {
	a.State.conversations().DeferSweep(conversationID, time.Now().UTC().Add(conversationSweepRetryDelay))
}

func (a *App) StartEphemeralConversationCleanupLoop(parent context.Context) {
	if a == nil || a.State == nil {
		return
	}
	go func() {
		a.cleanupExpiredEphemeralConversations()
		timer := time.NewTimer(ephemeralConversationCleanupInterval)
		defer timer.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case <-timer.C:
				a.cleanupExpiredEphemeralConversations()
				timer.Reset(ephemeralConversationCleanupInterval)
			}
		}
	}()
}

func includeUsageInStream(payload map[string]any) bool {
	options := mapValue(payload["stream_options"])
	includeUsage, _ := options["include_usage"].(bool)
	return includeUsage
}

func decodeChatCompletionsRequestBodyFromRaw(raw []byte) (chatCompletionsRequestBody, map[string]any, error) {
	typed, err := decodeTypedBodyFromRaw[chatCompletionsRequestBody](raw)
	if err == nil {
		return normalizeTypedChatCompletionsRequestBody(typed), nil, nil
	}
	payload, mapErr := decodeBodyMapFromRaw(raw)
	if mapErr != nil {
		return chatCompletionsRequestBody{}, nil, mapErr
	}
	return extractChatCompletionsRequestBody(payload), payload, nil
}

func decodeResponsesRequestBodyFromRaw(raw []byte) (responsesRequestBody, map[string]any, error) {
	typed, err := decodeTypedBodyFromRaw[responsesRequestBody](raw)
	if err == nil {
		return normalizeTypedResponsesRequestBody(typed), nil, nil
	}
	payload, mapErr := decodeBodyMapFromRaw(raw)
	if mapErr != nil {
		return responsesRequestBody{}, nil, mapErr
	}
	return extractResponsesRequestBody(payload), payload, nil
}

func maybeSillyTavernByTypedMessages(rawMessages any) bool {
	items := sliceValue(rawMessages)
	if len(items) == 0 {
		return false
	}
	systemPrompts := make([]string, 0, len(items))
	for _, raw := range items {
		msg := mapValue(raw)
		if msg == nil {
			continue
		}
		if strings.TrimSpace(strings.ToLower(stringValue(msg["role"]))) != "system" {
			continue
		}
		text := collapseWhitespace(flattenContent(msg["content"]))
		if text != "" {
			systemPrompts = append(systemPrompts, text)
		}
	}
	if len(systemPrompts) == 0 {
		return false
	}
	if looksLikeSillyTavernImpersonate(systemPrompts) || looksLikeSillyTavernQuiet(systemPrompts, nil) {
		return true
	}
	for _, prompt := range systemPrompts {
		lower := strings.ToLower(collapseWhitespace(prompt))
		if strings.Contains(lower, "fictional chat between") ||
			strings.Contains(lower, "[start a new chat]") ||
			strings.Contains(lower, "[continue your last message without repeating its original content.]") {
			return true
		}
	}
	return false
}

func rawMayNeedSillyTavernPayloadFallback(raw []byte) bool {
	return bytes.Contains(raw, []byte(`"continue_prefill"`)) || bytes.Contains(raw, []byte(`"show_thoughts"`))
}

func chatCompletionInitialFlushDelayForRequest(request PromptRunRequest) time.Duration {
	if request.ClientProfile == sillyTavernClientProfile || request.StreamReasoningWarmup {
		return 0
	}
	return chatCompletionInitialFlushDelay
}

func applyInferenceResultOutputPolicy(result InferenceResult, request PromptRunRequest) InferenceResult {
	result.Text = sanitizeAssistantVisibleText(result.Text)
	result.Reasoning = sanitizeAssistantVisibleText(result.Reasoning)
	if request.SuppressReasoningOutput {
		result.Reasoning = ""
	}
	return result
}

func (a *App) runPrompt(r *http.Request, request PromptRunRequest) (InferenceResult, error) {
	if request.replayResult != nil {
		return *request.replayResult, nil
	}
	if a.runPromptOverride != nil {
		return a.runPromptOverride(r, request)
	}
	return a.runPromptWithAccountPool(r, request, nil)
}

// Replayed answers are chunked and paced so a cached response still arrives as
// a stream. Sending the whole text as one delta made every repeated request
// render as a non-streamed response, which clients that drive incremental
// rendering show as a single block of text appearing at once.
var (
	replayChunkRunes = 32
	replayChunkDelay = 15 * time.Millisecond
)

// splitReplayChunks cuts text into rune-counted chunks, never splitting a
// multi-byte character.
func splitReplayChunks(text string, size int) []string {
	if size <= 0 {
		return []string{text}
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	chunks := make([]string, 0, len(runes)/size+1)
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

// replayStreamPacing returns the chunk size and the delay between chunks for a
// result about to be written to an SSE stream.
//
// A cached replay has no upstream arrival spacing its chunks out. Writing them
// back-to-back delivered the whole answer in one burst, which clients rendered
// as a single block of text — the "no streaming effect" report. A live stream
// keeps the configured chunk size and needs no delay, because upstream already
// paces it.
func replayStreamPacing(result InferenceResult, configuredChunkRunes int) (int, time.Duration) {
	if !result.cachedReplay {
		return configuredChunkRunes, 0
	}
	return replayChunkRunes, replayChunkDelay
}

// emitReplayStream delivers a cached answer through a streaming sink without
// touching the account pool, so a repeated streamed request behaves like its
// non-streamed counterpart.
//
// This is NOT the path a replayed answer takes in production. Both streaming
// handlers check request.replayResult first and hand a cached result straight to
// writeChatCompletionStream / writeResponsesStream, so nothing here is reached
// once a replay is armed; it is the guard that keeps an armed replay from ever
// dispatching upstream, and it is exercised by tests. Pacing a replayed answer
// is the job of replayStreamPacing, on the handler above.
func emitReplayStream(request PromptRunRequest, emit func(string) error) (InferenceResult, error) {
	result := *request.replayResult
	if emit == nil {
		return result, nil
	}
	for _, chunk := range splitReplayChunks(result.Text, replayChunkRunes) {
		if err := emit(chunk); err != nil {
			return InferenceResult{}, err
		}
		if replayChunkDelay > 0 {
			time.Sleep(replayChunkDelay)
		}
	}
	return result, nil
}

func (a *App) runPromptStream(r *http.Request, request PromptRunRequest, onDelta func(string) error) (InferenceResult, error) {
	if request.replayResult != nil {
		return emitReplayStream(request, onDelta)
	}
	if a.runPromptStreamOverride != nil {
		return a.runPromptStreamOverride(r, request, onDelta)
	}
	return a.runPromptWithAccountPool(r, request, onDelta)
}

func (a *App) runPromptStreamWithSink(r *http.Request, request PromptRunRequest, sink InferenceStreamSink) (InferenceResult, error) {
	if request.replayResult != nil {
		return emitReplayStream(request, sink.Text)
	}
	if a.runPromptStreamSinkOverride != nil {
		return a.runPromptStreamSinkOverride(r, request, sink)
	}
	if a.runPromptStreamOverride != nil {
		return a.runPromptStreamOverride(r, request, sink.Text)
	}
	return a.runPromptWithAccountPoolWithSink(r, request, sink)
}

func (a *App) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	raw, err := a.decodeBodyRaw(w, r)
	if err != nil {
		writeInvalidBodyError(w, err)
		return
	}
	typed, payload, err := decodeChatCompletionsRequestBodyFromRaw(raw)
	if err != nil {
		writeInvalidBodyError(w, err)
		return
	}
	if payload == nil && (typed.likelySillyTavernByEnvelope() || maybeSillyTavernByTypedMessages(typed.Messages) || rawMayNeedSillyTavernPayloadFallback(raw)) {
		payload, err = decodeBodyMapFromRaw(raw)
		if err != nil {
			writeInvalidBodyError(w, err)
			return
		}
	}
	if payload != nil && (typed.likelySillyTavernByEnvelope() || isLikelySillyTavernPayload(payload)) {
		a.handleSillyTavernChatCompletionsPayload(w, r, payload)
		return
	}
	messages := sliceValue(typed.Messages)
	if len(messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "messages must be an array", "invalid_request_error", nilString())
		return
	}
	normalized, err := normalizeChatInputFromParts(messages, typed.Attachments)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", nilString())
		return
	}
	if normalized.Prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "messages must contain text or supported attachments", "invalid_request_error", nilString())
		return
	}
	cfg, _, registry := a.State.Snapshot()
	requestedModelID := requestedModelFromTyped(typed.Model, cfg.DefaultPublicModel())
	useWebSearch := requestedWebSearchFromTyped(typed.UseWebSearch, typed.Metadata, typed.Tools, cfg.Features.UseWebSearch)
	preferredConversationID := requestedConversationIDFromTyped(r, typed.ConversationID, typed.Conversation, typed.Metadata)
	explicitThreadID := requestedThreadIDFromTyped(r, typed.ThreadID, typed.Thread, typed.NotionThreadID, typed.Metadata)
	requestedAccount := requestedAccountEmailFromTyped(r, typed.AccountEmail, typed.NotionAccountEmail, typed.Metadata)
	requestedWorkspace := requestedWorkspaceID(r, typed.WorkspaceID, typed.SpaceID, typed.Metadata)
	entry, err := registry.Resolve(requestedModelID, cfg.DefaultPublicModel())
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_not_found")
		return
	}
	hiddenPrompt := strings.TrimSpace(normalized.HiddenPrompt)
	promptText := normalized.Prompt
	latestPrompt := resolveRequestPromptForContinuation(normalized)
	continuationScope := requestClientContinuationScope(r, "openai", "", "chat_completions", entry.ID, requestedAccount, requestedWorkspace)
	originalFingerprint := canonicalConversationFingerprintScoped(requestClientFingerprintScope(r, "openai", "", "chat_completions", entry.ID, requestedAccount, requestedWorkspace), hiddenPrompt, normalized.Segments)
	originalRawMessageCount := sessionRawMessageCount(normalized.Segments)
	request := PromptRunRequest{
		Prompt:             promptText,
		HistorySegments:    normalized.Segments,
		LatestUserPrompt:   latestPrompt,
		HiddenPrompt:       hiddenPrompt,
		PublicModel:        entry.ID,
		NotionModel:        entry.NotionModel,
		UseWebSearch:       useWebSearch,
		Attachments:        normalized.Attachments,
		SessionFingerprint: originalFingerprint,
		RawMessageCount:    originalRawMessageCount,
		ClientScope:        continuationScope,
		WorkspaceID:        requestedWorkspace,
	}
	freshThreadMode := forceFreshThreadPerRequest(cfg)
	conversation := ConversationEntry{}
	if matched, ok := a.resolveContinuationConversationWithExplicit("", originalFingerprint, continuationScope, normalized.Segments, preferredConversationID, explicitThreadID); ok {
		conversation = matched.Conversation
		if requestedWorkspace != "" && requestedWorkspace != strings.TrimSpace(conversation.SpaceID) {
			writeOpenAIError(w, http.StatusBadRequest, errConversationWorkspaceMismatch.Error(), "invalid_request_error", "conversation_workspace_mismatch")
			return
		}
		request.PinnedSpaceID = conversation.SpaceID
		request.HiddenPrompt = firstNonEmpty(request.HiddenPrompt, conversation.HiddenPrompt)
		account, accountErr := resolveContinuationAccount(cfg, strings.TrimSpace(conversation.ThreadID), requestedAccount, conversation)
		if accountErr != nil {
			writeOpenAIError(w, http.StatusBadRequest, accountErr.Error(), "invalid_request_error", "conversation_account_mismatch")
			return
		}
		request.PinnedAccountEmail = firstNonEmpty(strings.TrimSpace(conversation.AccountEmail), requestedAccount, account)
		if freshThreadMode {
			request.ForceLocalConversationContinue = strings.TrimSpace(conversation.ID) != ""
			request.Prompt = buildFreshThreadReplayPromptFromConversation(conversation, latestPrompt, normalized.Attachments, promptText)
		} else {
			request.UpstreamThreadID = strings.TrimSpace(conversation.ThreadID)
			request.continuationDraft = buildContinuationDraft(matched.Session)
			if matched.Session != nil && (request.ForceSessionRepeatTurn || request.RawMessageCount == matched.Session.Session.RawMessageCount) && requestMatchesConversationFinalTurn(request, normalized.Segments, conversation) {
				request.SessionRepeatTurn = true
				request.replayResult = replayResultFromConversation(conversation)
			}
			request.Prompt = latestPrompt
		}
	} else {
		request.PinnedAccountEmail = requestedAccount
	}
	request.ConversationID = firstNonEmpty(strings.TrimSpace(conversation.ID), preferredConversationID)
	a.markEphemeralConversationRequest(&request)
	conversationID, turnErr := a.startConversationTurn(conversation.ID, preferredConversationID, "api", "chat_completions", resolveRequestPromptForContinuation(normalized), request)
	if turnErr != nil {
		writeOpenAIError(w, http.StatusConflict, turnErr.Error(), "invalid_request_error", "conversation_busy")
		return
	}
	request.ConversationID = conversationID
	// A panic or early return must not leave the turn "running" forever.
	defer a.abandonConversationTurn(conversationID)
	setConversationIDHeader(w, conversationID)
	stream := typed.Stream
	if stream {
		includeUsage := false
		if typed.StreamIncludeUsage != nil {
			includeUsage = *typed.StreamIncludeUsage
		}
		a.writeChatCompletionLiveStream(w, r, request, entry.ID, includeUsage, conversationID)
		return
	}
	result, err := a.runPrompt(r, request)
	if err != nil {
		a.failConversation(conversationID, err)
		a.writeUpstreamError(w, err)
		return
	}
	result = applyInferenceResultOutputPolicy(result, request)
	responsePayload := buildChatCompletion(result, entry.ID, cfg.DebugUpstream)
	attachConversationResponseMetadata(responsePayload, conversationID, result.ThreadID)
	setThreadIDHeader(w, result.ThreadID)
	a.markConversationEnvelope(conversationID, "", stringValue(responsePayload["id"]))
	a.completeConversation(conversationID, result)
	a.persistConversationSession(conversationID, request, result)
	writeJSON(w, http.StatusOK, responsePayload)
}

func (a *App) handleSillyTavernChatCompletions(w http.ResponseWriter, r *http.Request) {
	payload, err := a.decodeBody(w, r)
	if err != nil {
		writeInvalidBodyError(w, err)
		return
	}
	a.handleSillyTavernChatCompletionsPayload(w, r, payload)
}

func (a *App) handleSillyTavernChatCompletionsPayload(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	ctx, err := buildSillyTavernContext(payload)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", nilString())
		return
	}
	if ctx.Normalized.Prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "messages must contain text or supported attachments", "invalid_request_error", nilString())
		return
	}

	cfg, _, registry := a.State.Snapshot()
	entry, err := registry.Resolve(requestedModel(payload, cfg.DefaultPublicModel()), cfg.DefaultPublicModel())
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_not_found")
		return
	}

	requestedAccount := requestedAccountEmail(r, payload)
	requestedWorkspace := requestedWorkspaceID(r, strings.TrimSpace(stringValue(payload["workspace_id"])), strings.TrimSpace(stringValue(payload["space_id"])), payload["metadata"])
	continuationScope := requestClientContinuationScope(r, "sillytavern", ctx.ProfileKey, "chat_completions", entry.ID, requestedAccount, requestedWorkspace)
	originalFingerprint := canonicalConversationFingerprintScoped(requestClientFingerprintScope(r, "sillytavern", ctx.ProfileKey, "chat_completions", entry.ID, requestedAccount, requestedWorkspace), ctx.StableHidden, ctx.RequestSegments)
	originalRawMessageCount := sessionRawMessageCount(ctx.RequestSegments)
	request := PromptRunRequest{
		Prompt:             ctx.Normalized.Prompt,
		HistorySegments:    ctx.RequestSegments,
		LatestUserPrompt:   ctx.LatestPrompt,
		HiddenPrompt:       ctx.RequestHidden,
		PublicModel:        entry.ID,
		NotionModel:        entry.NotionModel,
		ClientProfile:      sillyTavernClientProfile,
		ClientMode:         ctx.Mode,
		ClientSessionKey:   ctx.ProfileKey,
		UseWebSearch:       requestedWebSearch(payload, cfg.Features.UseWebSearch),
		Attachments:        ctx.Normalized.Attachments,
		SessionFingerprint: originalFingerprint,
		RawMessageCount:    originalRawMessageCount,
		ClientScope:        continuationScope,
		WorkspaceID:        requestedWorkspace,
	}
	if ctx.Mode == sillyTavernModeContinue {
		request.LatestUserPrompt = sillyTavernContinuationPrompt(payload)
	}
	request.SuppressReasoningOutput = !sillyTavernWantsReasoning(payload)
	if streamEnabled, _ := payload["stream"].(bool); streamEnabled && !request.SuppressReasoningOutput {
		request.StreamReasoningWarmup = true
	}
	a.markEphemeralConversationRequest(&request)
	freshThreadMode := forceFreshThreadPerRequest(cfg)

	preferredConversationID := requestedConversationID(r, payload)
	conversation := ConversationEntry{}
	if matched, ok := a.resolveSillyTavernContinuation(r, payload, ctx, originalFingerprint, continuationScope); ok {
		request.SuppressUpstreamThreadPersistence = matched.SuppressPersist
		conversation = matched.Target.Conversation
		if requestedWorkspace != "" && requestedWorkspace != strings.TrimSpace(conversation.SpaceID) {
			writeOpenAIError(w, http.StatusBadRequest, errConversationWorkspaceMismatch.Error(), "invalid_request_error", "conversation_workspace_mismatch")
			return
		}
		request.PinnedSpaceID = conversation.SpaceID
		request.HiddenPrompt = firstNonEmpty(request.HiddenPrompt, conversation.HiddenPrompt)
		account, accountErr := resolveContinuationAccount(cfg, strings.TrimSpace(conversation.ThreadID), requestedAccount, conversation)
		if accountErr != nil {
			writeOpenAIError(w, http.StatusBadRequest, accountErr.Error(), "invalid_request_error", "conversation_account_mismatch")
			return
		}
		request.PinnedAccountEmail = firstNonEmpty(strings.TrimSpace(conversation.AccountEmail), requestedAccount, account)
		if freshThreadMode {
			request.ForceLocalConversationContinue = strings.TrimSpace(conversation.ID) != ""
			request.Prompt = buildFreshThreadReplayPromptFromConversation(conversation, request.LatestUserPrompt, ctx.Normalized.Attachments, request.Prompt)
		} else {
			request.UpstreamThreadID = strings.TrimSpace(conversation.ThreadID)
			request.continuationDraft = buildContinuationDraft(matched.Target.Session)
			request.ForceSessionRepeatTurn = matched.ForceRepeatTurn
			if request.UpstreamThreadID != "" {
				if ctx.Mode == sillyTavernModeContinue {
					request.Prompt = sillyTavernContinuationPrompt(payload)
				} else {
					request.Prompt = ctx.LatestPrompt
				}
			}
		}
	} else {
		request.PinnedAccountEmail = requestedAccountEmail(r, payload)
		preferActiveAccountForRequest(cfg, &request)
		if ctx.Mode == sillyTavernModeQuiet || ctx.Mode == sillyTavernModeImpersona {
			request.SuppressUpstreamThreadPersistence = true
		}
	}

	if ctx.Mode != sillyTavernModeContinue && request.continuationDraft != nil && (request.ForceSessionRepeatTurn || request.RawMessageCount == request.continuationDraft.RawMessageCount) && requestMatchesConversationFinalTurn(request, ctx.RequestSegments, conversation) {
		request.SessionRepeatTurn = true
		request.replayResult = replayResultFromConversation(conversation)
	}

	request.ConversationID = firstNonEmpty(strings.TrimSpace(conversation.ID), preferredConversationID)
	displayPrompt := ctx.DisplayPrompt
	if ctx.Mode == sillyTavernModeContinue {
		displayPrompt = request.LatestUserPrompt
	}
	conversationID, turnErr := a.startConversationTurn(conversation.ID, preferredConversationID, "sillytavern", "chat_completions", displayPrompt, request)
	if turnErr != nil {
		writeOpenAIError(w, http.StatusConflict, turnErr.Error(), "invalid_request_error", "conversation_busy")
		return
	}
	request.ConversationID = conversationID
	// A panic or early return must not leave the turn "running" forever.
	defer a.abandonConversationTurn(conversationID)
	setConversationIDHeader(w, conversationID)

	stream, _ := payload["stream"].(bool)
	if stream {
		a.writeChatCompletionLiveStream(w, r, request, entry.ID, includeUsageInStream(payload), conversationID)
		return
	}

	result, err := a.runPrompt(r, request)
	if err != nil {
		a.failConversation(conversationID, err)
		a.writeUpstreamError(w, err)
		return
	}
	result = applyInferenceResultOutputPolicy(result, request)
	responsePayload := buildChatCompletion(result, entry.ID, cfg.DebugUpstream)
	attachConversationResponseMetadata(responsePayload, conversationID, result.ThreadID)
	setThreadIDHeader(w, result.ThreadID)
	a.markConversationEnvelope(conversationID, "", stringValue(responsePayload["id"]))
	a.completeConversation(conversationID, result)
	a.persistConversationSession(conversationID, request, result)
	if !request.SuppressUpstreamThreadPersistence {
		a.persistSillyTavernBinding(conversationID, ctx.ProfileKey, ctx.Mode)
	}
	writeJSON(w, http.StatusOK, responsePayload)
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request) {
	raw, err := a.decodeBodyRaw(w, r)
	if err != nil {
		writeInvalidBodyError(w, err)
		return
	}
	typed, _, err := decodeResponsesRequestBodyFromRaw(raw)
	if err != nil {
		writeInvalidBodyError(w, err)
		return
	}
	stream := typed.Stream
	var previousResponse map[string]any
	previousResponseID := strings.TrimSpace(typed.PreviousResponseID)
	if previousResponseID != "" {
		var ok bool
		previousResponse, ok = a.State.getResponse(previousResponseID)
		if !ok {
			_, ok = a.State.getContinuationResponse(previousResponseID)
		}
		if !ok {
			writeOpenAIError(w, http.StatusNotFound, "response not found", "invalid_request_error", "response_not_found")
			return
		}
	}
	normalized, err := normalizeResponsesInputFromParts(typed.Input, typed.Attachments, previousResponse)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", nilString())
		return
	}
	if normalized.Prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "input must contain text or supported attachments", "invalid_request_error", nilString())
		return
	}
	cfg, _, registry := a.State.Snapshot()
	requestedModelID := requestedModelFromTyped(typed.Model, cfg.DefaultPublicModel())
	useWebSearch := requestedWebSearchFromTyped(typed.UseWebSearch, typed.Metadata, typed.Tools, cfg.Features.UseWebSearch)
	preferredConversationID := requestedConversationIDFromTyped(r, typed.ConversationID, typed.Conversation, typed.Metadata)
	explicitThreadID := requestedThreadIDFromTyped(r, typed.ThreadID, typed.Thread, typed.NotionThreadID, typed.Metadata)
	requestedAccount := requestedAccountEmailFromTyped(r, typed.AccountEmail, typed.NotionAccountEmail, typed.Metadata)
	requestedWorkspace := requestedWorkspaceID(r, typed.WorkspaceID, typed.SpaceID, typed.Metadata)
	entry, err := registry.Resolve(requestedModelID, cfg.DefaultPublicModel())
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_not_found")
		return
	}
	hiddenPrompt := strings.TrimSpace(normalized.HiddenPrompt)
	promptText := normalized.Prompt
	latestPrompt := resolveRequestPromptForContinuation(normalized)
	continuationScope := requestClientContinuationScope(r, "openai", "", "responses", entry.ID, requestedAccount, requestedWorkspace)
	originalFingerprint := canonicalConversationFingerprintScoped(requestClientFingerprintScope(r, "openai", "", "responses", entry.ID, requestedAccount, requestedWorkspace), hiddenPrompt, normalized.Segments)
	originalRawMessageCount := sessionRawMessageCount(normalized.Segments)
	request := PromptRunRequest{
		Prompt:             promptText,
		HistorySegments:    normalized.Segments,
		LatestUserPrompt:   latestPrompt,
		HiddenPrompt:       hiddenPrompt,
		PublicModel:        entry.ID,
		NotionModel:        entry.NotionModel,
		UseWebSearch:       useWebSearch,
		Attachments:        normalized.Attachments,
		SessionFingerprint: originalFingerprint,
		RawMessageCount:    originalRawMessageCount,
		ClientScope:        continuationScope,
		WorkspaceID:        requestedWorkspace,
	}
	freshThreadMode := forceFreshThreadPerRequest(cfg)
	conversation := ConversationEntry{}
	if matched, ok := a.resolveContinuationConversationWithExplicit(previousResponseID, originalFingerprint, continuationScope, normalized.Segments, preferredConversationID, explicitThreadID); ok {
		conversation = matched.Conversation
		if requestedWorkspace != "" && requestedWorkspace != strings.TrimSpace(conversation.SpaceID) {
			writeOpenAIError(w, http.StatusBadRequest, errConversationWorkspaceMismatch.Error(), "invalid_request_error", "conversation_workspace_mismatch")
			return
		}
		request.PinnedSpaceID = conversation.SpaceID
		request.HiddenPrompt = firstNonEmpty(request.HiddenPrompt, conversation.HiddenPrompt)
		account, accountErr := resolveContinuationAccount(cfg, strings.TrimSpace(conversation.ThreadID), requestedAccount, conversation)
		if accountErr != nil {
			writeOpenAIError(w, http.StatusBadRequest, accountErr.Error(), "invalid_request_error", "conversation_account_mismatch")
			return
		}
		request.PinnedAccountEmail = firstNonEmpty(strings.TrimSpace(conversation.AccountEmail), requestedAccount, account)
		if freshThreadMode {
			request.ForceLocalConversationContinue = strings.TrimSpace(conversation.ID) != ""
			request.Prompt = buildFreshThreadReplayPromptFromConversation(conversation, latestPrompt, normalized.Attachments, promptText)
		} else {
			request.UpstreamThreadID = strings.TrimSpace(conversation.ThreadID)
			request.continuationDraft = buildContinuationDraft(matched.Session)
			if matched.Session != nil && (request.ForceSessionRepeatTurn || request.RawMessageCount == matched.Session.Session.RawMessageCount) && requestMatchesConversationFinalTurn(request, normalized.Segments, conversation) {
				request.SessionRepeatTurn = true
				request.replayResult = replayResultFromConversation(conversation)
			}
			request.Prompt = latestPrompt
		}
	} else {
		request.PinnedAccountEmail = requestedAccount
	}
	if freshThreadMode && strings.TrimSpace(conversation.ID) == "" {
		request.Prompt = buildFreshThreadReplayPromptFromStoredResponse(normalized.PreviousResponsePrompt, latestPrompt, normalized.Attachments, request.Prompt)
	}
	request.ConversationID = firstNonEmpty(strings.TrimSpace(conversation.ID), preferredConversationID)
	a.markEphemeralConversationRequest(&request)
	conversationID, turnErr := a.startConversationTurn(conversation.ID, preferredConversationID, "api", "responses", resolveRequestPromptForContinuation(normalized), request)
	if turnErr != nil {
		writeOpenAIError(w, http.StatusConflict, turnErr.Error(), "invalid_request_error", "conversation_busy")
		return
	}
	request.ConversationID = conversationID
	// A panic or early return must not leave the turn "running" forever.
	defer a.abandonConversationTurn(conversationID)
	setConversationIDHeader(w, conversationID)
	if stream {
		a.writeResponsesLiveStream(w, r, request, entry.ID, cfg.DebugUpstream, conversationID)
		return
	}
	result, err := a.runPrompt(r, request)
	if err != nil {
		a.failConversation(conversationID, err)
		a.writeUpstreamError(w, err)
		return
	}
	result = applyInferenceResultOutputPolicy(result, request)
	responsePayload := buildResponsesOutputWithIDs(
		result,
		entry.ID,
		cfg.DebugUpstream,
		"resp_"+strings.ReplaceAll(randomUUID(), "-", ""),
		"msg_"+strings.ReplaceAll(randomUUID(), "-", ""),
		time.Now().Unix(),
	)
	attachConversationResponseMetadata(responsePayload, conversationID, result.ThreadID)
	setThreadIDHeader(w, result.ThreadID)
	responseID := stringValue(responsePayload["id"])
	if responseID != "" {
		a.State.saveResponseWithAccount(responseID, responsePayload, conversationID, result.ThreadID, result.AccountEmail)
	}
	a.markConversationEnvelope(conversationID, responseID, "")
	a.completeConversation(conversationID, result)
	a.persistConversationSession(conversationID, request, result)
	writeJSON(w, http.StatusOK, responsePayload)
}

func (a *App) writeUpstreamError(w http.ResponseWriter, err error) {
	var selectionErr *modelSelectionError
	if errors.As(err, &selectionErr) {
		writeModelSelectionError(w, err)
		return
	}
	if isClientInputError(err) {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request_input")
		return
	}
	var apiErr *notionAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
		if !apiErr.RetryAfter.IsZero() {
			w.Header().Set("Retry-After", apiErr.RetryAfter.UTC().Format(http.TimeFormat))
		}
		writeOpenAIError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error", "upstream_rate_limited")
		return
	}
	message := err.Error()
	lower := strings.ToLower(message)
	if isDispatchCapacityExceededError(err) {
		writeOpenAIError(w, http.StatusTooManyRequests, message, "rate_limit_error", "dispatch_capacity_exceeded")
		return
	}
	if strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "timeout") {
		writeOpenAIError(w, http.StatusGatewayTimeout, message, "api_timeout_error", "upstream_timeout")
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, message, "api_error", "upstream_error")
}

func prepareOpenAISSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

var (
	chatCompletionInitialFlushDelay = 1500 * time.Millisecond
)

func writeSSEDone(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (a *App) writeChatCompletionStream(w http.ResponseWriter, r *http.Request, result InferenceResult, modelID string, includeUsage bool, conversationID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming is not supported by this response writer", "api_error", "stream_unsupported")
		return
	}
	prepareOpenAISSEHeaders(w)

	completionID := "chatcmpl-" + strings.ReplaceAll(randomUUID(), "-", "")
	created := time.Now().Unix()
	assistantText := sanitizeAssistantVisibleText(result.Text)
	reasoningText := sanitizeAssistantVisibleText(result.Reasoning)

	chunks := []map[string]any{
		buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamDeltaChoice(0, map[string]any{"role": "assistant"}),
		}, nil),
	}
	cfg, _, _ := a.State.Snapshot()
	chunkRunes, chunkDelay := replayStreamPacing(result, cfg.StreamChunkRunes)
	for _, part := range splitTextChunks(reasoningText, chunkRunes) {
		if part == "" {
			continue
		}
		chunks = append(chunks, buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamReasoningChoice(0, part),
		}, nil))
	}
	for _, part := range splitTextChunks(assistantText, chunkRunes) {
		chunks = append(chunks, buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamDeltaChoice(0, map[string]any{"content": part}),
		}, nil))
	}
	finalUsage := map[string]any{}
	if includeUsage {
		finalUsage = buildUsage(result.Prompt, assistantText, reasoningText)
	}
	// This is the landing point for a replayed answer (writeChatCompletionLiveStream
	// delegates a cached result here), so it has to report truncation too.
	// Hardcoding "stop" meant a replayed cut-off answer looked clean to the
	// client, which is exactly what the live path already avoids.
	finishReason := "stop"
	if result.Truncated {
		finishReason = "length"
	}
	chunks = append(chunks, buildChatStreamChunk(completionID, created, modelID, []map[string]any{
		buildChatStreamFinishChoice(0, finishReason),
	}, finalUsage))

	for index, chunk := range chunks {
		if err := writeSSEData(w, flusher, chunk); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		default:
		}
		// The terminal chunk is not followed by a delay: it ends the turn.
		if chunkDelay > 0 && index < len(chunks)-1 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(chunkDelay):
			}
		}
	}
	writeSSEDone(w, flusher)
}

func (a *App) writeChatCompletionLiveStream(w http.ResponseWriter, r *http.Request, request PromptRunRequest, modelID string, includeUsage bool, conversationID string) {
	if request.replayResult != nil {
		setThreadIDHeader(w, request.replayResult.ThreadID)
		a.writeChatCompletionStream(w, r, *request.replayResult, modelID, includeUsage, conversationID)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming is not supported by this response writer", "api_error", "stream_unsupported")
		return
	}

	completionID := "chatcmpl-" + strings.ReplaceAll(randomUUID(), "-", "")
	created := time.Now().Unix()
	var emittedVisibleText strings.Builder
	var emittedReasoning strings.Builder
	warmupSent := false
	const reasoningHeartbeat = "\u200b"
	var writeMu sync.Mutex
	headersSent := false
	// streamStarted reads headersSent under writeMu; sink callbacks set it
	// from the upstream reader goroutine.
	streamStarted := func() bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		return headersSent
	}
	safeWriteData := func(payload any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeSSEData(w, flusher, payload)
	}
	safeWriteDone := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeSSEDone(w, flusher)
	}
	startStream := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if headersSent {
			return nil
		}
		headersSent = true
		prepareOpenAISSEHeaders(w)
		a.markConversationEnvelope(conversationID, "", completionID)
		return writeSSEData(w, flusher, buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamDeltaChoice(0, map[string]any{"role": "assistant"}),
		}, nil))
	}
	emitContent := func(part string) error {
		if part == "" {
			return nil
		}
		if err := startStream(); err != nil {
			return err
		}
		emittedVisibleText.WriteString(part)
		return safeWriteData(buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamDeltaChoice(0, map[string]any{"content": part}),
		}, nil))
	}
	emitReasoning := func(part string) error {
		if part == "" || request.SuppressReasoningOutput {
			return nil
		}
		if err := startStream(); err != nil {
			return err
		}
		emittedReasoning.WriteString(part)
		return safeWriteData(buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamReasoningChoice(0, part),
		}, nil))
	}
	emitReasoningWarmup := func() error {
		if !request.StreamReasoningWarmup || request.SuppressReasoningOutput {
			return nil
		}
		if warmupSent {
			return nil
		}
		if err := startStream(); err != nil {
			return err
		}
		warmupSent = true
		return safeWriteData(buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamReasoningChoice(0, reasoningHeartbeat),
		}, nil))
	}
	emitKeepAlive := func() error {
		if err := startStream(); err != nil {
			return err
		}
		return safeWriteData(buildChatStreamChunk(completionID, created, modelID, []map[string]any{
			buildChatStreamHeartbeatChoice(0),
		}, nil))
	}
	stopProactiveFlush := make(chan struct{})
	var proactiveFlushWG sync.WaitGroup
	var stopProactiveFlushOnce sync.Once
	stopInitialFlush := func() {
		stopProactiveFlushOnce.Do(func() { close(stopProactiveFlush) })
		proactiveFlushWG.Wait()
	}
	defer stopInitialFlush()
	if chatCompletionInitialFlushDelayForRequest(request) <= 0 {
		_ = startStream()
		_ = emitReasoningWarmup()
	} else {
		proactiveFlushWG.Add(1)
		go func() {
			defer proactiveFlushWG.Done()
			timer := time.NewTimer(chatCompletionInitialFlushDelayForRequest(request))
			defer timer.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-stopProactiveFlush:
					return
				case <-timer.C:
					if err := startStream(); err == nil {
						_ = emitKeepAlive()
					}
					return
				}
			}
		}()
	}
	result, err := a.runPromptStreamWithSink(r, request, InferenceStreamSink{
		Text: func(delta string) error {
			if delta == "" {
				return nil
			}
			a.pushConversationDelta(conversationID, delta)
			return emitContent(delta)
		},
		Reasoning:       emitReasoning,
		ReasoningWarmup: emitReasoningWarmup,
		KeepAlive:       emitKeepAlive,
	})
	stopInitialFlush()
	if err != nil {
		// A stream that died mid-flight must never be reported as a clean
		// stop: the client already saw partial text, so the error event is the
		// only way to tell it the answer was truncated.
		a.failConversation(conversationID, err)
		if !streamStarted() {
			a.writeUpstreamError(w, err)
			return
		}
		_ = safeWriteData(map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "api_error",
				"param":   nil,
				"code":    "upstream_error",
			},
		})
		safeWriteDone()
		return
	}
	result = applyInferenceResultOutputPolicy(result, request)
	a.completeConversation(conversationID, result)
	a.persistConversationSession(conversationID, request, result)
	if request.ClientProfile == sillyTavernClientProfile && !request.SuppressUpstreamThreadPersistence {
		a.persistSillyTavernBinding(conversationID, request.ClientSessionKey, request.ClientMode)
	}

	assistantText := result.Text
	reasoningText := result.Reasoning
	finalUsage := map[string]any{}
	if includeUsage {
		finalUsage = buildUsage(result.Prompt, assistantText, reasoningText)
	}
	if remainingReasoning := textDeltaSuffix(emittedReasoning.String(), reasoningText); remainingReasoning != "" {
		if err := emitReasoning(remainingReasoning); err != nil {
			return
		}
	}
	if remainingText := textDeltaSuffix(emittedVisibleText.String(), assistantText); remainingText != "" {
		if err := emitContent(remainingText); err != nil {
			return
		}
	}
	if err := startStream(); err != nil {
		return
	}
	finishReason := "stop"
	if result.Truncated {
		finishReason = "length"
	}
	_ = safeWriteData(buildChatStreamChunk(completionID, created, modelID, []map[string]any{
		buildChatStreamFinishChoice(0, finishReason),
	}, finalUsage))
	safeWriteDone()
}

func (a *App) writeResponsesLiveStream(w http.ResponseWriter, r *http.Request, request PromptRunRequest, modelID string, includeTrace bool, conversationID string) {
	if request.replayResult != nil {
		setThreadIDHeader(w, request.replayResult.ThreadID)
		a.writeResponsesStream(w, r, *request.replayResult, modelID, includeTrace, conversationID)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming is not supported by this response writer", "api_error", "stream_unsupported")
		return
	}

	responseID := "resp_" + strings.ReplaceAll(randomUUID(), "-", "")
	outputItemID := "msg_" + strings.ReplaceAll(randomUUID(), "-", "")
	createdAt := time.Now().Unix()
	inProgressResponse := buildResponsesInProgressObject(responseID, modelID, createdAt)
	attachConversationResponseMetadata(inProgressResponse, conversationID, "")
	inProgressItem := buildResponsesMessageItem(outputItemID, "", "in_progress")
	sequenceNumber := 0
	var writeMu sync.Mutex
	safeWriteEvent := func(eventType string, payload map[string]any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if payload == nil {
			payload = map[string]any{}
		}
		payload["sequence_number"] = sequenceNumber
		sequenceNumber++
		return writeSSEEvent(w, flusher, eventType, payload)
	}
	safeWriteDone := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeSSEDone(w, flusher)
	}
	headersSent := false
	// streamStarted reads headersSent under writeMu; sink callbacks set it
	// from the upstream reader goroutine.
	streamStarted := func() bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		return headersSent
	}
	startStream := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if headersSent {
			return nil
		}
		headersSent = true
		prepareOpenAISSEHeaders(w)
		a.markConversationEnvelope(conversationID, responseID, "")
		initialEvents := []struct {
			name    string
			payload map[string]any
		}{
			{name: "response.created", payload: buildResponsesCreatedEvent(inProgressResponse)},
			{name: "response.in_progress", payload: buildResponsesInProgressEvent(inProgressResponse)},
			{name: "response.output_item.added", payload: buildResponsesOutputItemAddedEvent(responseID, inProgressItem)},
			{name: "response.content_part.added", payload: buildResponsesContentPartAddedEvent(responseID, outputItemID)},
		}
		for _, event := range initialEvents {
			payload := event.payload
			if payload == nil {
				payload = map[string]any{}
			}
			payload["sequence_number"] = sequenceNumber
			sequenceNumber++
			if err := writeSSEEvent(w, flusher, event.name, payload); err != nil {
				return err
			}
		}
		return nil
	}
	var emittedVisibleText strings.Builder
	var emittedReasoning strings.Builder
	warmupSent := false
	const reasoningHeartbeat = "\u200b"
	reasoningPhaseStarted := false
	reasoningPhaseDone := false
	emitTextDelta := func(part string) error {
		if part == "" {
			return nil
		}
		if err := startStream(); err != nil {
			return err
		}
		if reasoningPhaseStarted && !reasoningPhaseDone {
			reasoningPhaseDone = true
			if sanitizeAssistantVisibleText(emittedReasoning.String()) != "" {
				if err := safeWriteEvent("response.reasoning.done", buildResponsesReasoningDoneEvent(
					responseID,
					outputItemID,
					"",
				)); err != nil {
					return err
				}
			}
		}
		reasoningPhaseStarted = false
		emittedVisibleText.WriteString(part)
		return safeWriteEvent("response.output_text.delta", buildResponsesOutputTextDeltaEvent(responseID, outputItemID, part))
	}
	emitReasoningDelta := func(part string) error {
		if part == "" || request.SuppressReasoningOutput {
			return nil
		}
		if err := startStream(); err != nil {
			return err
		}
		reasoningPhaseStarted = true
		emittedReasoning.WriteString(part)
		return safeWriteEvent("response.reasoning.delta", buildResponsesReasoningDeltaEvent(responseID, outputItemID, part))
	}
	emitReasoningWarmup := func() error {
		if !request.StreamReasoningWarmup || request.SuppressReasoningOutput {
			return nil
		}
		if warmupSent {
			return nil
		}
		if err := startStream(); err != nil {
			return err
		}
		warmupSent = true
		reasoningPhaseStarted = true
		return safeWriteEvent("response.reasoning.delta", buildResponsesReasoningDeltaEvent(responseID, outputItemID, reasoningHeartbeat))
	}
	emitKeepAlive := func() error {
		if err := startStream(); err != nil {
			return err
		}
		return safeWriteEvent("response.in_progress", buildResponsesInProgressEvent(inProgressResponse))
	}

	if request.StreamReasoningWarmup {
		_ = emitReasoningWarmup()
	}

	result, err := a.runPromptStreamWithSink(r, request, InferenceStreamSink{
		Text: func(delta string) error {
			if delta == "" {
				return nil
			}
			a.pushConversationDelta(conversationID, delta)
			return emitTextDelta(delta)
		},
		Reasoning:       emitReasoningDelta,
		ReasoningWarmup: emitReasoningWarmup,
		KeepAlive:       emitKeepAlive,
	})
	if err != nil {
		a.failConversation(conversationID, err)
		if !streamStarted() {
			a.writeUpstreamError(w, err)
			return
		}
		failedResponse := buildResponsesFailedObject(responseID, modelID, createdAt, err.Error())
		if partialText := sanitizeAssistantVisibleText(emittedVisibleText.String()); partialText != "" {
			failedResponse["output"] = []any{buildResponsesMessageItem(outputItemID, partialText, "incomplete")}
		}
		if conversation, ok := a.State.conversations().Get(conversationID); ok {
			attachConversationResponseMetadata(failedResponse, conversationID, conversation.ThreadID)
			a.State.saveResponseWithAccount(responseID, failedResponse, conversationID, conversation.ThreadID, conversation.AccountEmail)
		}
		_ = safeWriteEvent("response.failed", buildResponsesFailedEvent(failedResponse))
		safeWriteDone()
		return
	}

	result = applyInferenceResultOutputPolicy(result, request)
	finalText := result.Text
	if strings.TrimSpace(result.Text) == "" && strings.TrimSpace(finalText) != "" {
		result.Text = finalText
	} else if strings.TrimSpace(result.Text) != finalText {
		result.Text = finalText
	}
	if remainingReasoning := textDeltaSuffix(emittedReasoning.String(), result.Reasoning); remainingReasoning != "" {
		if err := emitReasoningDelta(remainingReasoning); err != nil {
			return
		}
	}
	if remainingText := textDeltaSuffix(emittedVisibleText.String(), finalText); remainingText != "" {
		if err := emitTextDelta(remainingText); err != nil {
			return
		}
	}
	completedResponse := buildResponsesOutputWithIDs(result, modelID, includeTrace, responseID, outputItemID, createdAt)
	attachConversationResponseMetadata(completedResponse, conversationID, result.ThreadID)
	a.State.saveResponseWithAccount(responseID, completedResponse, conversationID, result.ThreadID, result.AccountEmail)
	a.completeConversation(conversationID, result)
	a.persistConversationSession(conversationID, request, result)
	streamCompletedItem := buildResponsesStreamTerminalItem(outputItemID, responsesTerminalStatus(result.Truncated))
	streamCompletedResponse := buildResponsesStreamCompletedResponse(completedResponse, outputItemID)
	if err := startStream(); err != nil {
		return
	}
	finalEvents := []struct {
		name    string
		payload map[string]any
	}{
		{name: "response.output_text.done", payload: buildResponsesOutputTextDoneEvent(responseID, outputItemID, "")},
		{name: "response.content_part.done", payload: buildResponsesContentPartDoneEvent(responseID, outputItemID, "")},
	}
	if result.Reasoning != "" && !reasoningPhaseDone {
		reasoningPhaseDone = true
		finalEvents = append(finalEvents, struct {
			name    string
			payload map[string]any
		}{name: "response.reasoning.done", payload: buildResponsesReasoningDoneEvent(responseID, outputItemID, "")})
	}
	finalEvents = append(finalEvents, struct {
		name    string
		payload map[string]any
	}{name: "response.output_item.done", payload: buildResponsesOutputItemDoneEvent(responseID, streamCompletedItem)})
	for _, event := range finalEvents {
		if err := safeWriteEvent(event.name, event.payload); err != nil {
			return
		}
	}
	if err := safeWriteEvent(responsesTerminalEventName(result.Truncated), buildResponsesCompletedEvent(streamCompletedResponse)); err != nil {
		return
	}
	safeWriteDone()
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, payload any) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
		return err
	}
	return writeSSEData(w, flusher, payload)
}

func writeSSEData(w http.ResponseWriter, flusher http.Flusher, payload any) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", marshalJSON(payload)); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEComment(w http.ResponseWriter, flusher http.Flusher, comment string) error {
	if strings.TrimSpace(comment) == "" {
		comment = "keepalive"
	}
	if _, err := fmt.Fprintf(w, ": %s\n\n", comment); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (a *App) writeResponsesStream(w http.ResponseWriter, r *http.Request, result InferenceResult, modelID string, includeTrace bool, conversationID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming is not supported by this response writer", "api_error", "stream_unsupported")
		return
	}
	prepareOpenAISSEHeaders(w)

	responseID := "resp_" + strings.ReplaceAll(randomUUID(), "-", "")
	outputItemID := "msg_" + strings.ReplaceAll(randomUUID(), "-", "")
	createdAt := time.Now().Unix()
	inProgressResponse := buildResponsesInProgressObject(responseID, modelID, createdAt)
	assistantText := sanitizeAssistantVisibleText(result.Text)
	completedResponse := buildResponsesOutputWithIDs(result, modelID, includeTrace, responseID, outputItemID, createdAt)
	attachConversationResponseMetadata(inProgressResponse, conversationID, "")
	attachConversationResponseMetadata(completedResponse, conversationID, result.ThreadID)
	a.State.saveResponseWithAccount(responseID, completedResponse, conversationID, result.ThreadID, result.AccountEmail)
	streamCompletedItem := buildResponsesStreamTerminalItem(outputItemID, responsesTerminalStatus(result.Truncated))
	streamCompletedResponse := buildResponsesStreamCompletedResponse(completedResponse, outputItemID)
	inProgressItem := buildResponsesMessageItem(outputItemID, "", "in_progress")
	cfg, _, _ := a.State.Snapshot()
	sequenceNumber := 0
	writeEvent := func(eventType string, payload map[string]any) error {
		if payload == nil {
			payload = map[string]any{}
		}
		payload["sequence_number"] = sequenceNumber
		sequenceNumber++
		return writeSSEEvent(w, flusher, eventType, payload)
	}

	events := []struct {
		name    string
		payload map[string]any
	}{
		{name: "response.created", payload: buildResponsesCreatedEvent(inProgressResponse)},
		{name: "response.in_progress", payload: buildResponsesInProgressEvent(inProgressResponse)},
		{name: "response.output_item.added", payload: buildResponsesOutputItemAddedEvent(responseID, inProgressItem)},
		{name: "response.content_part.added", payload: buildResponsesContentPartAddedEvent(responseID, outputItemID)},
	}

	for _, event := range events {
		if err := writeEvent(event.name, event.payload); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		default:
		}
	}

	chunkRunes, chunkDelay := replayStreamPacing(result, cfg.StreamChunkRunes)
	parts := splitTextChunks(assistantText, chunkRunes)
	for index, part := range parts {
		if err := writeEvent("response.output_text.delta", buildResponsesOutputTextDeltaEvent(responseID, outputItemID, part)); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if chunkDelay > 0 && index < len(parts)-1 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(chunkDelay):
			}
		}
	}

	finalEvents := []struct {
		name    string
		payload map[string]any
	}{
		{name: "response.output_text.done", payload: buildResponsesOutputTextDoneEvent(responseID, outputItemID, "")},
		{name: "response.content_part.done", payload: buildResponsesContentPartDoneEvent(responseID, outputItemID, "")},
		{name: "response.output_item.done", payload: buildResponsesOutputItemDoneEvent(responseID, streamCompletedItem)},
	}
	for _, event := range finalEvents {
		if err := writeEvent(event.name, event.payload); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		default:
		}
	}
	if err := writeEvent(responsesTerminalEventName(result.Truncated), buildResponsesCompletedEvent(streamCompletedResponse)); err != nil {
		return
	}
	select {
	case <-r.Context().Done():
		return
	default:
	}
	writeSSEDone(w, flusher)
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	statusCode := http.StatusOK
	defer func() {
		observeRequestDuration(r.URL.Path, r.Method, statusCode, time.Since(startedAt))
	}()
	safeWriter := &panicSafeResponseWriter{ResponseWriter: w}
	applyCORSHeadersForPath(safeWriter, r.URL.Path)
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := strings.TrimSpace(string(debug.Stack()))
			log.Printf("[panic] %s %s remote=%s panic=%v\n%s", r.Method, r.URL.Path, r.RemoteAddr, recovered, stack)
			cfg, _, _ := a.State.Snapshot()
			message := "internal server panic"
			if cfg.DebugUpstream {
				message = fmt.Sprintf("internal server panic: %v", recovered)
			}
			contentType := strings.ToLower(strings.TrimSpace(safeWriter.Header().Get("Content-Type")))
			if !safeWriter.wroteHeader {
				writeOpenAIError(safeWriter, http.StatusInternalServerError, message, "api_error", "internal_panic")
				return
			}
			if strings.Contains(contentType, "text/event-stream") {
				payload := map[string]any{
					"error": map[string]any{
						"message": message,
						"type":    "api_error",
						"code":    "internal_panic",
					},
				}
				if encoded, err := json.Marshal(payload); err == nil {
					_, _ = fmt.Fprintf(safeWriter, "event: error\ndata: %s\n\n", encoded)
				}
				_, _ = fmt.Fprint(safeWriter, "data: [DONE]\n\n")
				safeWriter.Flush()
			}
		}
	}()

	if r.Method == http.MethodOptions {
		safeWriter.WriteHeader(http.StatusNoContent)
		statusCode = safeWriter.status
		return
	}

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/":
		a.serveIndex(safeWriter)
		statusCode = safeWriter.status
		return
	case strings.HasPrefix(path, "/admin"):
		a.handleAdmin(safeWriter, r)
		statusCode = safeWriter.status
		return
	case r.Method == http.MethodGet && path == "/healthz":
		a.serveHealthz(safeWriter, r)
		statusCode = safeWriter.status
		return
	}

	if !a.authOK(safeWriter, r) {
		statusCode = safeWriter.status
		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/v1/models":
		a.serveModels(safeWriter)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/models/"):
		a.serveModelByID(safeWriter, path)
	case r.Method == http.MethodGet && path == "/debug/vars":
		expvar.Handler().ServeHTTP(safeWriter, r)
	case r.Method == http.MethodGet && path == "/metrics":
		writePrometheusMetrics(safeWriter)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/responses/"):
		a.serveResponseByID(safeWriter, path)
	case r.Method == http.MethodPost && path == "/v1/st/chat/completions":
		a.handleSillyTavernChatCompletions(safeWriter, r)
	case r.Method == http.MethodPost && path == "/v1/chat/completions":
		a.handleChatCompletions(safeWriter, r)
	case r.Method == http.MethodPost && path == "/v1/responses":
		a.handleResponses(safeWriter, r)
	default:
		writeOpenAIError(safeWriter, http.StatusNotFound, "route not found", "invalid_request_error", "not_found")
	}
	statusCode = safeWriter.status
}

const (
	// shutdownGracePeriod bounds how long in-flight requests may finish after
	// SIGINT/SIGTERM before connections are closed.
	shutdownGracePeriod = 30 * time.Second
	// shutdownHandlerDrain is how long handlers interrupted by a forced close
	// get to record their final conversation state before the store closes.
	shutdownHandlerDrain = 5 * time.Second
)

func Main() {
	cfg := parseCLI()
	state, err := newServerState(cfg)
	if err != nil {
		log.Fatalf("init state failed: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app := &App{State: state}
	state.StartSessionRefreshLoop(ctx)
	app.StartEphemeralConversationCleanupLoop(ctx)
	if cfg.Debug.PprofEnabled {
		go func(addr string) {
			log.Printf("[pprof] listening on http://%s/debug/pprof/ (local debug endpoint; avoid public exposure)", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Printf("[pprof] server stopped: %v", err)
			}
		}(cfg.Debug.PprofAddr)
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var activeHandlers atomic.Int64
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			activeHandlers.Add(1)
			defer activeHandlers.Add(-1)
			app.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 15 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("[notion2api-go] listening on http://%s default_model=%s", addr, cfg.DefaultPublicModel())
		serveErr <- server.ListenAndServe()
	}()
	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			_ = state.Close()
			log.Fatal(err)
		}
	case <-ctx.Done():
		log.Printf("[notion2api-go] shutdown signal received; draining in-flight requests (up to %s)", shutdownGracePeriod)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("[notion2api-go] graceful shutdown incomplete: %v; closing remaining connections", err)
			_ = server.Close()
		}
		cancel()
		// Handlers whose connection was force-closed still record their
		// failed turn; give them a moment before the store closes.
		deadline := time.Now().Add(shutdownHandlerDrain)
		for activeHandlers.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := state.Close(); err != nil {
		log.Printf("[notion2api-go] closing state failed: %v", err)
	}
	log.Printf("[notion2api-go] stopped")
}
