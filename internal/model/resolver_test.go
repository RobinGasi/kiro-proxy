package model

import "testing"

func TestNormalizeModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		// Documented examples.
		{"claude-haiku-4-5-20251001", "claude-haiku-4.5"},
		{"claude-sonnet-4-5", "claude-sonnet-4.5"},
		{"claude-opus-4-5", "claude-opus-4.5"},
		{"claude-sonnet-4", "claude-sonnet-4"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"claude-3-7-sonnet", "claude-3.7-sonnet"},
		{"claude-3-7-sonnet-20250219", "claude-3.7-sonnet"},
		{"claude-4.5-opus-high", "claude-opus-4.5"},
		{"claude-4.5-sonnet-low", "claude-sonnet-4.5"},
		{"auto", "auto"},
		// Newer models + the [1m] context suffix (the headline-bug regression).
		{"claude-opus-4-8", "claude-opus-4.8"},
		{"claude-opus-4-8[1m]", "claude-opus-4.8"},
		{"claude-opus-4-8-20251215[1m]", "claude-opus-4.8"},
		{"claude-sonnet-4-6[200k]", "claude-sonnet-4.6"},
		{"claude-haiku-4-5-latest", "claude-haiku-4.5"},
		// Unknown models pass through unchanged.
		{"glm-5", "glm-5"},
		{"qwen3-coder-next", "qwen3-coder-next"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeModelName(c.in); got != c.want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractModelFamily(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-haiku-4.5", "haiku"},
		{"claude-sonnet-4-5", "sonnet"},
		{"claude-3.7-sonnet", "sonnet"},
		{"claude-opus-4.8", "opus"},
		{"gpt-4", ""},
		{"glm-5", ""},
	}
	for _, c := range cases {
		if got := ExtractModelFamily(c.in); got != c.want {
			t.Errorf("ExtractModelFamily(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeCache is a Cache whose catalog is a plain id list, plus an optional
// per-id capability map standing in for what Kiro reported.
type fakeCache struct {
	ids  []string
	caps map[string]Capabilities
}

func (f fakeCache) IsValidModel(n string) bool {
	for _, id := range f.ids {
		if id == n {
			return true
		}
	}
	return false
}
func (f fakeCache) GetAllModelIDs() []string { return f.ids }
func (f fakeCache) Capabilities(n string) (Capabilities, bool) {
	caps, ok := f.caps[n]
	return caps, ok
}

func TestContextLimitFor(t *testing.T) {
	// def is intentionally distinct from any known window so the fallback branch
	// is unambiguous (note Haiku 4.5 returns 200000, not def).
	const def = 12345
	cases := []struct {
		in   string
		want int
	}{
		// Opus/Sonnet 4.x and newer → 1M (suffixes/date stamps stripped first).
		{"claude-opus-4-8[1m]", 1_000_000},
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8-20251215[1m]", 1_000_000},
		{"claude-sonnet-4-5", 1_000_000},
		{"claude-sonnet-4-20250514", 1_000_000},
		{"claude-sonnet-4", 1_000_000},
		{"claude-opus-4-1", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-opus-5", 1_000_000},
		// Bare family names (no version) denote "latest of family" → 1M. These
		// reach ContextLimitFor only when the model cache is unavailable; normally
		// Resolver.Resolve expands them to a concrete versioned id first.
		{"opus", 1_000_000},
		{"sonnet", 1_000_000},
		{"claude-opus", 1_000_000},
		{"opus[1m]", 1_000_000},
		{"sonnet[1m]", 1_000_000},
		// Haiku 4.5 → 200k.
		{"claude-haiku-4-5", 200_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"claude-haiku-4.5", 200_000},
		// Older Claude / other haiku → def.
		{"claude-3-7-sonnet", def},
		{"claude-3-5-haiku", def},
		{"claude-haiku-3.5", def},
		{"claude-opus-3", def},
		// Non-Claude models absent from the capability table → def.
		{"gpt-4", def},
		{"glm-5", def},
		{"", def},
	}
	for _, c := range cases {
		if got := ContextLimitFor(c.in, def); got != c.want {
			t.Errorf("ContextLimitFor(%q, %d) = %d, want %d", c.in, def, got, c.want)
		}
	}
}

func TestContextLimitForCatalogModels(t *testing.T) {
	// Models the offline capability table knows resolve to their real window
	// instead of the caller's generic default.
	const def = 12345
	cases := []struct {
		in   string
		want int
	}{
		// Kiro serves 272k for every GPT-5.6 tier
		// (https://kiro.dev/changelog/models/gpt-5-6/).
		{"gpt-5.6-sol", 272_000},
		{"gpt-5.6-terra", 272_000},
		{"gpt-5.6-luna", 272_000},
		{"qwen3-coder-next", 256_000},
		{"minimax-m2.5", 196_000},
		// Matching is exact: a neighbouring GPT id must not inherit Sol's window.
		{"gpt-5.6", def},
		{"gpt-5.6-sol-preview", def},
		{"gpt-5", def},
		// The "auto" router's window depends on what it dispatches to, so it is
		// deliberately unknown.
		{"auto", def},
	}
	for _, c := range cases {
		if got := ContextLimitFor(c.in, def); got != c.want {
			t.Errorf("ContextLimitFor(%q, %d) = %d, want %d", c.in, def, got, c.want)
		}
	}
}

func TestCapabilitiesFor(t *testing.T) {
	caps, ok := CapabilitiesFor("gpt-5.6-sol")
	if !ok || caps.MaxInputTokens != 272_000 || caps.MaxOutputTokens != 128_000 {
		t.Errorf("CapabilitiesFor(sol) = %+v, %v; want 272000/128000, true", caps, ok)
	}
	// Normalization runs first, so client suffixes and date stamps still hit.
	if caps, ok := CapabilitiesFor("claude-opus-4-8[1m]"); !ok || caps.MaxOutputTokens != 128_000 {
		t.Errorf("CapabilitiesFor(opus-4-8[1m]) = %+v, %v; want 128000 output, true", caps, ok)
	}
	// Unknown ids report a miss rather than a zero-valued limit.
	for _, id := range []string{"gpt-5.6", "glm-5", "auto", ""} {
		if caps, ok := CapabilitiesFor(id); ok {
			t.Errorf("CapabilitiesFor(%q) = %+v, true; want miss", id, caps)
		}
	}
}

func TestResolverCapabilitiesPrefersLiveCatalog(t *testing.T) {
	// The live catalog is authoritative: a model Kiro re-tiers is picked up
	// without a code change, and a model missing from it falls back to the
	// offline table.
	r := NewResolver(
		fakeCache{
			ids: []string{"gpt-5.6-sol", "claude-opus-5", "glm-5", "auto"},
			caps: map[string]Capabilities{
				"gpt-5.6-sol": {MaxInputTokens: 400_000, MaxOutputTokens: 200_000},
				"glm-5":       {MaxInputTokens: 131_072, MaxOutputTokens: 32_768},
				"auto":        {MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000},
			},
		},
		nil,
		map[string]string{"auto-kiro": "auto"},
		[]string{"auto"},
	)

	if caps, ok := r.Capabilities("gpt-5.6-sol"); !ok || caps.MaxInputTokens != 400_000 || caps.MaxOutputTokens != 200_000 {
		t.Errorf("live catalog ignored: %+v, %v", caps, ok)
	}
	// Reported only by the catalog, absent from the offline table.
	if caps, ok := r.Capabilities("glm-5"); !ok || caps.MaxInputTokens != 131_072 {
		t.Errorf("catalog-only model = %+v, %v; want 131072, true", caps, ok)
	}
	// In the catalog but with no limits reported → offline table.
	if caps, ok := r.Capabilities("claude-opus-5"); !ok || caps.MaxInputTokens != 1_000_000 {
		t.Errorf("offline fallback = %+v, %v; want 1000000, true", caps, ok)
	}
	// The "auto" router stays unknown even when the catalog reports a window,
	// including through its alias.
	for _, id := range []string{"auto", "auto-kiro"} {
		if caps, ok := r.Capabilities(id); ok {
			t.Errorf("Capabilities(%q) = %+v, true; want miss", id, caps)
		}
	}
	// Unknown to both → miss, so the caller keeps its own default.
	if caps, ok := r.Capabilities("mystery-model"); ok {
		t.Errorf("Capabilities(mystery) = %+v, true; want miss", caps)
	}
}

func TestResolverContextLimitFor(t *testing.T) {
	const def = 12345
	r := NewResolver(
		fakeCache{
			ids: []string{"gpt-5.6-sol", "claude-opus-4.8"},
			caps: map[string]Capabilities{
				"gpt-5.6-sol": {MaxInputTokens: 272_000, MaxOutputTokens: 128_000},
				// Kiro reports the smaller window here because the 1M window is
				// requested via the "[1m]" client suffix, which normalization
				// strips. The family rule must therefore outrank the catalog.
				"claude-opus-4.8": {MaxInputTokens: 200_000, MaxOutputTokens: 128_000},
			},
		},
		nil, nil, nil,
	)

	if got := r.ContextLimitFor("gpt-5.6-sol", def); got != 272_000 {
		t.Errorf("ContextLimitFor(sol) = %d, want 272000", got)
	}
	if got := r.ContextLimitFor("claude-opus-4-8[1m]", def); got != 1_000_000 {
		t.Errorf("family rule lost to catalog: got %d, want 1000000", got)
	}
	if got := r.ContextLimitFor("mystery-model", def); got != def {
		t.Errorf("ContextLimitFor(mystery) = %d, want %d", got, def)
	}
}

func TestResolverPassthroughAndAlias(t *testing.T) {
	r := NewResolver(
		fakeCache{ids: []string{"claude-sonnet-4.5", "auto"}},
		map[string]string{"claude-3.7-sonnet": "CLAUDE_3_7"},
		map[string]string{"auto-kiro": "auto"},
		[]string{"auto"},
	)

	// Alias resolves then normalizes to a cached model.
	if res := r.Resolve("auto-kiro"); res.InternalID != "auto" || res.Source != "cache" {
		t.Errorf("alias resolve = %+v", res)
	}
	// Hidden model maps to internal id.
	if res := r.Resolve("claude-3-7-sonnet"); res.InternalID != "CLAUDE_3_7" || res.Source != "hidden" {
		t.Errorf("hidden resolve = %+v", res)
	}
	// Unknown newer model passes through (never rejected).
	if res := r.Resolve("claude-opus-4-8[1m]"); res.InternalID != "claude-opus-4.8" || res.Source != "passthrough" {
		t.Errorf("passthrough resolve = %+v", res)
	}
	// /v1/models list hides "auto" but exposes the alias.
	models := r.GetAvailableModels()
	hasAuto, hasAlias := false, false
	for _, m := range models {
		if m == "auto" {
			hasAuto = true
		}
		if m == "auto-kiro" {
			hasAlias = true
		}
	}
	if hasAuto {
		t.Errorf("expected 'auto' hidden from list, got %v", models)
	}
	if !hasAlias {
		t.Errorf("expected 'auto-kiro' in list, got %v", models)
	}
}

func TestResolveBareFamilyToLatest(t *testing.T) {
	r := NewResolver(
		fakeCache{ids: []string{
			"claude-opus-4.5", "claude-opus-4.8", "claude-opus-4.6",
			"claude-sonnet-4", "claude-sonnet-4.6",
			"claude-haiku-4.5",
		}},
		nil, nil, nil,
	)

	// A bare family name resolves to the newest model of that family — including
	// with a context-window suffix like "opus[1m]" (used to request 1M context).
	cases := map[string]string{
		"opus":        "claude-opus-4.8",
		"claude-opus": "claude-opus-4.8",
		"OPUS":        "claude-opus-4.8",
		"opus[1m]":    "claude-opus-4.8",
		"sonnet":      "claude-sonnet-4.6",
		"haiku":       "claude-haiku-4.5",
	}
	for in, want := range cases {
		if got := r.Resolve(in).InternalID; got != want {
			t.Errorf("Resolve(%q).InternalID = %q, want %q", in, got, want)
		}
	}

	// A specific version is NOT treated as a bare family — it passes through.
	if got := r.Resolve("claude-opus-4.6").InternalID; got != "claude-opus-4.6" {
		t.Errorf("specific version was rewritten: %q", got)
	}

	// A family with no known models falls through unchanged.
	empty := NewResolver(fakeCache{ids: []string{"claude-opus-4.8"}}, nil, nil, nil)
	if got := empty.Resolve("haiku").InternalID; got != "haiku" {
		t.Errorf("empty family should pass through, got %q", got)
	}
}

func TestNextFamilyFallbackOnlyForBareRequests(t *testing.T) {
	r := NewResolver(
		fakeCache{ids: []string{
			"claude-opus-4.7", "claude-opus-5", "claude-opus-4.8",
			"claude-sonnet-4.6",
		}},
		nil,
		map[string]string{"stable-opus": "claude-opus-4.8"},
		nil,
	)

	cases := []struct {
		requested string
		failed    string
		want      string
	}{
		{"opus", "claude-opus-5", "claude-opus-4.8"},
		{"opus[1m]", "claude-opus-4.8", "claude-opus-4.7"},
		{"claude-opus", "claude-opus-4.7", ""},
		{"claude-opus-5", "claude-opus-5", ""},
		{"stable-opus", "claude-opus-4.8", ""},
		{"sonnet", "claude-opus-5", ""},
	}
	for _, tc := range cases {
		if got := r.NextFamilyFallback(tc.requested, tc.failed); got != tc.want {
			t.Errorf("NextFamilyFallback(%q, %q) = %q, want %q", tc.requested, tc.failed, got, tc.want)
		}
	}
}
