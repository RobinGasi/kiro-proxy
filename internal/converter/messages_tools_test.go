package converter

import "testing"

func TestEnsureAssistantBeforeToolResultsConvertsMismatchedResult(t *testing.T) {
	messages := []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID: "toolu_previous",
				Function: FunctionCall{
					Name:      "Bash",
					Arguments: map[string]any{},
				},
			}},
		},
		{
			Role: "user",
			ToolResults: []ToolResult{{
				ToolUseID: "toolu_orphaned",
				Content:   "file contents",
			}},
		},
	}

	got, converted := ensureAssistantBeforeToolResults(messages)
	if !converted {
		t.Fatal("expected mismatched tool_result to be converted")
	}
	if len(got[1].ToolResults) != 0 {
		t.Fatalf("orphaned tool_result was left in the message: %+v", got[1].ToolResults)
	}
	if text := ExtractTextContent(got[1].Content); text == "" {
		t.Fatal("expected orphaned tool_result content to be preserved as text")
	}
}

func TestEnsureAssistantBeforeToolResultsKeepsOnlyMatchingResults(t *testing.T) {
	messages := []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:       "toolu_matching",
				Function: FunctionCall{Name: "Read"},
			}},
		},
		{
			Role: "user",
			ToolResults: []ToolResult{
				{ToolUseID: "toolu_matching", Content: "matching result"},
				{ToolUseID: "toolu_orphaned", Content: "orphaned result"},
			},
		},
	}

	got, converted := ensureAssistantBeforeToolResults(messages)
	if !converted {
		t.Fatal("expected orphaned tool_result to be converted")
	}
	if len(got[1].ToolResults) != 1 || got[1].ToolResults[0].ToolUseID != "toolu_matching" {
		t.Fatalf("matching tool_result was not preserved: %+v", got[1].ToolResults)
	}
	if text := ExtractTextContent(got[1].Content); text == "" {
		t.Fatal("expected orphaned tool_result content to be preserved as text")
	}
}
