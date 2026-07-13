package converter

import "testing"

// TestMergeAdjacentMessagesMergesImages guards the fix for images being dropped
// when two consecutive same-role messages are merged (e.g. a tool_result user
// turn immediately followed by a user turn carrying a screenshot).
func TestMergeAdjacentMessagesMergesImages(t *testing.T) {
	msgs := []Message{
		{
			Role:        "user",
			ToolResults: []ToolResult{{ToolUseID: "t1", Content: "ok"}},
			Images:      []Image{{MediaType: "image/png", Data: "AAA"}},
		},
		{
			Role:    "user",
			Content: []ContentPart{{Type: "text", Text: "and this screenshot"}},
			Images:  []Image{{MediaType: "image/png", Data: "BBB"}},
		},
	}

	merged := mergeAdjacentMessages(msgs)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged message, got %d", len(merged))
	}
	if len(merged[0].Images) != 2 {
		t.Fatalf("expected 2 images after merge, got %d", len(merged[0].Images))
	}
	if merged[0].Images[0].Data != "AAA" || merged[0].Images[1].Data != "BBB" {
		t.Fatalf("images not preserved in order: %+v", merged[0].Images)
	}
}
