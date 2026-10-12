package converter

import (
	"context"
	"encoding/json"
	"testing"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
)

func TestBuildKiroPayloadPreservesFailedToolResult(t *testing.T) {
	req := Request{
		Model: "claude-opus-5.5",
		Messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Function: FunctionCall{Name: "bash", Arguments: map[string]any{}}}}},
			{Role: "user", ToolResults: []ToolResult{{ToolUseID: "toolu_1", Content: "exit 1", IsError: true}}},
		},
		Tools: []Tool{{Name: "bash", Description: "run", InputSchema: map[string]any{"type": "object"}}},
	}
	body, _, err := BuildKiroPayload(context.Background(), &config.Config{KiroMaxPayloadBytes: 1 << 20}, req, "", req.Model)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	current := payload["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	results := current["userInputMessageContext"].(map[string]any)["toolResults"].([]any)
	if got := results[0].(map[string]any)["status"]; got != "error" {
		t.Fatalf("tool status = %v, want error", got)
	}
}

func TestBuildKiroPayloadHonorsToolChoiceNone(t *testing.T) {
	req := Request{
		Model:      "claude-opus-5.5",
		Messages:   []Message{{Role: "user", Content: textContent("answer only")}},
		Tools:      []Tool{{Name: "bash", Description: "run", InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: ToolChoice{Type: "none"},
	}
	body, _, err := BuildKiroPayload(context.Background(), &config.Config{KiroMaxPayloadBytes: 1 << 20}, req, "", req.Model)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "" {
		t.Fatal("empty payload")
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	current := payload["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if ctx, ok := current["userInputMessageContext"].(map[string]any); ok && ctx["tools"] != nil {
		t.Fatalf("tool_choice none retained tools: %#v", ctx["tools"])
	}
}
