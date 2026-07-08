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

type fakeCache struct{ ids []string }

func (f fakeCache) IsValidModel(n string) bool {
	for _, id := range f.ids {
		if id == n {
			return true
		}
	}
	return false
}
func (f fakeCache) GetAllModelIDs() []string { return f.ids }

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
		// Haiku 4.5 → 200k.
		{"claude-haiku-4-5", 200_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"claude-haiku-4.5", 200_000},
		// Older Claude / other haiku → def.
		{"claude-3-7-sonnet", def},
		{"claude-3-5-haiku", def},
		{"claude-haiku-3.5", def},
		{"claude-opus-3", def},
		// Non-Claude / unknown → def.
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
