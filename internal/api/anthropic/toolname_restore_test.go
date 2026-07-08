package anthropic

import (
	"testing"

	"github.com/RobinGasi/kiro-proxy/internal/awsstream"
	"github.com/RobinGasi/kiro-proxy/internal/converter"
	"github.com/RobinGasi/kiro-proxy/internal/streaming"
)

// TestBuildMessagesResponseRestoresToolName verifies that a tool_use block whose
// name was aliased for Kiro is restored to the original name in the non-streaming
// response, using the reverse map returned by BuildKiroPayload.
func TestBuildMessagesResponseRestoresToolName(t *testing.T) {
	original := "mcp__plugin_atlassian_atlassian__getJiraProjectIssueTypesMetadata"
	alias := converter.ShortenToolName(original)
	if alias == original {
		t.Fatalf("precondition: name should be aliased, len=%d", len(original))
	}
	reverse := map[string]string{alias: original}

	// The model's response references the aliased name (as Kiro would echo it).
	result := &streaming.StreamResult{
		ToolCalls: []awsstream.ToolCall{{
			ID:       "toolu_1",
			Type:     "function",
			Function: awsstream.ToolCallFunction{Name: alias, Arguments: "{}"},
		}},
	}

	resp := buildMessagesResponse(result, "claude-sonnet-4", 10, 200000, false, reverse)

	var found bool
	for _, block := range resp.Content {
		tu, ok := block.(ToolUseBlock)
		if !ok {
			continue
		}
		found = true
		if tu.Name != original {
			t.Errorf("tool_use name = %q, want restored original %q", tu.Name, original)
		}
	}
	if !found {
		t.Fatal("no tool_use block in response")
	}
}
