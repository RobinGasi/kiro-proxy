package converter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

func TestBuildKiroPayloadUsesNativeClaudeReasoningWithoutXML(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"output_config":{"properties":{"effort":{"enum":["low","medium","high","max"]}}},"thinking":{"properties":{"display":{"enum":["summarized"]}}}}}`)
	req := Request{
		Model:              "claude-opus-5.5",
		Messages:           []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "hello"}}}},
		Thinking:           ThinkingConfig{Enabled: true, Effort: "xhigh"},
		ModelRequestSchema: schema,
	}
	body, _, err := BuildKiroPayload(context.Background(), &config.Config{FakeReasoningEnabled: true, FakeReasoningMaxTokens: 10_000, KiroMaxPayloadBytes: 1 << 20}, req, "", req.Model)
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}
	if strings.Contains(string(body), "thinking_mode") || strings.Contains(string(body), "thinking_instruction") {
		t.Fatalf("native reasoning payload contains emulated XML: %s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	fields := payload["additionalModelRequestFields"].(map[string]any)
	if fields["output_config"].(map[string]any)["effort"] != "max" {
		t.Fatalf("native fields = %#v, want nearest supported effort max", fields)
	}
	thinking := fields["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Fatalf("thinking fields = %#v", thinking)
	}
}

func TestBuildKiroPayloadCopiesOnlyAdvertisedGenerationFields(t *testing.T) {
	maxTokens := 64000
	temperature := 0.2
	topP := 0.9
	schema := json.RawMessage(`{"properties":{"output_config":{"properties":{"effort":{"enum":["medium"]}}},"thinking":{"properties":{}},"maxOutputTokens":{},"temperature":{}}}`)
	req := Request{
		Model:              "claude-opus-5.5",
		Messages:           []Message{{Role: "user", Content: textContent("hello")}},
		Thinking:           ThinkingConfig{Enabled: true, Effort: "medium"},
		Generation:         GenerationConfig{MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP},
		ModelRequestSchema: schema,
	}
	body, _, err := BuildKiroPayload(context.Background(), &config.Config{KiroMaxPayloadBytes: 1 << 20}, req, "", req.Model)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	fields := payload["additionalModelRequestFields"].(map[string]any)
	if fields["maxOutputTokens"] != float64(64000) || fields["temperature"] != 0.2 {
		t.Fatalf("generation fields = %#v", fields)
	}
	if _, ok := fields["top_p"]; ok {
		t.Fatalf("unadvertised top_p leaked upstream: %#v", fields)
	}
}

func TestBuildKiroPayloadRecoveryDisablesAllReasoning(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"output_config":{"properties":{"effort":{"enum":["high"]}}}}}`)
	req := Request{
		Model:              "claude-opus-5.5",
		Messages:           []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "hello"}}}},
		Thinking:           ThinkingConfig{Enabled: false, Effort: "high"},
		ModelRequestSchema: schema,
	}
	body, _, err := BuildKiroPayload(context.Background(), &config.Config{FakeReasoningEnabled: true, FakeReasoningMaxTokens: 10_000, KiroMaxPayloadBytes: 1 << 20}, req, "", req.Model)
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}
	if strings.Contains(string(body), "thinking_mode") || strings.Contains(string(body), "additionalModelRequestFields") {
		t.Fatalf("recovery payload still requests reasoning: %s", body)
	}
}
