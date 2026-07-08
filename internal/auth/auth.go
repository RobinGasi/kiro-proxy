package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go, cgo-free SQLite driver (driver name "sqlite")

	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/util"
)

// AuthType is the authentication mechanism backing a manager.
type AuthType string

const (
	// AuthTypeKiroDesktop uses the Kiro Desktop Auth refresh endpoint
	// (https://prod.{region}.auth.desktop.kiro.dev/refreshToken).
	AuthTypeKiroDesktop AuthType = "kiro_desktop"
	// AuthTypeAWSSSOOIDC uses the AWS SSO OIDC CreateToken endpoint
	// (https://oidc.{region}.amazonaws.com/token), as used by kiro-cli,
	// IAM Identity Center and Builder ID.
	AuthTypeAWSSSOOIDC AuthType = "aws_sso_oidc"
)

// sqliteTokenKeys are the auth_kv token keys, searched in priority order.
var sqliteTokenKeys = []string{
	"kirocli:social:token",     // Social login (Google, GitHub, Microsoft, ...)
	"kirocli:odic:token",       // AWS SSO OIDC (kiro-cli corporate)
	"codewhisperer:odic:token", // Legacy AWS SSO OIDC
}

// sqliteRegistrationKeys are the auth_kv device-registration keys (AWS SSO OIDC).
var sqliteRegistrationKeys = []string{
	"kirocli:odic:device-registration",
	"codewhisperer:odic:device-registration",
}

// regionRe validates an AWS region string (e.g. us-east-1, eu-central-1).
var regionRe = regexp.MustCompile(`^[a-z]+-[a-z]+-\d+$`)

const defaultRegion = config.DefaultRegion

// Options configures a KiroAuthManager. Only the credential source(s) relevant
// to the chosen auth type need to be set; the rest are auto-detected.
type Options struct {
	// RefreshToken is the bare refresh token (e.g. from the REFRESH_TOKEN env).
	RefreshToken string
	// ProfileARN is the AWS CodeWhisperer profile ARN. It may be overridden by
	// values discovered in the SQLite/JSON credential sources.
	ProfileARN string
	// Region is the default/SSO region (KIRO_REGION). Defaults to us-east-1.
	Region string
	// APIRegion is an explicit per-account Q API region override (highest
	// priority in region resolution).
	APIRegion string
	// CredsFile is a path to a JSON credentials file (Kiro Desktop or, with a
	// clientIdHash, Enterprise Kiro IDE).
	CredsFile string
	// SQLiteDB is a path to a kiro-cli SQLite database. Takes priority over
	// CredsFile when both are set.
	SQLiteDB string
	// ClientID / ClientSecret are AWS SSO OIDC OAuth credentials. When both are
	// present (directly or via a credential source) the manager uses AWS SSO OIDC.
	ClientID     string
	ClientSecret string

	// SQLiteReadonly, when true, disables write-back to the SQLite database.
	// This (combined with reload-before-refresh) avoids the refresh-token
	// rotation race on a database shared with a running kiro-cli.
	SQLiteReadonly bool
	// ClientVersion is the spoofed Kiro IDE version used in the User-Agent.
	// Defaults to config's KIRO_CLIENT_VERSION default when empty.
	ClientVersion string
	// RefreshThreshold is how long before expiry a token is considered
	// "expiring soon". Defaults to 600s when zero.
	RefreshThreshold time.Duration
	// HTTPClient is used for refresh requests. Defaults to a 30s-timeout client.
	HTTPClient *http.Client
	// Logger receives diagnostic logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// KiroAuthManager manages the access-token lifecycle for a single Kiro account:
// loading credentials, refreshing on expiry, and exposing the resolved
// endpoints. It is safe for concurrent use; concurrent refreshes are coalesced
// behind a mutex.
type KiroAuthManager struct {
	mu sync.Mutex

	// Credential state (guarded by mu after construction).
	refreshToken string
	accessToken  string
	profileARN   string
	expiresAt    time.Time

	// AWS SSO OIDC fields.
	clientID              string
	clientSecret          string
	scopes                []string
	registrationExpiresAt time.Time // bug #10: client-registration expiry

	// Regions. ssoRegion is used for OIDC/refresh endpoints; apiRegion for the
	// runtime Q API endpoints; region is the configured default.
	region            string
	ssoRegion         string
	detectedAPIRegion string
	apiRegion         string

	// Source bookkeeping.
	credsFile      string
	sqliteDB       string
	sqliteTokenKey string // which auth_kv key we loaded the token from

	// Resolved endpoints.
	desktopRefreshURL string
	apiHost           string
	qHost             string

	// Behavior.
	authType       AuthType
	sqliteReadonly bool
	clientVersion  string
	fingerprint    string
	refreshThresh  time.Duration
	http           *http.Client
	log            *slog.Logger
}

// NewKiroAuthManager builds a manager, loading any configured credential
// sources and resolving the SSO/API regions. It performs local file/SQLite I/O,
// hence the context. It validates region configuration (bug #6) and surfaces
// client-registration expiry (bug #10).
func NewKiroAuthManager(ctx context.Context, opts Options) (*KiroAuthManager, error) {
	region := strings.TrimSpace(opts.Region)
	if region == "" {
		region = defaultRegion
	}
	clientVersion := opts.ClientVersion
	if clientVersion == "" {
		clientVersion = config.DefaultKiroClientVersion
	}
	thresh := opts.RefreshThreshold
	if thresh <= 0 {
		thresh = config.DefaultTokenRefreshThreshold * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	m := &KiroAuthManager{
		refreshToken:   opts.RefreshToken,
		profileARN:     opts.ProfileARN,
		region:         region,
		clientID:       opts.ClientID,
		clientSecret:   opts.ClientSecret,
		credsFile:      opts.CredsFile,
		sqliteDB:       opts.SQLiteDB,
		authType:       AuthTypeKiroDesktop,
		sqliteReadonly: opts.SQLiteReadonly,
		clientVersion:  clientVersion,
		fingerprint:    util.MachineFingerprint(),
		refreshThresh:  thresh,
		http:           httpClient,
		log:            logger,
	}

	// Load credentials: SQLite takes priority over a JSON file.
	if m.sqliteDB != "" {
		m.loadCredentialsFromSQLite(ctx, m.sqliteDB)
	} else if m.credsFile != "" {
		m.loadCredentialsFromFile(m.credsFile)
	}

	m.detectAuthType()
	m.resolveRegionsAndEndpoints(opts.APIRegion)
	return m, nil
}

// detectAuthType selects AWS SSO OIDC when OAuth client credentials are present.
func (m *KiroAuthManager) detectAuthType() {
	if m.clientID != "" && m.clientSecret != "" {
		m.authType = AuthTypeAWSSSOOIDC
	} else {
		m.authType = AuthTypeKiroDesktop
	}
	m.log.Info("auth type detected", "type", string(m.authType))
}

// resolveRegionsAndEndpoints applies the region priority hierarchy and computes
// the endpoint URLs. apiRegionOverride is the explicit per-account override.
//
// Priority: explicit api_region > KIRO_API_REGION env > detected (ARN/JSON) >
// SSO region > default region. Bug #6: any resolved region that does not look
// like a valid AWS region falls back to us-east-1 instead of producing an
// unreachable host.
func (m *KiroAuthManager) resolveRegionsAndEndpoints(apiRegionOverride string) {
	var finalAPIRegion string
	switch {
	case strings.TrimSpace(apiRegionOverride) != "":
		finalAPIRegion = apiRegionOverride
		m.log.Info("API region from account config", "region", finalAPIRegion)
	case strings.TrimSpace(os.Getenv("KIRO_API_REGION")) != "":
		finalAPIRegion = os.Getenv("KIRO_API_REGION")
		m.log.Info("API region from KIRO_API_REGION env", "region", finalAPIRegion)
	case m.detectedAPIRegion != "":
		finalAPIRegion = m.detectedAPIRegion
		m.log.Info("API region auto-detected from credentials", "region", finalAPIRegion)
	case m.ssoRegion != "":
		finalAPIRegion = m.ssoRegion
		m.log.Info("API region using SSO region", "region", finalAPIRegion)
	default:
		finalAPIRegion = m.region
		m.log.Info("API region using default", "region", finalAPIRegion)
	}

	// Bug #6: reject misconfigured/unreachable regions.
	if !regionRe.MatchString(finalAPIRegion) {
		m.log.Warn("configured API region looks invalid; falling back",
			"configured", finalAPIRegion, "fallback", defaultRegion)
		finalAPIRegion = defaultRegion
	}
	m.apiRegion = finalAPIRegion

	ssoRegionForOIDC := m.ssoRegion
	if ssoRegionForOIDC == "" {
		ssoRegionForOIDC = m.region
	}
	if !regionRe.MatchString(ssoRegionForOIDC) {
		m.log.Warn("configured SSO region looks invalid; falling back",
			"configured", ssoRegionForOIDC, "fallback", defaultRegion)
		ssoRegionForOIDC = defaultRegion
	}

	m.desktopRefreshURL = config.GetKiroRefreshURL(ssoRegionForOIDC)
	m.apiHost = config.GetKiroAPIHost(finalAPIRegion)
	m.qHost = config.GetKiroQHost(finalAPIRegion)

	m.log.Info("auth manager initialized",
		"auth_type", string(m.authType),
		"sso_region", ssoRegionForOIDC,
		"api_region", finalAPIRegion,
		"api_host", m.apiHost,
		"q_host", m.qHost,
	)
}

// oidcRefreshURL returns the AWS SSO OIDC token endpoint for the current SSO
// region (recomputed at refresh time because a SQLite reload may change it).
func (m *KiroAuthManager) oidcRefreshURL() string {
	r := m.ssoRegion
	if r == "" {
		r = m.region
	}
	if !regionRe.MatchString(r) {
		r = defaultRegion
	}
	return config.GetAWSSSOOIDCURL(r)
}

// --- credential loading ---

// loadCredentialsFromSQLite loads tokens, device registration and the profile
// ARN from a kiro-cli SQLite database. Missing rows are ignored; the manager
// keeps whatever it already had. Mutates state; callers must hold mu (or run
// during construction).
func (m *KiroAuthManager) loadCredentialsFromSQLite(ctx context.Context, dbPath string) {
	path := expandUser(dbPath)
	if _, err := os.Stat(path); err != nil {
		m.log.Warn("SQLite database not found", "path", dbPath)
		return
	}

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		m.log.Error("SQLite open failed", "err", err)
		return
	}
	defer db.Close()

	// Token row (priority order).
	for _, key := range sqliteTokenKeys {
		var value string
		err := db.QueryRowContext(ctx, "SELECT value FROM auth_kv WHERE key = ?", key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			m.log.Debug("SQLite token query failed", "key", key, "err", err)
			continue
		}
		m.sqliteTokenKey = key
		m.applyTokenJSON(value)
		break
	}

	// Device registration (client_id / client_secret / registration expiry).
	for _, key := range sqliteRegistrationKeys {
		var value string
		err := db.QueryRowContext(ctx, "SELECT value FROM auth_kv WHERE key = ?", key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			m.log.Debug("SQLite registration query failed", "key", key, "err", err)
			continue
		}
		m.applyRegistrationJSON(value)
		break
	}

	// Profile ARN + API region from the state table.
	var profileValue string
	err = db.QueryRowContext(ctx,
		"SELECT value FROM state WHERE key = 'api.codewhisperer.profile'").Scan(&profileValue)
	if err == nil {
		m.applyProfileJSON(profileValue)
	} else if !errors.Is(err, sql.ErrNoRows) {
		m.log.Debug("SQLite state query failed", "err", err)
	}

	m.log.Info("credentials loaded from SQLite", "path", dbPath)
}

// applyTokenJSON merges a kiro-cli auth_kv token value (snake_case JSON).
func (m *KiroAuthManager) applyTokenJSON(value string) {
	var t struct {
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		ProfileARN   string   `json:"profile_arn"`
		Region       string   `json:"region"`
		Scopes       []string `json:"scopes"`
		ExpiresAt    string   `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(value), &t); err != nil {
		m.log.Warn("failed to parse SQLite token JSON", "err", err)
		return
	}
	if t.AccessToken != "" {
		m.accessToken = t.AccessToken
	}
	if t.RefreshToken != "" {
		m.refreshToken = t.RefreshToken
	}
	if t.ProfileARN != "" {
		m.profileARN = t.ProfileARN
	}
	if t.Region != "" {
		m.ssoRegion = t.Region
	}
	if len(t.Scopes) > 0 {
		m.scopes = t.Scopes
	}
	if t.ExpiresAt != "" {
		if exp, ok := parseTime(t.ExpiresAt); ok {
			m.expiresAt = exp
		} else {
			m.log.Warn("failed to parse expires_at from SQLite", "value", t.ExpiresAt)
		}
	}
}

// applyRegistrationJSON merges a device-registration value (snake_case JSON).
func (m *KiroAuthManager) applyRegistrationJSON(value string) {
	var r struct {
		ClientID              string `json:"client_id"`
		ClientSecret          string `json:"client_secret"`
		Region                string `json:"region"`
		RegistrationExpiresAt string `json:"registration_expires_at"`
	}
	if err := json.Unmarshal([]byte(value), &r); err != nil {
		m.log.Warn("failed to parse SQLite registration JSON", "err", err)
		return
	}
	if r.ClientID != "" {
		m.clientID = r.ClientID
	}
	if r.ClientSecret != "" {
		m.clientSecret = r.ClientSecret
	}
	if r.Region != "" && m.ssoRegion == "" {
		m.ssoRegion = r.Region
	}
	if r.RegistrationExpiresAt != "" {
		if exp, ok := parseTime(r.RegistrationExpiresAt); ok {
			m.registrationExpiresAt = exp
		}
	}
}

// applyProfileJSON merges the state-table profile (arn → profile ARN + region).
func (m *KiroAuthManager) applyProfileJSON(value string) {
	var p struct {
		ARN string `json:"arn"`
	}
	if err := json.Unmarshal([]byte(value), &p); err != nil {
		m.log.Debug("failed to parse profile JSON from state table", "err", err)
		return
	}
	if p.ARN == "" {
		return
	}
	if m.profileARN == "" {
		m.profileARN = p.ARN
	}
	// ARN format: arn:aws:codewhisperer:REGION:account:profile/id
	parts := strings.Split(p.ARN, ":")
	if len(parts) >= 4 && regionRe.MatchString(parts[3]) {
		m.detectedAPIRegion = parts[3]
		m.log.Info("API region auto-detected from profile ARN", "region", parts[3])
	}
}

// loadCredentialsFromFile loads a JSON credentials file (Kiro Desktop or, via
// clientIdHash, Enterprise Kiro IDE). Mutates state; callers must hold mu (or
// run during construction).
func (m *KiroAuthManager) loadCredentialsFromFile(filePath string) {
	path := expandUser(filePath)
	if _, err := os.Stat(path); err != nil {
		m.log.Warn("credentials file not found", "path", filePath)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		m.log.Error("failed to read credentials file", "err", err)
		return
	}
	var d struct {
		RefreshToken          string `json:"refreshToken"`
		AccessToken           string `json:"accessToken"`
		ProfileARN            string `json:"profileArn"`
		Region                string `json:"region"`
		ClientIDHash          string `json:"clientIdHash"`
		ClientID              string `json:"clientId"`
		ClientSecret          string `json:"clientSecret"`
		ExpiresAt             string `json:"expiresAt"`
		RegistrationExpiresAt string `json:"registrationExpiresAt"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		m.log.Error("failed to parse credentials file", "err", err)
		return
	}

	if d.RefreshToken != "" {
		m.refreshToken = d.RefreshToken
	}
	if d.AccessToken != "" {
		m.accessToken = d.AccessToken
	}
	if d.ProfileARN != "" {
		m.profileARN = d.ProfileARN
	}
	if d.Region != "" {
		m.ssoRegion = d.Region
		// Bug #6: only trust a JSON region for the API endpoint if it is a
		// well-formed AWS region; otherwise leave detection unset so the
		// resolver falls back to us-east-1.
		if regionRe.MatchString(d.Region) {
			m.detectedAPIRegion = d.Region
		} else {
			m.log.Warn("ignoring malformed region in credentials file", "region", d.Region)
		}
	}
	if d.ClientIDHash != "" {
		m.loadEnterpriseDeviceRegistration(d.ClientIDHash)
	}
	if d.ClientID != "" {
		m.clientID = d.ClientID
	}
	if d.ClientSecret != "" {
		m.clientSecret = d.ClientSecret
	}
	if d.RegistrationExpiresAt != "" {
		if exp, ok := parseTime(d.RegistrationExpiresAt); ok {
			m.registrationExpiresAt = exp
		}
	}
	if d.ExpiresAt != "" {
		if exp, ok := parseTime(d.ExpiresAt); ok {
			m.expiresAt = exp
		} else {
			m.log.Warn("failed to parse expiresAt", "value", d.ExpiresAt)
		}
	}
	m.log.Info("credentials loaded from file", "path", filePath)
}

// loadEnterpriseDeviceRegistration loads clientId/clientSecret (and the
// registration expiry) from ~/.aws/sso/cache/{clientIdHash}.json.
func (m *KiroAuthManager) loadEnterpriseDeviceRegistration(clientIDHash string) {
	home, err := os.UserHomeDir()
	if err != nil {
		m.log.Error("cannot resolve home directory for device registration", "err", err)
		return
	}
	path := filepath.Join(home, ".aws", "sso", "cache", clientIDHash+".json")
	if _, err := os.Stat(path); err != nil {
		m.log.Warn("enterprise device registration file not found", "path", path)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		m.log.Error("failed to read device registration file", "err", err)
		return
	}
	var d struct {
		ClientID              string `json:"clientId"`
		ClientSecret          string `json:"clientSecret"`
		Region                string `json:"region"`
		RegistrationExpiresAt string `json:"registrationExpiresAt"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		m.log.Error("failed to parse device registration file", "err", err)
		return
	}
	if d.ClientID != "" {
		m.clientID = d.ClientID
	}
	if d.ClientSecret != "" {
		m.clientSecret = d.ClientSecret
	}
	if d.Region != "" && m.ssoRegion == "" {
		m.ssoRegion = d.Region
	}
	if d.RegistrationExpiresAt != "" {
		if exp, ok := parseTime(d.RegistrationExpiresAt); ok {
			m.registrationExpiresAt = exp
		}
	}
	m.log.Info("enterprise device registration loaded", "path", path)
}

// --- credential saving ---

// saveCredentialsToFile persists refreshed tokens back to the JSON credentials
// file, preserving any other fields. Errors are logged, never propagated.
func (m *KiroAuthManager) saveCredentialsToFile() {
	if m.credsFile == "" {
		return
	}
	path := expandUser(m.credsFile)

	existing := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &existing); err != nil {
			m.log.Warn("failed to parse existing credentials file; rewriting", "err", err)
			existing = map[string]any{}
		}
	}

	existing["accessToken"] = m.accessToken
	existing["refreshToken"] = m.refreshToken
	if !m.expiresAt.IsZero() {
		existing["expiresAt"] = m.expiresAt.Format(time.RFC3339Nano)
	}
	if m.profileARN != "" {
		existing["profileArn"] = m.profileARN
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		m.log.Error("failed to encode credentials", "err", err)
		return
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		m.log.Error("failed to write credentials file", "err", err)
		return
	}
	m.log.Debug("credentials saved to file", "path", m.credsFile)
}

// saveCredentialsToSQLite persists refreshed tokens back to the kiro-cli SQLite
// database using read-merge-write (preserving unknown fields).
//
// Bug #7: write-back is skipped entirely when SQLiteReadonly is set, and if the
// database turns out to be read-only or locked at runtime the manager degrades
// to read-only mode rather than corrupting a database shared with a running
// kiro-cli. Combined with reload-before-refresh in GetAccessToken (which avoids
// refreshing — and therefore rotating the refresh token — whenever kiro-cli has
// already produced a fresh token), this prevents the shared-SQLite rotation race.
func (m *KiroAuthManager) saveCredentialsToSQLite(ctx context.Context) {
	if m.sqliteDB == "" {
		return
	}
	if m.sqliteReadonly {
		m.log.Debug("SQLite write-back disabled (read-only mode)")
		return
	}
	path := expandUser(m.sqliteDB)
	if _, err := os.Stat(path); err != nil {
		m.log.Warn("SQLite database not found for writing", "path", m.sqliteDB)
		return
	}

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		m.log.Error("SQLite open for write failed", "err", err)
		return
	}
	defer db.Close()

	keys := make([]string, 0, len(sqliteTokenKeys)+1)
	if m.sqliteTokenKey != "" {
		keys = append(keys, m.sqliteTokenKey)
	}
	keys = append(keys, sqliteTokenKeys...)

	for _, key := range keys {
		ok, err := m.trySaveToKey(ctx, db, key)
		if err != nil {
			if isReadonlyErr(err) {
				// Shared/read-only database — stop trying and stay read-only.
				m.log.Warn("SQLite is read-only/locked; disabling write-back", "err", err)
				m.sqliteReadonly = true
				return
			}
			m.log.Debug("SQLite save attempt failed", "key", key, "err", err)
			continue
		}
		if ok {
			m.log.Debug("credentials saved to SQLite (merged)", "key", key)
			return
		}
	}
	m.log.Warn("failed to save credentials to SQLite: no matching keys found")
}

// trySaveToKey reads, merges and writes back one auth_kv token row. It returns
// (true, nil) when a row was updated, (false, nil) when the key is absent, and
// a non-nil error on a SQLite failure.
func (m *KiroAuthManager) trySaveToKey(ctx context.Context, db *sql.DB, key string) (bool, error) {
	var value string
	err := db.QueryRowContext(ctx, "SELECT value FROM auth_kv WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	existing := map[string]any{}
	if err := json.Unmarshal([]byte(value), &existing); err != nil {
		m.log.Warn("failed to parse JSON for SQLite key; skipping", "key", key, "err", err)
		return false, nil
	}

	existing["access_token"] = m.accessToken
	existing["refresh_token"] = m.refreshToken
	if m.expiresAt.IsZero() {
		existing["expires_at"] = nil
	} else {
		existing["expires_at"] = m.expiresAt.Format(time.RFC3339Nano)
	}
	if m.ssoRegion != "" {
		existing["region"] = m.ssoRegion
	} else {
		existing["region"] = m.region
	}
	if len(m.scopes) > 0 {
		existing["scopes"] = m.scopes
	}

	merged, err := json.Marshal(existing)
	if err != nil {
		return false, err
	}
	res, err := db.ExecContext(ctx, "UPDATE auth_kv SET value = ? WHERE key = ?", string(merged), key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- expiry helpers (callers must hold mu) ---

// tokenExpiresWithin reports whether the token expires within d (or has no known
// expiry). isTokenExpiringSoon uses the refresh threshold; isTokenExpired uses 0.
func (m *KiroAuthManager) tokenExpiresWithin(d time.Duration) bool {
	return m.expiresAt.IsZero() || !m.expiresAt.After(time.Now().Add(d))
}

func (m *KiroAuthManager) isTokenExpiringSoon() bool { return m.tokenExpiresWithin(m.refreshThresh) }
func (m *KiroAuthManager) isTokenExpired() bool      { return m.tokenExpiresWithin(0) }

// --- public token API ---

// GetAccessToken returns a valid access token, refreshing if necessary. It is
// safe for concurrent use; concurrent refreshes are coalesced by the mutex.
//
// For SQLite-backed accounts it reloads credentials before refreshing (so a
// fresh token written by kiro-cli is used without a refresh), and falls back to
// graceful degradation — serving a still-valid access token — when a refresh
// fails because the stored refresh token or client registration is stale.
func (m *KiroAuthManager) GetAccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.accessToken != "" && !m.isTokenExpiringSoon() {
		return m.accessToken, nil
	}

	// Bug #7: reload-before-refresh on shared SQLite databases. If kiro-cli has
	// already produced a fresh token, use it without refreshing (no rotation).
	if m.sqliteDB != "" && m.isTokenExpiringSoon() {
		prevAccess, prevRefresh, prevExpiry := m.accessToken, m.refreshToken, m.expiresAt
		m.loadCredentialsFromSQLite(ctx, m.sqliteDB)
		if m.accessToken != "" && !m.isTokenExpiringSoon() {
			m.log.Debug("SQLite reload provided a fresh token; no refresh needed")
			return m.accessToken, nil
		}
		// Don't let a staler on-disk token clobber a newer in-memory one.
		if !prevExpiry.IsZero() && (m.expiresAt.IsZero() || m.expiresAt.Before(prevExpiry)) {
			m.accessToken, m.refreshToken, m.expiresAt = prevAccess, prevRefresh, prevExpiry
		}
	}

	if err := m.refreshTokenRequest(ctx); err != nil {
		var hse *httpStatusError
		is400 := errors.As(err, &hse) && hse.statusCode == http.StatusBadRequest
		if errors.Is(err, ErrReLoginRequired) || (is400 && m.sqliteDB != "") {
			// Graceful degradation: keep serving a still-valid access token.
			if m.accessToken != "" && !m.isTokenExpired() {
				m.log.Warn("token refresh failed; serving existing access token until it expires; re-login when convenient")
				return m.accessToken, nil
			}
			return "", fmt.Errorf("%w: token expired and refresh failed; please re-login (e.g. run 'kiro-cli login')", ErrReLoginRequired)
		}
		return "", err
	}

	if m.accessToken == "" {
		return "", errors.New("failed to obtain access token")
	}
	return m.accessToken, nil
}

// ForceRefresh unconditionally refreshes the token and returns the new value.
// Used after a 403 from the API.
func (m *KiroAuthManager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshTokenRequest(ctx); err != nil {
		return "", err
	}
	if m.accessToken == "" {
		return "", errors.New("failed to obtain access token")
	}
	return m.accessToken, nil
}

// refreshTokenRequest routes to the appropriate refresh method. Callers hold mu.
func (m *KiroAuthManager) refreshTokenRequest(ctx context.Context) error {
	if m.authType == AuthTypeAWSSSOOIDC {
		return m.refreshAWSSSOOIDC(ctx)
	}
	return m.refreshKiroDesktop(ctx)
}

// refreshKiroDesktop refreshes via the Kiro Desktop Auth endpoint. Callers hold mu.
func (m *KiroAuthManager) refreshKiroDesktop(ctx context.Context) error {
	if m.refreshToken == "" {
		return errors.New("refresh token is not set")
	}
	m.log.Info("refreshing token via Kiro Desktop Auth")

	payload := map[string]string{"refreshToken": m.refreshToken}
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   fmt.Sprintf("KiroIDE-%s-%s", m.clientVersion, m.fingerprint),
	}

	body, status, err := m.postJSON(ctx, m.desktopRefreshURL, payload, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		m.log.Error("Kiro Desktop refresh failed", "status", status, "body", util.Truncate(string(body), 500))
		return &httpStatusError{statusCode: status, body: string(body), url: m.desktopRefreshURL}
	}

	var r struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    *int   `json:"expiresIn"`
		ProfileARN   string `json:"profileArn"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("kiro desktop: invalid refresh response: %w", err)
	}
	if r.AccessToken == "" {
		return fmt.Errorf("kiro desktop: response does not contain accessToken: %s", util.Truncate(string(body), 300))
	}

	m.accessToken = r.AccessToken
	if r.RefreshToken != "" {
		m.refreshToken = r.RefreshToken
	}
	if r.ProfileARN != "" {
		m.profileARN = r.ProfileARN
	}
	m.expiresAt = time.Now().UTC().Truncate(time.Second).Add(time.Duration(expiresInOrDefault(r.ExpiresIn)-60) * time.Second)
	m.log.Info("token refreshed via Kiro Desktop Auth", "expires_at", m.expiresAt.Format(time.RFC3339))

	m.persist(ctx)
	return nil
}

// refreshAWSSSOOIDC refreshes via AWS SSO OIDC, retrying once after reloading
// from SQLite if the first attempt fails with HTTP 400 (a stale token after a
// kiro-cli re-login). Callers hold mu.
func (m *KiroAuthManager) refreshAWSSSOOIDC(ctx context.Context) error {
	err := m.doAWSSSOOIDCRefresh(ctx)
	if err == nil {
		return nil
	}
	var hse *httpStatusError
	if errors.As(err, &hse) && hse.statusCode == http.StatusBadRequest && m.sqliteDB != "" {
		m.log.Warn("AWS SSO OIDC refresh failed with 400; reloading credentials from SQLite and retrying")
		m.loadCredentialsFromSQLite(ctx, m.sqliteDB)
		return m.doAWSSSOOIDCRefresh(ctx)
	}
	return err
}

// doAWSSSOOIDCRefresh performs a single AWS SSO OIDC CreateToken refresh.
// Callers hold mu.
func (m *KiroAuthManager) doAWSSSOOIDCRefresh(ctx context.Context) error {
	if m.refreshToken == "" {
		return errors.New("refresh token is not set")
	}
	if m.clientID == "" {
		return errors.New("client ID is not set (required for AWS SSO OIDC)")
	}
	if m.clientSecret == "" {
		return errors.New("client secret is not set (required for AWS SSO OIDC)")
	}

	// Bug #10: a refresh is impossible once the client registration has expired.
	if !m.registrationExpiresAt.IsZero() && !m.registrationExpiresAt.After(time.Now()) {
		return fmt.Errorf("%w: AWS SSO client registration expired at %s; please re-login (e.g. run 'kiro-cli login')",
			ErrReLoginRequired, m.registrationExpiresAt.Format(time.RFC3339))
	}

	m.log.Info("refreshing token via AWS SSO OIDC")

	url := m.oidcRefreshURL()
	// AWS SSO OIDC CreateToken requires a JSON body with camelCase keys (despite
	// documentation suggesting form-encoded, the API uses JSON).
	payload := map[string]string{
		"grantType":    "refresh_token",
		"clientId":     m.clientID,
		"clientSecret": m.clientSecret,
		"refreshToken": m.refreshToken,
	}
	headers := map[string]string{"Content-Type": "application/json"}

	body, status, err := m.postJSON(ctx, url, payload, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		m.log.Error("AWS SSO OIDC refresh failed", "status", status, "body", util.Truncate(string(body), 500))
		return &httpStatusError{statusCode: status, body: string(body), url: url}
	}

	var r struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    *int   `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("aws sso oidc: invalid refresh response: %w", err)
	}
	if r.AccessToken == "" {
		return fmt.Errorf("aws sso oidc: response does not contain accessToken: %s", util.Truncate(string(body), 300))
	}

	m.accessToken = r.AccessToken
	if r.RefreshToken != "" {
		m.refreshToken = r.RefreshToken
	}
	m.expiresAt = time.Now().UTC().Add(time.Duration(expiresInOrDefault(r.ExpiresIn)-60) * time.Second)
	m.log.Info("token refreshed via AWS SSO OIDC", "expires_at", m.expiresAt.Format(time.RFC3339))

	m.persist(ctx)
	return nil
}

// persist writes credentials back to the configured store. Callers hold mu.
func (m *KiroAuthManager) persist(ctx context.Context) {
	if m.sqliteDB != "" {
		m.saveCredentialsToSQLite(ctx)
	} else {
		m.saveCredentialsToFile()
	}
}

// postJSON POSTs a JSON payload and returns the response body and status code.
func (m *KiroAuthManager) postJSON(ctx context.Context, url string, payload any, headers map[string]string) ([]byte, int, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// --- accessors ---

// ProfileARN returns the AWS CodeWhisperer profile ARN (may be empty). Callers
// attach it on every request (handles bug #5).
func (m *KiroAuthManager) ProfileARN() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.profileARN
}

// Region returns the resolved Q API region used for the runtime endpoints.
func (m *KiroAuthManager) Region() string { return m.apiRegion }

// APIHost returns the runtime host for GenerateAssistantResponse.
func (m *KiroAuthManager) APIHost() string { return m.apiHost }

// QHost returns the runtime host for ListAvailableModels.
func (m *KiroAuthManager) QHost() string { return m.qHost }

// Fingerprint returns the stable machine fingerprint used in request headers.
func (m *KiroAuthManager) Fingerprint() string { return m.fingerprint }

// AuthType returns the authentication mechanism ("kiro_desktop" or "aws_sso_oidc").
func (m *KiroAuthManager) AuthType() string { return string(m.authType) }

// --- internal helpers ---

// httpStatusError is a non-2xx HTTP response from a refresh endpoint.
type httpStatusError struct {
	statusCode int
	body       string
	url        string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("kiro auth: HTTP %d from %s: %s", e.statusCode, e.url, util.Truncate(e.body, 500))
}

// expiresInOrDefault returns the response's expiresIn or 3600 when absent.
func expiresInOrDefault(v *int) int {
	if v == nil {
		return 3600
	}
	return *v
}

// expandUser expands a leading ~ to the user's home directory.
func expandUser(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// sqliteDSN builds a modernc.org/sqlite DSN with a busy timeout so reads/writes
// tolerate a database concurrently held by kiro-cli.
func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)"
}

// isReadonlyErr reports whether a SQLite error indicates a read-only or locked
// database (so write-back should be abandoned rather than retried).
func isReadonlyErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "readonly") ||
		strings.Contains(s, "read-only") ||
		strings.Contains(s, "read only") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "attempt to write")
}

// parseTime parses an RFC3339 / ISO-8601 timestamp, tolerating a trailing Z,
// fractional seconds with arbitrary precision, and missing zone (assumed UTC).
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
