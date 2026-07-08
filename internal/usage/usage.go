// Package usage surfaces Kiro "credit" consumption as usage information.
//
// Kiro reports credit usage in two places:
//
//   - Streaming: GenerateAssistantResponse emits a metering "usage" event whose
//     payload carries the credits consumed by the request. The awsstream parser
//     surfaces this as an Event of type EventUsage whose Data is the raw decoded
//     JSON value (a number or an object). CreditsFromUsageEvent extracts the
//     credit amount from that value.
//   - Non-streaming: a usage-limits endpoint on the Q host reports the account's
//     total / used / remaining credit balance. FetchLimits performs that call.
//
// This package backs a GET /v1/usage endpoint and lets routes add a
// credits_used field into response usage objects.
//
// IMPORTANT: Kiro's usage-limits API is not publicly documented. The endpoint
// path, x-amz-target, request body and response shape used by FetchLimits are
// best-effort assumptions (see the constants and comments below). FetchLimits is
// deliberately defensive: any failure — transport error, non-2xx status, or an
// unrecognized response shape — returns a descriptive error instead of panicking,
// so the calling route can degrade gracefully (e.g. omit credit balance).
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/util"
)

// Best-effort assumptions about Kiro's non-streaming usage-limits operation.
//
// The Kiro runtime exposes named, path-per-operation endpoints over the AWS
// JSON-1.0 protocol (e.g. /generateAssistantResponse, /listAvailableModels,
// /mcp). By analogy the usage-limits operation is assumed to live at
// /getUsageLimits with an x-amz-target on the non-streaming CodeWhisperer
// service. If these are wrong the call will fail with a clear error.
const (
	usageLimitsPath   = "/getUsageLimits"
	usageLimitsTarget = "AmazonCodeWhispererService.GetUsageLimits"
)

// creditUsageKeys are the (lower-cased) object keys searched, in priority order,
// when extracting the credits consumed from a streaming "usage" event payload.
var creditUsageKeys = []string{
	"creditsused",
	"credits_used",
	"creditused",
	"credit_used",
	"credits",
	"creditconsumption",
	"creditconsumed",
	"consumedcredits",
	"amount",
	"consumed",
}

// CreditsFromUsageEvent extracts the credits consumed from an awsstream
// EventUsage Data value. That value is whatever encoding/json decoded for the
// "usage" field of the metering event: typically a JSON number (the credits
// directly) or a JSON object carrying a credit/metering field.
//
// It is defensive about the concrete Go shapes encoding/json produces (float64
// for numbers, map[string]any for objects, string for quoted values) and about
// camelCase vs snake_case keys.
//
// ok reports whether a credit value was positively identified. For a bare number
// ok is false when the value is zero, because the awsstream parser uses 0 as the
// "usage field absent" sentinel; for an object an explicit credit key of 0 is
// reported as (0, true).
func CreditsFromUsageEvent(data any) (creditsUsed float64, ok bool) {
	switch v := data.(type) {
	case nil:
		return 0, false
	case map[string]any:
		return creditsFromMap(v)
	default:
		// A bare numeric usage value is the credits consumed. Treat zero as the
		// "absent" sentinel rather than a real reading.
		if f, isNum := asNumber(v); isNum && f != 0 {
			return f, true
		}
		return 0, false
	}
}

// creditsFromMap searches a decoded usage object (and a nested "metering"
// object, if present) for a recognizable credit value.
func creditsFromMap(m map[string]any) (float64, bool) {
	lower := lowerKeys(m)
	for _, k := range creditUsageKeys {
		if v, present := lower[k]; present {
			if f, ok := asFloat(v); ok {
				return f, true
			}
		}
	}
	// "metering" may be a number or a nested object.
	if v, present := lower["metering"]; present {
		if f, ok := asFloat(v); ok {
			return f, true
		}
		if sub, ok := v.(map[string]any); ok {
			if f, ok := creditsFromMap(sub); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// Limits is a best-effort view of an account's Kiro credit balance.
//
// Any field may be zero if the upstream response did not report it. Raw holds
// the full decoded response so callers can surface additional detail or debug an
// unrecognized shape.
type Limits struct {
	TotalCredits     float64
	UsedCredits      float64
	RemainingCredits float64
	Raw              map[string]any
}

// limit-field key sets (lower-cased), searched case-insensitively.
var (
	totalCreditKeys     = []string{"totalcredits", "total", "creditlimit", "limit", "quota", "maxcredits", "creditquota"}
	usedCreditKeys      = []string{"usedcredits", "used", "creditsused", "credits_used", "consumed", "consumedcredits", "usage"}
	remainingCreditKeys = []string{"remainingcredits", "remaining", "available", "availablecredits", "creditsremaining", "balance"}
)

// authManager is the subset of *auth.KiroAuthManager that FetchLimits needs.
// Declared as an anonymous interface in the signature so this package does not
// import internal/auth.
type authManager interface {
	QHost() string
	GetAccessToken(context.Context) (string, error)
	Fingerprint() string
	ProfileARN() string
}

// FetchLimits performs a best-effort, non-streaming call to Kiro's usage-limits
// endpoint at am.QHost() and returns the account's credit balance.
//
// It builds the standard Kiro headers via util.KiroHeaders, overrides the
// x-amz-target for the usage-limits operation, and ALWAYS includes the account's
// profileArn in the request body (Kiro rejects credit/quota calls without it).
// When hc is nil a 30s-timeout client is used. clientVersion is the spoofed Kiro
// IDE version for the User-Agent.
//
// Because the upstream contract is undocumented, FetchLimits returns a clear
// error on transport failure, a non-2xx status, an unparseable body, or a
// response whose shape carries no recognizable credit fields. On the last case
// the returned Limits still carries Raw so the route may inspect or forward it.
func FetchLimits(ctx context.Context, am authManager, hc *http.Client, clientVersion string) (Limits, error) {
	if am == nil {
		return Limits{}, fmt.Errorf("usage: auth manager is nil")
	}
	token, err := am.GetAccessToken(ctx)
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to get access token: %w", err)
	}

	url := strings.TrimRight(am.QHost(), "/") + usageLimitsPath

	headers := util.KiroHeaders(token, am.Fingerprint(), clientVersion)
	// KiroHeaders targets the streaming GenerateAssistantResponse operation;
	// override it for the usage-limits call.
	headers["x-amz-target"] = usageLimitsTarget

	// ALWAYS include profileArn (Kiro associates usage/quota with the profile).
	reqBody := map[string]any{"profileArn": am.ProfileARN()}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := hc
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return Limits{}, fmt.Errorf("usage: usage-limits request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to read usage-limits response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Limits{}, fmt.Errorf("usage: usage-limits returned HTTP %d: %s", resp.StatusCode, util.Truncate(string(body), 300))
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Limits{}, fmt.Errorf("usage: could not parse usage-limits response (assumptions about the endpoint may be wrong): %w", err)
	}

	lim, found := extractLimits(parsed)
	lim.Raw = parsed
	if !found {
		return lim, fmt.Errorf("usage: usage-limits response had no recognizable credit fields (endpoint/shape assumptions may be wrong)")
	}
	return lim, nil
}

// extractLimits pulls total/used/remaining credits out of a decoded usage-limits
// response, searching the top level first and then one level of nested objects
// (Kiro may wrap the figures under a container key). Missing values are derived
// arithmetically when the other two are present.
func extractLimits(m map[string]any) (Limits, bool) {
	lower := lowerKeys(m)
	total, tOK := findFloat(lower, totalCreditKeys)
	used, uOK := findFloat(lower, usedCreditKeys)
	remaining, rOK := findFloat(lower, remainingCreditKeys)

	if !tOK && !uOK && !rOK {
		// Descend one level into nested objects.
		for _, v := range m {
			sub, ok := v.(map[string]any)
			if !ok {
				continue
			}
			subLower := lowerKeys(sub)
			total, tOK = findFloat(subLower, totalCreditKeys)
			used, uOK = findFloat(subLower, usedCreditKeys)
			remaining, rOK = findFloat(subLower, remainingCreditKeys)
			if tOK || uOK || rOK {
				break
			}
		}
	}

	if !tOK && !uOK && !rOK {
		return Limits{}, false
	}

	switch {
	case !rOK && tOK && uOK:
		remaining = total - used
	case !uOK && tOK && rOK:
		used = total - remaining
	case !tOK && uOK && rOK:
		total = used + remaining
	}

	return Limits{TotalCredits: total, UsedCredits: used, RemainingCredits: remaining}, true
}

// findFloat returns the first numeric value found for any of keys in the
// already-lower-cased map.
func findFloat(lower map[string]any, keys []string) (float64, bool) {
	for _, k := range keys {
		if v, present := lower[k]; present {
			if f, ok := asFloat(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// lowerKeys returns a shallow copy of m with lower-cased keys for
// case-insensitive lookup.
func lowerKeys(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// asFloat coerces a decoded JSON scalar (number or numeric string) to float64.
func asFloat(v any) (float64, bool) {
	if f, ok := asNumber(v); ok {
		return f, true
	}
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// asNumber coerces numeric Go shapes (the kinds encoding/json yields, plus a few
// defensive extras) to float64. Strings are not accepted; use asFloat for those.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
		return 0, false
	default:
		return 0, false
	}
}
