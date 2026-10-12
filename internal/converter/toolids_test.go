package converter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

// conformingToolUseID mirrors the contract Kiro enforces on toolUseId: 1..64
// bytes of [A-Za-z0-9_-]. A violation is answered with a flat HTTP 400
// "Invalid tool use format.", so this is the invariant every id crossing the
// Kiro boundary must satisfy.
func conformingToolUseID(id string) bool {
	if len(id) == 0 || len(id) > KiroMaxToolUseIDLen {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '_', r == '-':
			return false
		default:
			return true
		}
	}) < 0
}

func TestSanitizeToolUseIDPassesConformingIDsThrough(t *testing.T) {
	for _, id := range []string{
		"toolu_01A2b3C4",
		"call_z5D9RvvsfQpXxRG9x1Cd8Nmf",
		"call-ad238945-488d-4ba7-ab86-c435d0b82604",
		"a",
		strings.Repeat("x", KiroMaxToolUseIDLen),
	} {
		if got := SanitizeToolUseID(id); got != id {
			t.Errorf("SanitizeToolUseID(%q) = %q, want it unchanged", id, got)
		}
	}
}

func TestSanitizeToolUseIDRepairsNonConformingIDs(t *testing.T) {
	// The embedded-newline case is the one observed in the wild: two provider
	// ids spliced together by a resumed/repaired conversation, which Kiro
	// rejected with "Invalid tool use format." for every subsequent request.
	cases := map[string]string{
		"newline":       "call-36ac832b-6f80-4418-bd94-9d218fc71bc7-24\nfc_e6a354bc-95bd-9320-835f-98781c977c75_0",
		"too long":      strings.Repeat("x", KiroMaxToolUseIDLen+1),
		"space":         "call abc",
		"punctuation":   "call:abc/def#1",
		"non-ascii":     "call_✅_done",
		"newline only":  "\n",
		"exactly 65 ok": strings.Repeat("a", 65),
	}
	for name, id := range cases {
		got := SanitizeToolUseID(id)
		if !conformingToolUseID(got) {
			t.Errorf("%s: SanitizeToolUseID(%q) = %q, which still violates the Kiro contract", name, id, got)
		}
		if got == id {
			t.Errorf("%s: SanitizeToolUseID(%q) returned it unchanged", name, id)
		}
	}
}

// TestSanitizeToolUseIDIsDeterministic covers the property that keeps a tool
// result paired with its tool use: both sides are sanitized independently, so
// the function must be pure.
func TestSanitizeToolUseIDIsDeterministic(t *testing.T) {
	id := "call-abc\nfc_def_0"
	if a, b := SanitizeToolUseID(id), SanitizeToolUseID(id); a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
}

// TestSanitizeToolUseIDDistinguishesIDsDifferingOnlyInInvalidBytes guards the
// reason the digest is taken over the original id rather than the cleaned one:
// otherwise two distinct tool calls would collapse onto a single toolUseId and
// their results would be cross-wired.
func TestSanitizeToolUseIDDistinguishesIDsDifferingOnlyInInvalidBytes(t *testing.T) {
	a := SanitizeToolUseID("call\nabc")
	b := SanitizeToolUseID("call abc")
	if a == b {
		t.Errorf("distinct originals collapsed onto %q", a)
	}
}

func TestSanitizeToolUseIDKeepsEmptyEmpty(t *testing.T) {
	if got := SanitizeToolUseID(""); got != "" {
		t.Errorf("SanitizeToolUseID(\"\") = %q, want \"\"", got)
	}
}

// TestBuildKiroPayloadSanitizesToolUseIDs is the end-to-end guard: a history
// carrying a malformed tool-call id must not reach Kiro verbatim, and the
// matching tool result must land on the same sanitized id so the pairing
// survives.
func TestBuildKiroPayloadSanitizesToolUseIDs(t *testing.T) {
	const badID = "call-dc30a1d4-6fa7-424d-a764-13a757fd32ea-3\nfc_6d96b0a1-3a02-9b3d-80d6-dd1928c4708d_0"

	req := Request{
		Model: "claude-sonnet-4",
		Tools: []Tool{{Name: "read", Description: "read a file"}},
		Messages: []Message{
			{Role: "user", Content: textContent("read main.go")},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:       badID,
				Type:     "function",
				Function: FunctionCall{Name: "read", Arguments: `{"path":"main.go"}`},
			}}},
			{Role: "user", ToolResults: []ToolResult{{ToolUseID: badID, Content: "package main"}}},
		},
	}

	body, _, err := BuildKiroPayload(context.Background(), config.Load(), req, "", "model-x")
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	want := SanitizeToolUseID(badID)
	if !conformingToolUseID(want) {
		t.Fatalf("sanitized id %q does not conform", want)
	}

	ids := collectToolUseIDs(t, body)
	if len(ids) == 0 {
		t.Fatal("payload carried no toolUseIds")
	}
	for _, got := range ids {
		if !conformingToolUseID(got) {
			t.Errorf("payload carries non-conforming toolUseId %q", got)
		}
		if got != want {
			t.Errorf("toolUseId = %q, want the sanitized %q", got, want)
		}
	}
	if len(ids) != 2 {
		t.Errorf("expected the tool use and its result to be paired, got %d ids: %v", len(ids), ids)
	}
}

// collectToolUseIDs walks the decoded payload and returns every "toolUseId"
// value, wherever it appears (history toolUses, current-message toolResults).
func collectToolUseIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	var ids []string
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, child := range n {
				if k == "toolUseId" {
					if s, ok := child.(string); ok {
						ids = append(ids, s)
						continue
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(root)
	return ids
}
