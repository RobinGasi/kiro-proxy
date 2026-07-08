package anthropic

import (
	"encoding/json"
	"testing"
)

// Regression for the headline bug: a request with an inline role:"system"
// message (as newer Claude Code emits) must parse and merge, never 422.
func TestInlineSystemMessageMerged(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-8[1m]",
		"max_tokens": 1024,
		"system": "top-level system",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "system", "content": "inline system from SessionStart hook"},
			{"role": "assistant", "content": [{"type":"text","text":"hello"}]},
			{"role": "user", "content": [{"type":"text","text":"continue"}]}
		]
	}`)

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal failed (this is the 422 bug): %v", err)
	}

	norm := req.NormalizeRoles()

	wantSys := "top-level system\n\ninline system from SessionStart hook"
	if norm.System != wantSys {
		t.Errorf("merged system = %q, want %q", norm.System, wantSys)
	}
	if len(norm.Turns) != 3 {
		t.Fatalf("turns = %d, want 3 (system pulled out)", len(norm.Turns))
	}
	for _, m := range norm.Turns {
		if m.Role != "user" && m.Role != "assistant" {
			t.Errorf("unexpected turn role %q", m.Role)
		}
	}
	if norm.Turns[0].Content.Text() != "hi" {
		t.Errorf("first turn text = %q", norm.Turns[0].Content.Text())
	}
}

func TestUnknownRoleCoercedNotRejected(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"tool","content":"x"}]}`)
	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	norm := req.NormalizeRoles()
	if len(norm.Turns) != 1 || norm.Turns[0].Role != "user" {
		t.Errorf("unknown role not coerced to user: %+v", norm.Turns)
	}
}

func TestStringAndBlockContentText(t *testing.T) {
	var c ContentField
	if err := json.Unmarshal([]byte(`"plain"`), &c); err != nil || c.Text() != "plain" {
		t.Errorf("string content: %v / %q", err, c.Text())
	}
	var c2 ContentField
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"a"},{"type":"image","source":{}},{"type":"text","text":"b"}]`), &c2); err != nil {
		t.Fatalf("blocks: %v", err)
	}
	if c2.Text() != "a\nb" {
		t.Errorf("block text = %q, want %q", c2.Text(), "a\nb")
	}
}
