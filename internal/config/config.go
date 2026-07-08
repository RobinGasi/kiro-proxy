// Package config centralizes all settings, constants, and mappings for
// kiro-proxy. It loads environment variables (including a .env file) and
// provides typed access to them.
package config

import (
	"bufio"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// URL templates for Kiro / AWS endpoints.
const (
	// KiroRefreshURLTemplate is the Kiro Desktop Auth token refresh endpoint.
	KiroRefreshURLTemplate = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	// AWSSSOOIDCURLTemplate is the AWS SSO OIDC CreateToken endpoint (kiro-cli).
	AWSSSOOIDCURLTemplate = "https://oidc.%s.amazonaws.com/token"
	// KiroAPIHostTemplate is the runtime host for GenerateAssistantResponse.
	KiroAPIHostTemplate = "https://runtime.%s.kiro.dev"
	// KiroQHostTemplate is the runtime host for ListAvailableModels.
	KiroQHostTemplate = "https://runtime.%s.kiro.dev"
)

// Application identity.
const (
	AppTitle       = "Kiro Proxy"
	AppDescription = "Proxy gateway for Kiro API (Amazon Q Developer / AWS CodeWhisperer). OpenAI and Anthropic compatible."
)

// AppVersion is the build version. It is a var (not a const) so the release
// build can override it at link time via
// -ldflags "-X github.com/RobinGasi/kiro-proxy/internal/config.AppVersion=<v>".
// The -X flag only works on string vars, so keep this a var.
var AppVersion = "0.0.0-dev"

// Server defaults.
const (
	DefaultServerHost = "0.0.0.0"
	// DefaultServerPort is an uncommon port chosen to avoid clashing with the
	// usual dev-server ports (3000/5000/8000/8080/…).
	DefaultServerPort = 8442
)

// Shared defaults exported so leaf packages reference a single source of truth
// instead of re-hardcoding these values.
const (
	DefaultRegion                = "us-east-1"
	DefaultKiroClientVersion     = "0.7.45"
	DefaultTokenRefreshThreshold = 600 // seconds
)

// FallbackModel is a single entry in the FallbackModels seed.
type FallbackModel struct {
	ModelID string `json:"modelId"`
}

// FallbackModels is used when ListAvailableModels is unreachable. It is an
// env-overridable seed (see Config.FallbackModels), never the source of truth.
var FallbackModels = []FallbackModel{
	{ModelID: "auto"},
	{ModelID: "claude-sonnet-4"},
	{ModelID: "claude-sonnet-4.5"},
	{ModelID: "claude-sonnet-4.6"},
	{ModelID: "claude-haiku-4.5"},
	{ModelID: "claude-opus-4.5"},
	{ModelID: "claude-opus-4.6"},
	{ModelID: "claude-opus-4.7"},
	{ModelID: "claude-opus-4.8"},
	{ModelID: "deepseek-3.2"},
	{ModelID: "glm-5"},
	{ModelID: "minimax-m2.1"},
	{ModelID: "minimax-m2.5"},
	{ModelID: "qwen3-coder-next"},
}

// Config holds all runtime settings. Construct with Load.
type Config struct {
	// Server
	ServerHost string
	ServerPort int

	// Proxy auth — multiple keys supported (issue #236). Empty slice means
	// auth disabled is NOT allowed; a default is always seeded.
	ProxyAPIKeys []string

	// VPN/proxy for reaching Kiro.
	VPNProxyURL string

	// Single-account credentials (legacy / ACCOUNT_SYSTEM=false).
	RefreshToken   string
	ProfileARN     string
	Region         string // KIRO_REGION (SSO/default region)
	APIRegion      string // KIRO_API_REGION (Q API override)
	CredsFile      string // KIRO_CREDS_FILE
	CLIDBFile      string // KIRO_CLI_DB_FILE
	SQLiteReadonly bool

	// Tokens / retry.
	TokenRefreshThreshold int // seconds
	MaxRetries            int
	BaseRetryDelay        float64 // seconds

	// Models.
	HiddenModels          map[string]string // display -> internal kiro id
	ModelAliases          map[string]string // alias -> real model id
	HiddenFromList        []string
	FallbackModels        []FallbackModel
	ModelCacheTTL         int // seconds
	DefaultMaxInputTokens int

	// Tooling.
	ToolDescriptionMaxLength int

	// Truncation recovery.
	TruncationRecovery bool

	// Logging.
	LogLevel string

	// Streaming timeouts.
	FirstTokenTimeout    float64
	StreamingReadTimeout float64
	FirstTokenMaxRetries int

	// Debug.
	DebugMode string // off|errors|all
	DebugDir  string

	// Fake reasoning.
	FakeReasoningEnabled       bool
	FakeReasoningMaxTokens     int
	FakeReasoningBudgetCap     int
	FakeReasoningHandling      string
	FakeReasoningOpenTags      []string
	FakeReasoningInitialBuffer int

	// Payload guard.
	KiroMaxPayloadBytes int
	AutoTrimPayload     bool

	// Web search.
	WebSearchEnabled bool

	// Account system.
	AccountSystem      bool
	AccountsConfigFile string
	AccountsStateFile  string

	// Circuit breaker.
	AccountRecoveryTimeout      int
	AccountMaxBackoffMultiplier float64
	AccountProbabilisticRetry   float64
	AccountCacheTTL             int

	// State persistence.
	StateSaveIntervalSeconds int

	// User-Agent spoofing (issue #115): configurable Kiro client version.
	KiroClientVersion string
}

// Load reads the .env file (if present) and the environment into a Config.
func Load() *Config {
	loadDotEnv(".env")

	c := &Config{
		ServerHost: getEnv("SERVER_HOST", DefaultServerHost),
		ServerPort: getEnvInt("SERVER_PORT", DefaultServerPort),

		ProxyAPIKeys: getEnvKeys("PROXY_API_KEY", "my-super-secret-password-123"),

		VPNProxyURL: getEnv("VPN_PROXY_URL", ""),

		RefreshToken:   getEnv("REFRESH_TOKEN", ""),
		ProfileARN:     getEnv("PROFILE_ARN", ""),
		Region:         getEnv("KIRO_REGION", DefaultRegion),
		APIRegion:      getEnv("KIRO_API_REGION", ""),
		CredsFile:      getRawEnv("KIRO_CREDS_FILE", ".env"),
		CLIDBFile:      getRawEnv("KIRO_CLI_DB_FILE", ".env"),
		SQLiteReadonly: getEnvBool("SQLITE_READONLY", false),

		TokenRefreshThreshold: DefaultTokenRefreshThreshold,
		MaxRetries:            3,
		BaseRetryDelay:        1.0,

		HiddenModels:          map[string]string{},
		ModelAliases:          map[string]string{"auto-kiro": "auto"},
		HiddenFromList:        []string{"auto"},
		FallbackModels:        FallbackModels,
		ModelCacheTTL:         3600,
		DefaultMaxInputTokens: 200000,

		ToolDescriptionMaxLength: getEnvInt("TOOL_DESCRIPTION_MAX_LENGTH", 10000),

		TruncationRecovery: getEnvBool("TRUNCATION_RECOVERY", true),

		LogLevel: strings.ToUpper(getEnv("LOG_LEVEL", "INFO")),

		FirstTokenTimeout:    getEnvFloat("FIRST_TOKEN_TIMEOUT", 15),
		StreamingReadTimeout: getEnvFloat("STREAMING_READ_TIMEOUT", 300),
		FirstTokenMaxRetries: getEnvInt("FIRST_TOKEN_MAX_RETRIES", 3),

		DebugMode: normalizeEnum(strings.ToLower(getEnv("DEBUG_MODE", "")), []string{"off", "errors", "all"}, "off"),
		DebugDir:  getEnv("DEBUG_DIR", "debug_logs"),

		FakeReasoningEnabled:       !inSet(strings.ToLower(getEnv("FAKE_REASONING", "")), "false", "0", "no", "disabled", "off"),
		FakeReasoningMaxTokens:     getEnvInt("FAKE_REASONING_MAX_TOKENS", 4000),
		FakeReasoningBudgetCap:     getEnvInt("FAKE_REASONING_BUDGET_CAP", 10000),
		FakeReasoningHandling:      normalizeEnum(strings.ToLower(getEnv("FAKE_REASONING_HANDLING", "as_reasoning_content")), []string{"as_reasoning_content", "remove", "pass", "strip_tags"}, "as_reasoning_content"),
		FakeReasoningOpenTags:      []string{"<thinking>", "<think>", "<reasoning>", "<thought>"},
		FakeReasoningInitialBuffer: getEnvInt("FAKE_REASONING_INITIAL_BUFFER_SIZE", 20),

		KiroMaxPayloadBytes: getEnvInt("KIRO_MAX_PAYLOAD_BYTES", 600000),
		AutoTrimPayload:     getEnvBool("AUTO_TRIM_PAYLOAD", false),

		WebSearchEnabled: getEnvBool("WEB_SEARCH_ENABLED", true),

		AccountSystem:      getEnvBool("ACCOUNT_SYSTEM", false),
		AccountsConfigFile: getEnv("ACCOUNTS_CONFIG_FILE", "credentials.json"),
		AccountsStateFile:  getEnv("ACCOUNTS_STATE_FILE", "state.json"),

		AccountRecoveryTimeout:      getEnvInt("ACCOUNT_RECOVERY_TIMEOUT", 60),
		AccountMaxBackoffMultiplier: getEnvFloat("ACCOUNT_MAX_BACKOFF_MULTIPLIER", 1440.0),
		AccountProbabilisticRetry:   getEnvFloat("ACCOUNT_PROBABILISTIC_RETRY_CHANCE", 0.1),
		AccountCacheTTL:             getEnvInt("ACCOUNT_CACHE_TTL", 43200),

		StateSaveIntervalSeconds: getEnvInt("STATE_SAVE_INTERVAL_SECONDS", 10),

		KiroClientVersion: getEnv("KIRO_CLIENT_VERSION", DefaultKiroClientVersion),
	}
	return c
}

// --- env helpers ---

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	// Truthy values are "true", "1", or "yes" (case-insensitive): when the
	// variable is set, ONLY those values are truthy (any other value is false,
	// regardless of the default); when unset, fall back to def.
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// getEnvKeys parses a comma/whitespace separated list of API keys, falling back
// to a single default. Supports multi-key auth (issue #236).
func getEnvKeys(key, def string) []string {
	out := strings.FieldsFunc(getEnv(key, def), func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	if len(out) == 0 {
		out = []string{def}
	}
	return out
}

func inSet(v string, set ...string) bool {
	return slices.Contains(set, v)
}

func normalizeEnum(v string, allowed []string, def string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return def
}

// URL builders.
func GetKiroRefreshURL(region string) string { return sprintfRegion(KiroRefreshURLTemplate, region) }
func GetAWSSSOOIDCURL(region string) string  { return sprintfRegion(AWSSSOOIDCURLTemplate, region) }
func GetKiroAPIHost(region string) string    { return sprintfRegion(KiroAPIHostTemplate, region) }
func GetKiroQHost(region string) string      { return sprintfRegion(KiroQHostTemplate, region) }

func sprintfRegion(tmpl, region string) string {
	return strings.Replace(tmpl, "%s", region, 1)
}

// getRawEnv reads a variable directly from the .env file without escape-sequence
// processing (important for Windows paths). Falls back to os.Getenv.
func getRawEnv(varName, envFile string) string {
	if raw := readRawEnvValue(varName, envFile); raw != "" {
		return raw
	}
	return os.Getenv(varName)
}

func readRawEnvValue(varName, envFile string) string {
	f, err := os.Open(envFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	prefix := varName + "="
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return stripMatchingQuotes(strings.TrimSpace(line[len(prefix):]))
	}
	return ""
}

// stripMatchingQuotes removes a single matching pair of surrounding quotes.
func stripMatchingQuotes(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// loadDotEnv loads KEY=VALUE pairs from the given file into the process
// environment without overriding already-set variables.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := stripMatchingQuotes(strings.TrimSpace(line[eq+1:]))
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
