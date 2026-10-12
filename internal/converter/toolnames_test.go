package converter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

func TestShortenToolNameLeavesShortNamesUnchanged(t *testing.T) {
	names := []string{
		"",
		"get_user",
		"mcp__github__list_issues",
		strings.Repeat("a", KiroMaxToolNameLen), // exactly at the limit
	}
	for _, n := range names {
		if got := ShortenToolName(n); got != n {
			t.Errorf("ShortenToolName(%q) = %q, want unchanged", n, got)
		}
	}
}

func TestShortenToolNameShortensOverLongNames(t *testing.T) {
	// The exact name from the reported Kiro 400 error (65 characters).
	name := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueTypesMetadata"
	if len(name) != 65 {
		t.Fatalf("precondition: len=%d, want 65", len(name))
	}

	alias := ShortenToolName(name)
	if len(alias) > KiroMaxToolNameLen {
		t.Errorf("alias %q has length %d, want <= %d", alias, len(alias), KiroMaxToolNameLen)
	}
	if alias == name {
		t.Errorf("alias was not shortened")
	}
	// Deterministic: same input yields the same alias.
	if again := ShortenToolName(name); again != alias {
		t.Errorf("ShortenToolName not deterministic: %q != %q", alias, again)
	}
}

func TestShortenToolNameDistinctOriginsDistinctAliases(t *testing.T) {
	// Two names sharing a long common prefix but differing past the truncation
	// point must not collide.
	base := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueType"
	a := ShortenToolName(base + "MetadataAlpha")
	b := ShortenToolName(base + "MetadataBravo")
	if a == b {
		t.Errorf("distinct names collided to the same alias %q", a)
	}
	if len(a) > KiroMaxToolNameLen || len(b) > KiroMaxToolNameLen {
		t.Errorf("aliases exceed limit: %d, %d", len(a), len(b))
	}
}

func TestAliasToolsRoundTrips(t *testing.T) {
	long := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueTypesMetadata"
	short := "mcp__github__list_issues"
	tools := []Tool{{Name: long}, {Name: short}}

	aliased, reverse := aliasTools(tools)
	if reverse == nil {
		t.Fatal("expected a non-nil reverse map when a name needs shortening")
	}
	if _, ok := reverse[short]; ok {
		t.Errorf("short name should not appear in the reverse map")
	}

	alias := ShortenToolName(long)
	if aliased[0].Name != alias {
		t.Errorf("aliased tool name = %q, want %q", aliased[0].Name, alias)
	}
	if aliased[1].Name != short {
		t.Errorf("short tool name should be unchanged, got %q", aliased[1].Name)
	}
	// Caller data must be untouched.
	if tools[0].Name != long {
		t.Errorf("input tools were mutated: %q", tools[0].Name)
	}

	if got := RestoreToolName(reverse, alias); got != long {
		t.Errorf("RestoreToolName(%q) = %q, want %q", alias, got, long)
	}
	// Names not in the map (and the short name) pass through unchanged.
	if got := RestoreToolName(reverse, short); got != short {
		t.Errorf("RestoreToolName(%q) = %q, want unchanged", short, got)
	}
}

func TestAliasToolsNilWhenNoShortening(t *testing.T) {
	tools := []Tool{{Name: "get_user"}, {Name: "mcp__github__list_issues"}}
	aliased, reverse := aliasTools(tools)
	if reverse != nil {
		t.Errorf("expected nil reverse map, got %v", reverse)
	}
	// The original slice is returned unchanged when nothing needs aliasing.
	if &aliased[0] != &tools[0] {
		t.Errorf("expected the original slice to be returned unchanged")
	}
	// RestoreToolName tolerates a nil map.
	if got := RestoreToolName(nil, "anything"); got != "anything" {
		t.Errorf("RestoreToolName(nil, ...) = %q, want unchanged", got)
	}
}

func TestAliasMessageToolNamesRewritesHistory(t *testing.T) {
	long := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueTypesMetadata"
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       "toolu_1",
			Function: FunctionCall{Name: long, Arguments: map[string]any{}},
		}}},
	}
	out := aliasMessageToolNames(msgs)

	if got := out[0].ToolCalls[0].Function.Name; got != ShortenToolName(long) {
		t.Errorf("history tool-call name = %q, want alias %q", got, ShortenToolName(long))
	}
	// Caller data must be untouched.
	if msgs[0].ToolCalls[0].Function.Name != long {
		t.Errorf("input message was mutated: %q", msgs[0].ToolCalls[0].Function.Name)
	}
}

// TestAliasMessageToolNamesRewritesContentParts covers the tool_use content-part
// path (clients that send assistant tool calls as content blocks rather than in
// ToolCalls).
func TestAliasMessageToolNamesRewritesContentParts(t *testing.T) {
	long := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueTypesMetadata"
	msgs := []Message{
		{Role: "assistant", Content: []ContentPart{
			{Type: "text", Text: "using a tool"},
			{Type: "tool_use", ID: "toolu_1", Name: long, Input: map[string]any{}},
		}},
	}
	out := aliasMessageToolNames(msgs)

	if got := out[0].Content[1].Name; got != ShortenToolName(long) {
		t.Errorf("content-part tool_use name = %q, want alias %q", got, ShortenToolName(long))
	}
	// The non-tool_use part is preserved untouched.
	if out[0].Content[0].Text != "using a tool" {
		t.Errorf("text part was altered: %q", out[0].Content[0].Text)
	}
	// Caller data must be untouched.
	if msgs[0].Content[1].Name != long {
		t.Errorf("input message was mutated: %q", msgs[0].Content[1].Name)
	}
}

// TestShortenToolNameStaysValidUTF8 checks that truncating a multi-byte name
// never leaves an invalid UTF-8 fragment in the alias (which JSON would rewrite
// to U+FFFD, breaking response-path restoration).
func TestShortenToolNameStaysValidUTF8(t *testing.T) {
	// A name whose byte 55 falls inside a multi-byte rune ('é' = 2 bytes).
	name := "mcp__srv__" + strings.Repeat("a", 44) + "é" + strings.Repeat("z", 20)
	if len(name) <= KiroMaxToolNameLen {
		t.Fatalf("precondition: name must exceed the limit, len=%d", len(name))
	}
	alias := ShortenToolName(name)
	if len(alias) > KiroMaxToolNameLen {
		t.Errorf("alias length %d exceeds limit", len(alias))
	}
	if !utf8.ValidString(alias) {
		t.Errorf("alias is not valid UTF-8: %q", alias)
	}
}

// TestBuildKiroPayloadAliasesOverLongToolNames is an end-to-end check that the
// serialized Kiro payload never contains a tool name longer than the limit, and
// that the returned reverse map recovers the original name.
func TestBuildKiroPayloadAliasesOverLongToolNames(t *testing.T) {
	long := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueTypesMetadata"
	req := Request{
		Model:    "claude-sonnet-4",
		Messages: []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}}}},
		Tools:    []Tool{{Name: long, Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}

	cfg := &config.Config{KiroMaxPayloadBytes: 1 << 20}
	body, reverse, err := BuildKiroPayload(context.Background(), cfg, req, "", "kiro-model")
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	alias := ShortenToolName(long)
	if !strings.Contains(string(body), alias) {
		t.Errorf("payload does not contain the aliased tool name %q", alias)
	}
	if strings.Contains(string(body), long) {
		t.Errorf("payload still contains the over-long original tool name")
	}

	if got := RestoreToolName(reverse, alias); got != long {
		t.Errorf("round-trip failed: RestoreToolName(%q) = %q, want %q", alias, got, long)
	}
}
