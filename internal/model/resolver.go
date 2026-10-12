// Package model implements the dynamic model-resolution pipeline:
// normalize → dynamic cache → hidden models → optimistic pass-through.
//
// Principle: we are a gateway, not a gatekeeper. Kiro is the final arbiter of
// which models exist, so unknown model IDs are forwarded as-is.
package model

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Cache is the dynamic model catalog (from ListAvailableModels).
type Cache interface {
	IsValidModel(normalized string) bool
	GetAllModelIDs() []string
	// Capabilities returns the token limits Kiro reported for normalized.
	Capabilities(normalized string) (Capabilities, bool)
}

type requestSchemaCache interface {
	AdditionalModelRequestFieldsSchema(normalized string) []byte
}

var (
	reContextSuffix = regexp.MustCompile(`(?i)\[\d+[mk]\]$`)
	reStandard      = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)-(\d{1,2})(?:-(?:\d{8}|latest|\d+))?$`)
	reNoMinor       = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)(?:-\d{8})?$`)
	reLegacy        = regexp.MustCompile(`^(claude)-(\d+)-(\d+)-(haiku|sonnet|opus)(?:-(?:\d{8}|latest|\d+))?$`)
	reDotWithDate   = regexp.MustCompile(`^(claude-(?:\d+\.\d+-)?(?:haiku|sonnet|opus)(?:-\d+\.\d+)?)-\d{8}$`)
	reInverted      = regexp.MustCompile(`^claude-(\d+)\.(\d+)-(haiku|sonnet|opus)-(.+)$`)
	reFamily        = regexp.MustCompile(`(?i)(haiku|sonnet|opus)`)
	reVersion       = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)
	// reBareFamily matches a bare family name ("opus", "haiku", optionally with a
	// "claude-" prefix) with NO version, so it can resolve to the latest available
	// model of that family.
	reBareFamily = regexp.MustCompile(`^(?:claude-)?(haiku|sonnet|opus)$`)
)

// Known input-context window sizes (in tokens) for Claude model families.
const (
	contextWindow1M   = 1_000_000
	contextWindow200K = 200_000
)

// Capabilities are the per-model token limits Kiro publishes in
// ListAvailableModels (`tokenLimits`). A zero field means "not reported".
type Capabilities struct {
	MaxInputTokens  int
	MaxOutputTokens int
}

// CatalogModel is one entry of Kiro's model catalog.
type CatalogModel struct {
	ID           string
	Capabilities Capabilities
	// AdditionalModelRequestFieldsSchema describes model-native reasoning fields.
	AdditionalModelRequestFieldsSchema json.RawMessage
}

// knownCapabilities is the offline fallback for model token limits, keyed by
// normalized model id. It exists only so a cold start — or a Kiro region that
// does not serve ListAvailableModels — still answers with real numbers instead
// of a generic default; a live catalog always wins (Resolver.Capabilities).
//
// Verified against ListAvailableModels on 2026-08-18. The GPT-5.6 tiers match
// Kiro's published 272K context window
// (https://kiro.dev/changelog/models/gpt-5-6/).
//
// The "auto" router is deliberately absent: it dispatches to whichever model
// Kiro picks, so the catalog's window for "auto" is an upper bound rather than
// something a client can rely on. Callers keep their own conservative default.
var knownCapabilities = map[string]Capabilities{
	"claude-haiku-4.5":  {MaxInputTokens: 200_000, MaxOutputTokens: 64_000},
	"claude-opus-4.5":   {MaxInputTokens: 200_000, MaxOutputTokens: 64_000},
	"claude-opus-4.6":   {MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000},
	"claude-opus-4.7":   {MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000},
	"claude-opus-4.8":   {MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000},
	"claude-opus-5":     {MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000},
	"claude-sonnet-4":   {MaxInputTokens: 200_000, MaxOutputTokens: 64_000},
	"claude-sonnet-4.5": {MaxInputTokens: 200_000, MaxOutputTokens: 64_000},
	"claude-sonnet-4.6": {MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000},
	"claude-sonnet-5":   {MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000},
	"gpt-5.6-luna":      {MaxInputTokens: 272_000, MaxOutputTokens: 128_000},
	"gpt-5.6-sol":       {MaxInputTokens: 272_000, MaxOutputTokens: 128_000},
	"gpt-5.6-terra":     {MaxInputTokens: 272_000, MaxOutputTokens: 128_000},
	"minimax-m2.1":      {MaxInputTokens: 196_000, MaxOutputTokens: 64_000},
	"minimax-m2.5":      {MaxInputTokens: 196_000, MaxOutputTokens: 64_000},
	"qwen3-coder-next":  {MaxInputTokens: 256_000, MaxOutputTokens: 64_000},
}

// CapabilitiesFor returns the offline-known token limits for modelID. The id is
// normalized first, then matched exactly, so an unrelated GPT or Claude id keeps
// the caller's own fallback rather than borrowing a neighbour's limits.
func CapabilitiesFor(modelID string) (Capabilities, bool) {
	caps, ok := knownCapabilities[NormalizeModelName(modelID)]
	return caps, ok
}

// ContextLimitFor returns the known input-context window (in tokens) for the
// given model, falling back to def when the model is unrecognized.
//
// Resolution order: the Claude family rules below, then the offline capability
// table (CapabilitiesFor), then def. Prefer Resolver.ContextLimitFor, which
// consults the live catalog before the offline table.
//
// Claude Opus and Sonnet 4.x (and newer), plus a bare "opus"/"sonnet" family
// name (which denotes the latest of that family), expose a 1,000,000-token
// window; Claude Haiku 4.5 exposes 200,000. Anything else — older versioned
// Claude models or non-Claude models — uses def (typically
// config.Config.DefaultMaxInputTokens).
// The model id is normalized first (NormalizeModelName), so client suffixes such
// as "[1m]", date stamps and "-latest" do not affect the result.
func ContextLimitFor(modelID string, def int) int {
	normalized := NormalizeModelName(modelID)
	if limit := familyContextLimit(normalized); limit > 0 {
		return limit
	}
	if caps, ok := knownCapabilities[normalized]; ok && caps.MaxInputTokens > 0 {
		return caps.MaxInputTokens
	}
	return def
}

// familyContextLimit applies the Claude family rules to an already-normalized id,
// returning 0 when no rule matches.
//
// These rules deliberately outrank any catalog-reported window: Kiro serves the
// larger window for the "[1m]" client suffix, which NormalizeModelName has
// already stripped by the time the limit is computed.
func familyContextLimit(normalized string) int {
	family := ExtractModelFamily(normalized)
	if family == "" {
		return 0
	}
	major, minor := modelVersion(normalized)
	switch family {
	case "opus", "sonnet":
		// Opus/Sonnet 4.x and newer expose a 1M window. major==0 means the name
		// carried no version — a bare family like "opus"/"sonnet", which denotes
		// "the latest of this family" and is currently 1M too. Treat it as 1M
		// rather than the unknown-model fallback. (This only matters when the model
		// cache is unavailable; Resolver.Resolve normally expands a bare family to a
		// concrete versioned id first.)
		if major == 0 || major >= 4 {
			return contextWindow1M
		}
	case "haiku":
		if major == 4 && minor == 5 {
			return contextWindow200K
		}
	}
	return 0
}

// modelVersion extracts the first major(.minor) version number from a normalized
// model name (e.g. "claude-opus-4.8" → 4, 8; "claude-sonnet-4" → 4, 0;
// "claude-3.7-sonnet" → 3, 7). It returns 0, 0 when no number is present.
func modelVersion(normalized string) (major, minor int) {
	m := reVersion.FindStringSubmatch(normalized)
	if m == nil {
		return 0, 0
	}
	major, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major, minor
}

// NormalizeModelName converts a client model name to Kiro's dotted format,
// stripping context-window suffixes ([1m], [200k]), date stamps and -latest.
// Unknown names pass through unchanged (case preserved minus the suffix).
func NormalizeModelName(name string) string {
	if name == "" {
		return name
	}
	// Strip context-window suffix (client indicator, not a model id).
	name = reContextSuffix.ReplaceAllString(name, "")

	lower := strings.ToLower(name)

	if m := reStandard.FindStringSubmatch(lower); m != nil {
		return m[1] + "." + m[2] // claude-haiku-4.5
	}
	if m := reNoMinor.FindStringSubmatch(lower); m != nil {
		return m[1] // claude-sonnet-4
	}
	if m := reLegacy.FindStringSubmatch(lower); m != nil {
		return m[1] + "-" + m[2] + "." + m[3] + "-" + m[4] // claude-3.7-sonnet
	}
	if m := reDotWithDate.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := reInverted.FindStringSubmatch(lower); m != nil {
		return "claude-" + m[3] + "-" + m[1] + "." + m[2] // claude-opus-4.5
	}
	// No transformation — preserve original case for pass-through.
	return name
}

// ToRuntimeModelID is a pass-through (no fallback). Kiro decides validity.
func ToRuntimeModelID(normalized string) string { return normalized }

// GetModelIDForKiro normalizes and resolves hidden models for converters that
// don't hold a full ModelResolver.
func GetModelIDForKiro(modelName string, hidden map[string]string) string {
	normalized := NormalizeModelName(modelName)
	if internal, ok := hidden[normalized]; ok {
		return ToRuntimeModelID(internal)
	}
	return ToRuntimeModelID(normalized)
}

// ExtractModelFamily returns "haiku"/"sonnet"/"opus" or "" if not a Claude model.
func ExtractModelFamily(modelName string) string {
	if m := reFamily.FindStringSubmatch(modelName); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// Resolution is the result of resolving an external model name.
type Resolution struct {
	InternalID      string // id to send to Kiro
	Source          string // "cache" | "hidden" | "passthrough"
	OriginalRequest string
	Normalized      string
	IsVerified      bool
}

// Resolver resolves external model names with optimistic pass-through.
type Resolver struct {
	cache          Cache
	hiddenModels   map[string]string
	aliases        map[string]string
	hiddenFromList map[string]struct{}
}

// NewResolver builds a Resolver. nil maps/slices are treated as empty.
func NewResolver(cache Cache, hidden, aliases map[string]string, hiddenFromList []string) *Resolver {
	hfl := make(map[string]struct{}, len(hiddenFromList))
	for _, h := range hiddenFromList {
		hfl[h] = struct{}{}
	}
	if hidden == nil {
		hidden = map[string]string{}
	}
	if aliases == nil {
		aliases = map[string]string{}
	}
	return &Resolver{cache: cache, hiddenModels: hidden, aliases: aliases, hiddenFromList: hfl}
}

// autoModelID is Kiro's server-side router. Its catalog window describes the
// best case across whatever it dispatches to, so it is deliberately treated as
// "unknown": a client that trusts an optimistic window overflows upstream,
// whereas an unknown one only makes the client compact earlier.
const autoModelID = "auto"

// Capabilities returns the token limits for modelID, preferring what Kiro
// reported in the live catalog over the offline table (CapabilitiesFor). The
// name is resolved first (aliases, bare families, hidden models), so the lookup
// uses the id Kiro itself knows.
//
// ok is false for an unknown model and for the "auto" router, letting the caller
// keep its own conservative default.
func (r *Resolver) Capabilities(modelID string) (Capabilities, bool) {
	id := r.Resolve(modelID).InternalID
	if id == autoModelID {
		return Capabilities{}, false
	}
	if r.cache != nil {
		if caps, ok := r.cache.Capabilities(id); ok {
			return caps, true
		}
	}
	return CapabilitiesFor(id)
}

// AdditionalModelRequestFieldsSchema returns a copy of Kiro's request-field
// schema for modelID, if the live catalog supplied one.
func (r *Resolver) AdditionalModelRequestFieldsSchema(modelID string) json.RawMessage {
	if r == nil || r.cache == nil {
		return nil
	}
	c, ok := r.cache.(requestSchemaCache)
	if !ok {
		return nil
	}
	id := r.Resolve(modelID).InternalID
	return c.AdditionalModelRequestFieldsSchema(id)
}

// ContextLimitFor returns the input-context window for modelID, falling back to
// def. It prefers the Claude family rules (see the package-level
// ContextLimitFor), then the live catalog, then the offline table.
func (r *Resolver) ContextLimitFor(modelID string, def int) int {
	if limit := familyContextLimit(NormalizeModelName(modelID)); limit > 0 {
		return limit
	}
	if caps, ok := r.Capabilities(modelID); ok && caps.MaxInputTokens > 0 {
		return caps.MaxInputTokens
	}
	return def
}

// Resolve never errors — unknown models pass through to Kiro.
func (r *Resolver) Resolve(externalModel string) Resolution {
	resolved := externalModel
	if alias, ok := r.aliases[externalModel]; ok {
		resolved = alias
	} else if latest := r.latestInFamily(externalModel); latest != "" {
		// A bare family name (e.g. "opus") resolves to the newest available model
		// of that family, so clients can pin a family and auto-track new releases.
		resolved = latest
	}
	normalized := NormalizeModelName(resolved)

	if r.cache != nil && r.cache.IsValidModel(normalized) {
		return Resolution{InternalID: ToRuntimeModelID(normalized), Source: "cache", OriginalRequest: externalModel, Normalized: normalized, IsVerified: true}
	}
	if internal, ok := r.hiddenModels[normalized]; ok {
		return Resolution{InternalID: ToRuntimeModelID(internal), Source: "hidden", OriginalRequest: externalModel, Normalized: normalized, IsVerified: true}
	}
	return Resolution{InternalID: ToRuntimeModelID(normalized), Source: "passthrough", OriginalRequest: externalModel, Normalized: normalized, IsVerified: false}
}

// latestInFamily resolves a bare family name (e.g. "opus", "claude-haiku") to the
// highest-version model of that family currently known to the cache. It returns
// "" when externalModel is not a bare family name or the family has no known
// models (in which case the caller passes the original through unchanged).
func (r *Resolver) latestInFamily(externalModel string) string {
	// Strip any context-window suffix ([1m]/[200k]) first, so "opus[1m]" — which a
	// client appends to request the larger window — still resolves by family.
	name := reContextSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(externalModel)), "")
	m := reBareFamily.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	family := m[1]

	if r.cache == nil {
		return ""
	}
	best := ""
	bestMajor, bestMinor := -1, -1
	for _, id := range r.cache.GetAllModelIDs() {
		if ExtractModelFamily(id) != family {
			continue
		}
		major, minor := modelVersion(NormalizeModelName(id))
		if major > bestMajor || (major == bestMajor && minor > bestMinor) {
			bestMajor, bestMinor, best = major, minor, id
		}
	}
	return best
}

// NextFamilyFallback returns the newest cached model in the same family that is
// older than failedID. It is available only for bare family requests; explicit
// versions and configured aliases are never rewritten.
func (r *Resolver) NextFamilyFallback(requested, failedID string) string {
	if r == nil || r.cache == nil {
		return ""
	}
	if _, aliased := r.aliases[requested]; aliased {
		return ""
	}
	name := reContextSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(requested)), "")
	m := reBareFamily.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	family := m[1]
	failedNormalized := NormalizeModelName(failedID)
	if ExtractModelFamily(failedNormalized) != family {
		return ""
	}
	failedMajor, failedMinor := modelVersion(failedNormalized)
	if failedMajor == 0 {
		return ""
	}

	best := ""
	bestMajor, bestMinor := -1, -1
	for _, id := range r.cache.GetAllModelIDs() {
		if ExtractModelFamily(id) != family {
			continue
		}
		major, minor := modelVersion(NormalizeModelName(id))
		if major == 0 || major > failedMajor || (major == failedMajor && minor >= failedMinor) {
			continue
		}
		if major > bestMajor || (major == bestMajor && minor > bestMinor) {
			bestMajor, bestMinor, best = major, minor, id
		}
	}
	return best
}

// GetAvailableModels returns the sorted union of cache + hidden + aliases,
// minus hiddenFromList.
func (r *Resolver) GetAvailableModels() []string {
	set := map[string]struct{}{}
	if r.cache != nil {
		for _, id := range r.cache.GetAllModelIDs() {
			set[id] = struct{}{}
		}
	}
	for k := range r.hiddenModels {
		set[k] = struct{}{}
	}
	for h := range r.hiddenFromList {
		delete(set, h)
	}
	for k := range r.aliases {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// GetModelsByFamily filters available models by family.
func (r *Resolver) GetModelsByFamily(family string) []string {
	family = strings.ToLower(family)
	var out []string
	for _, m := range r.GetAvailableModels() {
		if strings.Contains(strings.ToLower(m), family) {
			out = append(out, m)
		}
	}
	return out
}

// GetSuggestionsForModel returns same-family suggestions for error messages.
func (r *Resolver) GetSuggestionsForModel(modelName string) []string {
	if family := ExtractModelFamily(modelName); family != "" {
		return r.GetModelsByFamily(family)
	}
	return r.GetAvailableModels()
}
