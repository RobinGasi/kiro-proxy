package anthropic

import (
	"encoding/json"
	"testing"
)

func TestBuildConverterRequestPreservesOutputEffort(t *testing.T) {
	var req MessagesRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-opus-5-5",
		"max_tokens":64000,
		"output_config":{"effort":"xhigh"},
		"thinking":{"type":"adaptive"},
		"messages":[{"role":"user","content":"fix it"}]
	}`), &req); err != nil {
		t.Fatal(err)
	}

	got := buildConverterRequest(&req).Thinking
	if !got.Enabled || got.Effort != "xhigh" {
		t.Fatalf("thinking = %+v, want enabled xhigh", got)
	}
}

func TestBuildConverterRequestDerivesEffortFromThinkingBudget(t *testing.T) {
	var req MessagesRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-opus-5",
		"max_tokens":10000,
		"thinking":{"type":"enabled","budget_tokens":8000},
		"messages":[{"role":"user","content":"fix it"}]
	}`), &req); err != nil {
		t.Fatal(err)
	}

	got := buildConverterRequest(&req).Thinking
	if !got.Enabled || got.Effort != "high" {
		t.Fatalf("thinking = %+v, want enabled high", got)
	}
}

func TestBuildConverterRequestUsesModelEffortDefault(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5.5": "medium",
		"claude-opus-5":   "high",
	} {
		req := MessagesRequest{Model: model, Messages: []Message{{Role: "user"}}}
		if got := buildConverterRequest(&req).Thinking.Effort; got != want {
			t.Errorf("model %q effort = %q, want %q", model, got, want)
		}
	}
}
