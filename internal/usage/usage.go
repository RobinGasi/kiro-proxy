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
// Kiro's usage-limits API is undocumented but was recovered from the IDE's own
// GetUsageLimitsCommand call (captured in q-client.log): a GET at /GetUsageLimits
// on the profile's home-region runtime host, with origin/profileArn/resourceType
// query params, returning a usageBreakdownList. FetchLimits mirrors that. It stays
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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/util"
)

// Kiro's non-streaming usage-limits operation, recovered from the IDE bundle.
//
// GetUsageLimits belongs to the non-streaming AmazonCodeWhispererService (Kiro's
// CodeWhispererRuntimeClient), which resolves to the AWS Q data-plane endpoint
// https://q.<region>.amazonaws.com — NOT the runtime.<region>.kiro.dev host that
// serves the streaming GenerateAssistantResponse op (usage-limits 404s there,
// and so does ListAvailableModels — the proxy quietly falls back to a static
// model list). It speaks awsJson1_0: POST "/", Content-Type
// application/x-amz-json-1.0, dispatched by the x-amz-target header, with a
// bearer token. Usage is profile-scoped, so the region comes from the profile ARN.
const (
	// usageLimitsTarget is the awsJson x-amz-target that dispatches the operation;
	// the POST itself is to the root path "/" per the awsJson1_0 protocol.
	usageLimitsTarget = "AmazonCodeWhispererService.GetUsageLimits"

	// codeWhispererHostTemplate is the AWS Q data-plane endpoint that serves the
	// non-streaming AmazonCodeWhispererService operations (GetUsageLimits, GetProfile).
	codeWhispererHostTemplate = "https://q.%s.amazonaws.com"

	// usageResourceType is the credit pool requested. Kiro's IDE sends this exact
	// value; the returned breakdown entry is itself labelled resourceType "CREDIT".
	usageResourceType = "AGENTIC_REQUEST"
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
	totalCreditKeys     = []string{"totalcredits", "total", "creditlimit", "limit", "quota", "maxcredits", "creditquota", "usagelimit"}
	usedCreditKeys      = []string{"usedcredits", "used", "creditsused", "credits_used", "consumed", "consumedcredits", "usage", "currentusage"}
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

// FetchLimits performs the non-streaming awsJson1_0 GetUsageLimits call against
// the AWS Q data-plane endpoint for the profile's home region and returns the
// account's credit balance.
//
// It mirrors Kiro's CodeWhispererRuntimeClient: POST "/" with an x-amz-target
// header, Content-Type application/x-amz-json-1.0 and a body of {origin,
// profileArn, resourceType}, reusing util.KiroHeaders (which already sets the
// bearer token, content-type and user-agent) and overriding only the target,
// which otherwise names the streaming op. The region is derived from the profile
// ARN because usage is profile-scoped. When hc is nil a 30s-timeout client is
// used; clientVersion is the spoofed Kiro IDE version for the User-Agent.
//
// FetchLimits stays defensive: it returns a clear error on transport failure, a
// non-2xx status, an unparseable body, or a response whose shape carries no
// recognizable credit fields. In the last case the returned Limits still carries
// Raw so the route may inspect or forward it.
func FetchLimits(ctx context.Context, am authManager, hc *http.Client, clientVersion string) (Limits, error) {
	if am == nil {
		return Limits{}, fmt.Errorf("usage: auth manager is nil")
	}
	token, err := am.GetAccessToken(ctx)
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to get access token: %w", err)
	}

	reqBody, err := json.Marshal(map[string]any{
		"origin":       "AI_EDITOR",
		"profileArn":   am.ProfileARN(),
		"resourceType": usageResourceType,
	})
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to encode request: %w", err)
	}

	// awsJson1_0: POST to the root path, dispatched by x-amz-target. Reuse the
	// standard Kiro headers (bearer token, content-type, user-agent) and override
	// only the target, which otherwise names the streaming op.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, usageHost(am)+"/", bytes.NewReader(reqBody))
	if err != nil {
		return Limits{}, fmt.Errorf("usage: failed to build request: %w", err)
	}
	for k, v := range util.KiroHeaders(token, am.Fingerprint(), clientVersion) {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-amz-target", usageLimitsTarget)

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
		return Limits{}, fmt.Errorf("usage: could not parse usage-limits response: %w", err)
	}

	lim, found := extractLimits(parsed)
	lim.Raw = parsed
	if !found {
		return lim, fmt.Errorf("usage: usage-limits response had no recognizable credit fields")
	}
	return lim, nil
}

// regionRe validates an AWS region token (e.g. eu-central-1) parsed from an ARN.
var regionRe = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d+$`)

// usageHost returns the AWS Q data-plane endpoint for the usage-limits call,
// pinned to the profile's home region (parsed from the ARN:
// arn:aws:codewhisperer:REGION:account:profile/id). Usage is profile-scoped, so
// this must be the profile's region rather than the globally-configured
// KIRO_REGION. Falls back to us-east-1 (the SDK default) when the ARN carries no
// parseable region.
func usageHost(am authManager) string {
	region := "us-east-1"
	if parts := strings.Split(am.ProfileARN(), ":"); len(parts) >= 4 && regionRe.MatchString(parts[3]) {
		region = parts[3]
	}
	return fmt.Sprintf(codeWhispererHostTemplate, region)
}

// limitsFromBreakdown reads Kiro's documented usage shape: a usageBreakdownList
// array whose entries carry currentUsage/usageLimit per resourceType. It prefers
// the AGENTIC_REQUEST pool (the credit balance) and otherwise takes the first
// entry that reports either figure.
func limitsFromBreakdown(m map[string]any) (Limits, bool) {
	var raw any
	for k, v := range m {
		if strings.EqualFold(k, "usageBreakdownList") {
			raw = v
			break
		}
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return Limits{}, false
	}

	pick := func(want string) (limit, used float64, ok bool) {
		for _, item := range list {
			entry, isMap := item.(map[string]any)
			if !isMap {
				continue
			}
			lower := lowerKeys(entry)
			if want != "" {
				rt, _ := lower["resourcetype"].(string)
				if !strings.EqualFold(rt, want) {
					continue
				}
			}
			limit, lOK := findFloat(lower, []string{"usagelimit"})
			used, uOK := findFloat(lower, []string{"currentusage"})
			if lOK || uOK {
				return limit, used, true
			}
		}
		return 0, 0, false
	}

	limit, used, ok := pick("CREDIT")
	if !ok {
		limit, used, ok = pick(usageResourceType)
	}
	if !ok {
		limit, used, ok = pick("")
	}
	if !ok {
		return Limits{}, false
	}
	return Limits{TotalCredits: limit, UsedCredits: used, RemainingCredits: limit - used}, true
}

// extractLimits pulls total/used/remaining credits out of a decoded usage-limits
// response, searching the top level first and then one level of nested objects
// (Kiro may wrap the figures under a container key). Missing values are derived
// arithmetically when the other two are present.
func extractLimits(m map[string]any) (Limits, bool) {
	// Preferred: Kiro's documented usageBreakdownList shape.
	if lim, ok := limitsFromBreakdown(m); ok {
		return lim, true
	}

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
