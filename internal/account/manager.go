// Package account implements the multi-account system. It manages one or more
// Kiro accounts with intelligent failover, a sticky global index, and a circuit
// breaker (exponential backoff with probabilistic retry). Account runtime state
// (failure counters and usage statistics) is persisted to a JSON state file and
// merged back on startup.
//
// Design notes:
//   - Each account owns its own *auth.KiroAuthManager, *modelcache.ModelInfoCache
//     and *model.Resolver, lazily initialized on first use.
//   - Model-list fetching (upstream.ListAvailableModels) lives outside this
//     package to avoid an import cycle with api/*. Callers inject a ModelFetcher
//     (or refresh a cache directly); when none is set accounts fall back to the
//     configured fallback model list.
//   - The package imports only auth, modelcache, model and config; it never
//     imports api/*.
package account

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/auth"
	"git.gasi.dev/robin/kiro-proxy/internal/config"
	"git.gasi.dev/robin/kiro-proxy/internal/model"
	"git.gasi.dev/robin/kiro-proxy/internal/modelcache"
)

// ModelFetcher fetches an account's model catalog from upstream — model IDs plus
// the token limits Kiro reports for them (typically a thin wrapper over
// upstream.ListAvailableModels). It is injected rather than imported so the
// account package stays free of an api/* dependency. The account being refreshed
// is passed so the fetcher can read its auth manager via Account.Auth.
type ModelFetcher func(ctx context.Context, a *Account) ([]model.CatalogModel, error)

// AccountStats tracks per-account request counters for monitoring.
type AccountStats struct {
	TotalRequests      int64 `json:"total_requests"`
	SuccessfulRequests int64 `json:"successful_requests"`
	FailedRequests     int64 `json:"failed_requests"`
}

// Account is a complete Kiro account entity: its authentication, model cache,
// resolver and runtime circuit-breaker state. All dependencies are lazily
// initialized on first use by Manager.Pick. Fields are guarded by the owning
// Manager's mutex; access them through the exported accessors.
type Account struct {
	id       string
	authOpts auth.Options // template used for lazy initialization

	auth     *auth.KiroAuthManager
	cache    *modelcache.ModelInfoCache
	resolver *model.Resolver

	failures        int
	lastFailureTime time.Time
	modelsCachedAt  time.Time
	stats           AccountStats

	initialized bool
}

// ID returns the stable account identifier (a resolved file path for
// json/sqlite credentials, "refresh_token_<hash>" for inline refresh tokens, or
// "default" for the legacy single-account path).
func (a *Account) ID() string { return a.id }

// Auth returns the account's authentication manager, or nil if the account has
// not been initialized yet. The returned *auth.KiroAuthManager satisfies the
// upstream AuthProvider contract.
func (a *Account) Auth() *auth.KiroAuthManager { return a.auth }

// Resolver returns the account's model resolver, or nil before initialization.
func (a *Account) Resolver() *model.Resolver { return a.resolver }

// Cache returns the account's model cache, or nil before initialization.
func (a *Account) Cache() *modelcache.ModelInfoCache { return a.cache }

// Stats returns a snapshot of the account's usage statistics.
func (a *Account) Stats() AccountStats { return a.stats }

// Manager owns the set of accounts and the failover policy. It is safe for
// concurrent use. Construct one with New.
type Manager struct {
	cfg *config.Config
	hc  *http.Client
	log *slog.Logger

	mu           sync.Mutex
	accounts     []*Account // insertion order; the sticky index addresses this slice
	byID         map[string]*Account
	currentIndex int
	dirty        bool

	fetch ModelFetcher

	recoveryTimeout time.Duration
	maxBackoff      float64
	probRetry       float64
	accountCacheTTL time.Duration
	cacheTTL        time.Duration
	fallbackIDs     []string
}

// credentialEntry is a single record in the credentials.json array.
type credentialEntry struct {
	Type         string `json:"type"`
	Path         string `json:"path"`
	RefreshToken string `json:"refresh_token"`
	ProfileARN   string `json:"profile_arn"`
	Region       string `json:"region"`
	APIRegion    string `json:"api_region"`
	Enabled      *bool  `json:"enabled"`
}

// New constructs a Manager. When cfg.AccountSystem is false a single account is
// built from the environment credentials (legacy path). When true the
// credentials file (cfg.AccountsConfigFile) is loaded and each entry is
// registered for lazy initialization. New does not perform network I/O or token
// refreshes; accounts are initialized on first Pick.
func New(ctx context.Context, cfg *config.Config, hc *http.Client, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	m := &Manager{
		cfg:             cfg,
		hc:              hc,
		log:             log,
		byID:            map[string]*Account{},
		recoveryTimeout: time.Duration(cfg.AccountRecoveryTimeout) * time.Second,
		maxBackoff:      cfg.AccountMaxBackoffMultiplier,
		probRetry:       cfg.AccountProbabilisticRetry,
		accountCacheTTL: time.Duration(cfg.AccountCacheTTL) * time.Second,
		cacheTTL:        time.Duration(cfg.ModelCacheTTL) * time.Second,
		fallbackIDs:     fallbackModelIDs(cfg.FallbackModels),
	}

	if !cfg.AccountSystem {
		m.addAccount("default", auth.Options{
			RefreshToken:   cfg.RefreshToken,
			ProfileARN:     cfg.ProfileARN,
			Region:         cfg.Region,
			APIRegion:      cfg.APIRegion,
			CredsFile:      cfg.CredsFile,
			SQLiteDB:       cfg.CLIDBFile,
			SQLiteReadonly: cfg.SQLiteReadonly,
		})
		return m, nil
	}

	if err := m.loadAccounts(); err != nil {
		return nil, err
	}
	return m, nil
}

// fallbackModelIDs extracts the model IDs from the configured fallback list.
func fallbackModelIDs(models []config.FallbackModel) []string {
	ids := make([]string, 0, len(models))
	for _, fm := range models {
		if fm.ModelID != "" {
			ids = append(ids, fm.ModelID)
		}
	}
	return ids
}

// SetModelFetcher installs the function used to refresh accounts' model caches
// from upstream. It is optional: when nil, accounts retain the configured
// fallback model list. Call it once during wiring, before serving traffic.
func (m *Manager) SetModelFetcher(f ModelFetcher) {
	m.mu.Lock()
	m.fetch = f
	m.mu.Unlock()
}

// addAccount registers a new account unless one with the same id already exists.
// Callers during construction hold no lock (single-threaded); thereafter callers
// must hold m.mu.
func (m *Manager) addAccount(id string, opts auth.Options) {
	if _, ok := m.byID[id]; ok {
		return
	}
	a := &Account{id: id, authOpts: opts}
	m.accounts = append(m.accounts, a)
	m.byID[id] = a
	m.log.Debug("registered account", "id", id)
}

// loadAccounts reads and parses the credentials file, registering one account
// per (enabled) entry. Folder paths for json/sqlite entries are scanned and each
// valid credential file becomes its own account. It returns an error when the
// file cannot be read or parsed, or when no usable accounts result.
func (m *Manager) loadAccounts() error {
	path := expandUser(m.cfg.AccountsConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("account: read credentials file %q: %w", m.cfg.AccountsConfigFile, err)
	}
	var entries []credentialEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("account: parse credentials file %q: %w", m.cfg.AccountsConfigFile, err)
	}

	for _, e := range entries {
		if e.Enabled != nil && !*e.Enabled {
			continue
		}
		switch e.Type {
		case "refresh_token":
			if e.RefreshToken == "" {
				m.log.Warn("skipping refresh_token entry without refresh_token field")
				continue
			}
			id := "refresh_token_" + shortHash(e.RefreshToken)
			m.addAccount(id, auth.Options{
				RefreshToken:   e.RefreshToken,
				ProfileARN:     e.ProfileARN,
				Region:         e.Region,
				APIRegion:      e.APIRegion,
				SQLiteReadonly: m.cfg.SQLiteReadonly,
			})
		case "json", "sqlite":
			if e.Path == "" {
				m.log.Warn("skipping credential entry without path", "type", e.Type)
				continue
			}
			m.loadPathEntry(e)
		default:
			m.log.Warn("skipping credential entry with unknown type", "type", e.Type)
		}
	}

	if len(m.accounts) == 0 {
		return errors.New("account: no usable accounts in credentials file")
	}
	m.log.Info("loaded accounts", "count", len(m.accounts))
	return nil
}

// loadPathEntry registers account(s) for a json/sqlite entry, scanning folders
// and validating each candidate credential file.
func (m *Manager) loadPathEntry(e credentialEntry) {
	ep := expandUser(e.Path)
	info, err := os.Stat(ep)
	if err != nil {
		m.log.Warn("credential path not found", "path", e.Path)
		return
	}

	register := func(file string) {
		abs, err := filepath.Abs(file)
		if err != nil {
			abs = file
		}
		opts := auth.Options{
			ProfileARN:     e.ProfileARN,
			Region:         e.Region,
			APIRegion:      e.APIRegion,
			SQLiteReadonly: m.cfg.SQLiteReadonly,
		}
		if e.Type == "sqlite" {
			opts.SQLiteDB = abs
		} else {
			opts.CredsFile = abs
		}
		m.addAccount(abs, opts)
	}

	if !info.IsDir() {
		register(ep)
		return
	}

	m.log.Info("scanning folder for credentials", "path", e.Path)
	files, err := os.ReadDir(ep)
	if err != nil {
		m.log.Warn("failed to scan credentials folder", "path", e.Path, "error", err)
		return
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		full := filepath.Join(ep, f.Name())
		if !validCredentialFile(e.Type, full) {
			m.log.Warn("skipping invalid credentials file", "file", f.Name())
			continue
		}
		register(full)
	}
}

// validCredentialFile performs a lightweight validity check on a candidate
// credential file: JSON files must contain a refreshToken or clientId key;
// SQLite databases must contain an auth_kv table.
func validCredentialFile(credType, path string) bool {
	switch credType {
	case "json":
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			return false
		}
		_, hasToken := obj["refreshToken"]
		_, hasClient := obj["clientId"]
		return hasToken || hasClient
	case "sqlite":
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
		if err != nil {
			return false
		}
		defer db.Close()
		var name string
		err = db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name='auth_kv'").Scan(&name)
		return err == nil
	default:
		return false
	}
}

// Pick selects the active account using the sticky global index and the circuit
// breaker. With a single account the breaker is bypassed (the caller should see
// the real upstream error rather than a synthetic "unavailable"). With multiple
// accounts it iterates from the sticky index, skipping accounts in cooldown
// (exponential backoff) unless a probabilistic retry fires. Accounts are lazily
// initialized and their model caches refreshed on TTL expiry. It returns an
// error when no account can be made ready.
func (m *Manager) Pick(ctx context.Context) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.accounts) == 0 {
		return nil, errors.New("account: no accounts configured")
	}

	// Single account: bypass the circuit breaker entirely.
	if len(m.accounts) == 1 {
		a := m.accounts[0]
		if !a.initialized {
			if err := m.initAccount(ctx, a); err != nil {
				return nil, fmt.Errorf("account: initialize single account: %w", err)
			}
		}
		m.maybeRefreshModels(ctx, a)
		return a, nil
	}

	n := len(m.accounts)
	start := m.currentIndex
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		a := m.accounts[idx]

		if a.failures > 0 && a.inCooldown(m.recoveryTimeout, m.maxBackoff) {
			// Probabilistic retry: most of the time skip a broken account.
			if rand.Float64() > m.probRetry {
				continue
			}
			m.log.Info("probabilistic retry for broken account", "id", a.id)
		} else if a.failures > 0 {
			m.log.Info("half-open: recovery timeout passed", "id", a.id)
		}

		if !a.initialized {
			if err := m.initAccount(ctx, a); err != nil {
				m.log.Warn("account initialization failed", "id", a.id, "error", err)
				a.failures++
				a.lastFailureTime = time.Now()
				m.dirty = true
				continue
			}
		}

		m.maybeRefreshModels(ctx, a)
		return a, nil
	}

	return nil, errors.New("account: all accounts unavailable")
}

// inCooldown reports whether the account is still within its (exponentially
// backed-off) recovery window. Callers must hold m.mu.
func (a *Account) inCooldown(base time.Duration, maxMultiplier float64) bool {
	if a.failures <= 0 {
		return false
	}
	multiplier := math.Pow(2, float64(a.failures-1))
	if multiplier > maxMultiplier {
		multiplier = maxMultiplier
	}
	effective := time.Duration(float64(base) * multiplier)
	return time.Since(a.lastFailureTime) < effective
}

// initAccount lazily builds the auth manager, verifies credentials, and creates
// the model cache + resolver. Callers must hold m.mu.
func (m *Manager) initAccount(ctx context.Context, a *Account) error {
	opts := a.authOpts
	opts.HTTPClient = m.hc
	opts.Logger = m.log
	opts.ClientVersion = m.cfg.KiroClientVersion
	opts.RefreshThreshold = time.Duration(m.cfg.TokenRefreshThreshold) * time.Second

	am, err := auth.NewKiroAuthManager(ctx, opts)
	if err != nil {
		return err
	}
	// Verify the credentials are usable before considering the account ready.
	if _, err := am.GetAccessToken(ctx); err != nil {
		return err
	}

	cache := modelcache.New(m.fallbackIDs, m.cacheTTL, m.log)
	resolver := model.NewResolver(cache, m.cfg.HiddenModels, m.cfg.ModelAliases, m.cfg.HiddenFromList)

	a.auth = am
	a.cache = cache
	a.resolver = resolver
	a.initialized = true

	// Best-effort initial model fetch; on failure the fallback list is kept. This
	// is expected on Kiro regions that don't serve ListAvailableModels, so it is
	// logged at INFO — the built-in fallback models keep the proxy fully usable.
	if m.fetch != nil {
		if err := cache.Refresh(ctx, m.fetchClosure(a)); err != nil {
			m.log.Info("model catalog unavailable from Kiro; using built-in fallback models", "id", a.id, "error", err)
		}
	}
	a.modelsCachedAt = time.Now()
	m.dirty = true

	m.log.Info("initialized account", "id", a.id, "models", cache.Size())
	return nil
}

// maybeRefreshModels refreshes the account's model cache when its TTL has
// elapsed. Callers must hold m.mu.
func (m *Manager) maybeRefreshModels(ctx context.Context, a *Account) {
	if m.fetch == nil || a.cache == nil || a.modelsCachedAt.IsZero() {
		return
	}
	if time.Since(a.modelsCachedAt) <= m.accountCacheTTL {
		return
	}
	if err := a.cache.Refresh(ctx, m.fetchClosure(a)); err != nil {
		m.log.Info("model catalog refresh failed; keeping cached models", "id", a.id, "error", err)
		return
	}
	a.modelsCachedAt = time.Now()
	m.dirty = true
}

// fetchClosure adapts the manager's ModelFetcher to the cache's fetch signature.
func (m *Manager) fetchClosure(a *Account) func(context.Context) ([]model.CatalogModel, error) {
	return func(ctx context.Context) ([]model.CatalogModel, error) {
		return m.fetch(ctx, a)
	}
}

// RefreshModels forces a model-cache refresh for the account using the manager's
// configured fetcher. It is a no-op (returning nil) when no fetcher is set or
// the account is uninitialized.
func (m *Manager) RefreshModels(ctx context.Context, a *Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fetch == nil || a == nil || a.cache == nil {
		return nil
	}
	if err := a.cache.Refresh(ctx, m.fetchClosure(a)); err != nil {
		return err
	}
	a.modelsCachedAt = time.Now()
	m.dirty = true
	return nil
}

// RecordSuccess resets the account's failure counter, bumps its success
// statistics, and makes it the sticky account for subsequent Picks.
func (m *Manager) RecordSuccess(a *Account) {
	if a == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if a.failures > 0 {
		a.failures = 0
	}
	a.stats.TotalRequests++
	a.stats.SuccessfulRequests++
	m.dirty = true

	for i, acc := range m.accounts {
		if acc == a {
			if m.currentIndex != i {
				m.currentIndex = i
			}
			break
		}
	}
}

// RecordFailure updates the account's circuit-breaker and statistics for a
// failed request. The error is classified with auth.ClassifyError: only
// recoverable (account-level) failures advance the breaker; fatal (request-level)
// errors do not penalize the account. The special reason "INVALID_MODEL_ID" is
// treated as a model-discovery miss, not an account failure.
func (m *Manager) RecordFailure(a *Account, status int, reason string) {
	if a == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Model not available on this account: healthy account, not a failure.
	if reason == "INVALID_MODEL_ID" {
		a.stats.TotalRequests++
		m.dirty = true
		m.log.Warn("model not available on account", "id", a.id, "status", status, "reason", reason)
		return
	}

	if auth.ClassifyError(status, reason) == auth.ErrorTypeRecoverable {
		a.failures++
		a.lastFailureTime = time.Now()
		multiplier := math.Pow(2, float64(a.failures-1))
		if multiplier > m.maxBackoff {
			multiplier = m.maxBackoff
		}
		cooldown := time.Duration(float64(m.recoveryTimeout) * multiplier)
		m.log.Warn("account failure",
			"id", a.id, "failures", a.failures, "status", status, "reason", reason,
			"cooldown", cooldown.String())
	}

	a.stats.TotalRequests++
	a.stats.FailedRequests++
	m.dirty = true
	// The sticky index is intentionally not changed on failure; failover happens
	// via the circuit breaker on the next Pick.
}

// Accounts returns the registered accounts in their stable order. The slice is a
// copy; the *Account pointers are shared.
func (m *Manager) Accounts() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Account, len(m.accounts))
	copy(out, m.accounts)
	return out
}

// AllAvailableModels returns the sorted union of available models across all
// initialized accounts, each with the token limits its owning account can
// establish (live catalog first, then the offline table). Used by model-listing
// endpoints.
//
// When two accounts disagree about a model's limits — different regions can serve
// different catalogs — the larger window wins, since the account that serves the
// request is picked independently of this listing.
func (m *Manager) AllAvailableModels() []model.CatalogModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	caps := map[string]model.Capabilities{}
	for _, a := range m.accounts {
		if a.resolver == nil {
			continue
		}
		for _, id := range a.resolver.GetAvailableModels() {
			found, _ := a.resolver.Capabilities(id)
			if best, seen := caps[id]; seen && best.MaxInputTokens >= found.MaxInputTokens {
				continue
			}
			caps[id] = found
		}
	}
	out := make([]model.CatalogModel, 0, len(caps))
	for id, c := range caps {
		out = append(out, model.CatalogModel{ID: id, Capabilities: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- state persistence ---

// persistedState is the on-disk shape of the state file.
type persistedState struct {
	CurrentAccountIndex int                              `json:"current_account_index"`
	Accounts            map[string]persistedAccountState `json:"accounts"`
}

type persistedAccountState struct {
	Failures        int          `json:"failures"`
	LastFailureTime float64      `json:"last_failure_time"`
	ModelsCachedAt  float64      `json:"models_cached_at"`
	Stats           AccountStats `json:"stats"`
}

// LoadState reads the state file and merges runtime state into already-registered
// accounts. Unknown account IDs in the file are ignored, and a missing file is
// not an error.
func (m *Manager) LoadState() error {
	data, err := os.ReadFile(expandUser(m.cfg.AccountsStateFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("account: read state file: %w", err)
	}

	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("account: parse state file: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if st.CurrentAccountIndex >= 0 && st.CurrentAccountIndex < len(m.accounts) {
		m.currentIndex = st.CurrentAccountIndex
	}
	for id, ps := range st.Accounts {
		a, ok := m.byID[id]
		if !ok {
			continue
		}
		a.failures = ps.Failures
		a.lastFailureTime = unixToTime(ps.LastFailureTime)
		a.modelsCachedAt = unixToTime(ps.ModelsCachedAt)
		a.stats = ps.Stats
	}
	m.log.Info("loaded account state", "accounts", len(st.Accounts))
	return nil
}

// SaveState writes the current runtime state to the state file atomically
// (temp file + rename). It is safe to call concurrently with request handling.
func (m *Manager) SaveState() error {
	m.mu.Lock()
	st := persistedState{
		CurrentAccountIndex: m.currentIndex,
		Accounts:            make(map[string]persistedAccountState, len(m.accounts)),
	}
	for _, a := range m.accounts {
		st.Accounts[a.id] = persistedAccountState{
			Failures:        a.failures,
			LastFailureTime: timeToUnix(a.lastFailureTime),
			ModelsCachedAt:  timeToUnix(a.modelsCachedAt),
			Stats:           a.stats,
		}
	}
	m.dirty = false
	m.mu.Unlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("account: marshal state: %w", err)
	}

	path := expandUser(m.cfg.AccountsStateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("account: write state temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("account: rename state file: %w", err)
	}
	m.log.Debug("saved account state")
	return nil
}

// StartStatePersistence runs a background loop that saves state every
// cfg.StateSaveIntervalSeconds when there are unsaved changes. It blocks until
// ctx is cancelled, performing a final save before returning. Run it in its own
// goroutine.
func (m *Manager) StartStatePersistence(ctx context.Context) {
	interval := time.Duration(m.cfg.StateSaveIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if m.isDirty() {
				if err := m.SaveState(); err != nil {
					m.log.Warn("final state save failed", "error", err)
				}
			}
			return
		case <-ticker.C:
			if !m.isDirty() {
				continue
			}
			if err := m.SaveState(); err != nil {
				m.log.Warn("periodic state save failed", "error", err)
			}
		}
	}
}

// isDirty reports whether there is unsaved state.
func (m *Manager) isDirty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirty
}

// --- helpers ---

// shortHash returns the first 16 hex chars of the SHA-256 of s. Used to derive a
// stable account id from an inline refresh token.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// expandUser expands a leading ~ to the user's home directory.
func expandUser(path string) string {
	if path == "~" || (len(path) >= 2 && path[0] == '~' && (path[1] == '/' || path[1] == filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// unixToTime converts fractional Unix seconds to a time.Time (zero for <= 0).
func unixToTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

// timeToUnix converts a time.Time to fractional Unix seconds (0 for the zero time).
func timeToUnix(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}
