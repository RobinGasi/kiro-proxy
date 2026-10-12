package converter

import (
	"fmt"
	"log/slog"
	"strings"
)

// processToolsWithLongDescriptions moves tool descriptions longer than maxLen out
// of the tool definition and into a documentation block destined for the system
// prompt, leaving a short reference behind. When maxLen <= 0 the limit is
// disabled and tools are returned unchanged.
//
// It returns the processed tools and the documentation string to append to the
// system prompt (empty when every description fits).
func processToolsWithLongDescriptions(tools []Tool, maxLen int) ([]Tool, string) {
	if len(tools) == 0 {
		return nil, ""
	}
	if maxLen <= 0 {
		return tools, ""
	}

	var docParts []string
	processed := make([]Tool, 0, len(tools))

	for _, tool := range tools {
		description := tool.Description
		if len(description) <= maxLen {
			processed = append(processed, tool)
			continue
		}

		slog.Debug("tool has long description, moving to system prompt",
			"tool", tool.Name, "length", len(description), "limit", maxLen)

		docParts = append(docParts, fmt.Sprintf("## Tool: %s\n\n%s", tool.Name, description))

		ref := tool
		ref.Description = fmt.Sprintf("[Full documentation in system prompt under '## Tool: %s']", tool.Name)
		processed = append(processed, ref)
	}

	var doc string
	if len(docParts) > 0 {
		doc = "\n\n---\n" +
			"# Tool Documentation\n" +
			"The following tools have detailed documentation that couldn't fit in the tool definition.\n\n" +
			strings.Join(docParts, "\n\n---\n\n")
	}

	if len(processed) == 0 {
		return nil, doc
	}
	return processed, doc
}

func applyToolChoice(tools []Tool, choice ToolChoice) ([]Tool, string) {
	if len(tools) == 0 {
		return nil, ""
	}
	var instruction string
	switch choice.Type {
	case "none":
		return nil, ""
	case "tool", "function":
		selected := make([]Tool, 0, 1)
		for _, tool := range tools {
			if tool.Name == choice.Name {
				selected = append(selected, tool)
				break
			}
		}
		if len(selected) == 0 {
			return nil, ""
		}
		instruction = fmt.Sprintf("\n\nYou MUST use the `%s` tool for this response.", choice.Name)
		tools = selected
	case "any":
		instruction = "\n\nYou MUST use one of the available tools for this response."
	}
	if choice.DisableParallelUse {
		instruction += " Call at most one tool in this response."
	}
	return tools, instruction
}

// SelectedToolNames returns the exact tool names Kiro sees after tool-choice
// filtering and deterministic length aliasing. Stream fallback parsing uses this
// as its allow-list; a text-emitted call can never invoke an unadvertised tool.
func SelectedToolNames(tools []Tool, choice ToolChoice) []string {
	selected, _ := applyToolChoice(tools, choice)
	if len(selected) == 0 {
		return nil
	}
	names := make([]string, 0, len(selected))
	for _, tool := range selected {
		names = append(names, ShortenToolName(tool.Name))
	}
	return names
}

// convertToolsToKiroFormat converts unified tools into Kiro toolSpecification
// entries, sanitizing each input schema and substituting a placeholder for empty
// descriptions.
func convertToolsToKiroFormat(tools []Tool) []any {
	if len(tools) == 0 {
		return nil
	}

	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		sanitized := SanitizeToolSchema(tool.InputSchema)

		description := tool.Description
		if strings.TrimSpace(description) == "" {
			description = "Tool: " + tool.Name
			slog.Debug("tool has empty description, using placeholder", "tool", tool.Name)
		}

		out = append(out, map[string]any{
			"toolSpecification": map[string]any{
				"name":        tool.Name,
				"description": description,
				"inputSchema": map[string]any{"json": sanitized},
			},
		})
	}
	return out
}

// convertImagesToKiroFormat converts unified images into Kiro image entries
// ({"format": ..., "source": {"bytes": ...}}), stripping any data-URL prefix and
// deriving the format from the media type. Images with empty data are skipped.
func convertImagesToKiroFormat(images []Image) []any {
	if len(images) == 0 {
		return nil
	}

	out := make([]any, 0, len(images))
	for _, img := range images {
		mediaType := img.MediaType
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		data := img.Data
		if data == "" {
			slog.Warn("skipping image with empty data")
			continue
		}

		// Strip a "data:<media-type>;base64,<...>" prefix if present; Kiro expects
		// bare base64.
		if strings.HasPrefix(data, "data:") {
			if comma := strings.IndexByte(data, ','); comma >= 0 {
				header := data[:comma]
				actual := data[comma+1:]
				mediaPart := header
				if semi := strings.IndexByte(header, ';'); semi >= 0 {
					mediaPart = header[:semi]
				}
				extracted := strings.TrimPrefix(mediaPart, "data:")
				if extracted != "" {
					mediaType = extracted
				}
				data = actual
				slog.Debug("stripped data URL prefix", "media_type", mediaType)
			} else {
				slog.Warn("failed to parse data URL prefix")
			}
		}

		format := mediaType
		if slash := strings.LastIndexByte(mediaType, '/'); slash >= 0 {
			format = mediaType[slash+1:]
		}

		out = append(out, map[string]any{
			"format": format,
			"source": map[string]any{"bytes": data},
		})
	}

	if len(out) > 0 {
		slog.Debug("converted images to Kiro format", "count", len(out))
	}
	return out
}

// convertToolResultsToKiroFormat converts unified tool results into Kiro
// toolResults entries. Empty content becomes "(empty result)".
func convertToolResultsToKiroFormat(results []ToolResult) []any {
	out := make([]any, 0, len(results))
	for _, tr := range results {
		text := toolResultText(tr.Content)
		if text == "" {
			text = "(empty result)"
		}
		status := "success"
		if tr.IsError {
			status = "error"
		}
		out = append(out, map[string]any{
			"content":   []any{map[string]any{"text": text}},
			"status":    status,
			"toolUseId": SanitizeToolUseID(tr.ToolUseID),
		})
	}
	return out
}

// extractToolResultsFromContent extracts tool_result blocks embedded in a content
// list and converts them to Kiro toolResults form.
func extractToolResultsFromContent(parts []ContentPart) []any {
	var out []any
	for _, p := range parts {
		if p.Type != "tool_result" {
			continue
		}
		text := toolResultText(p.Content)
		if text == "" {
			text = "(empty result)"
		}
		out = append(out, map[string]any{
			"content":   []any{map[string]any{"text": text}},
			"status":    "success",
			"toolUseId": SanitizeToolUseID(p.ToolUseID),
		})
	}
	return out
}

// extractToolUsesFromMessage gathers tool uses from the assistant tool-call list
// and from tool_use blocks embedded in content, converting them to Kiro toolUses
// form.
func extractToolUsesFromMessage(parts []ContentPart, toolCalls []ToolCall) []any {
	var out []any

	for _, tc := range toolCalls {
		out = append(out, map[string]any{
			"name":      tc.Function.Name,
			"input":     argsToInput(tc.Function.Arguments),
			"toolUseId": SanitizeToolUseID(tc.ID),
		})
	}

	for _, p := range parts {
		if p.Type != "tool_use" {
			continue
		}
		input := p.Input
		if input == nil {
			input = map[string]any{}
		}
		out = append(out, map[string]any{
			"name":      p.Name,
			"input":     input,
			"toolUseId": SanitizeToolUseID(p.ID),
		})
	}

	return out
}

// toolCallsToText renders tool calls as human-readable text, used when stripping
// tool content because no tools are defined.
func toolCallsToText(toolCalls []ToolCall) string {
	if len(toolCalls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		name := tc.Function.Name
		if name == "" {
			name = "unknown"
		}
		arguments := argsToText(tc.Function.Arguments)
		if tc.ID != "" {
			parts = append(parts, fmt.Sprintf("[Tool: %s (%s)]\n%s", name, tc.ID, arguments))
		} else {
			parts = append(parts, fmt.Sprintf("[Tool: %s]\n%s", name, arguments))
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolResultsToText renders tool results as human-readable text, used when
// stripping tool content because no tools are defined.
func toolResultsToText(results []ToolResult) string {
	if len(results) == 0 {
		return ""
	}
	parts := make([]string, 0, len(results))
	for _, tr := range results {
		text := toolResultText(tr.Content)
		if text == "" {
			text = "(empty result)"
		}
		prefix := "[Tool Result"
		if tr.IsError {
			prefix = "[Tool Error"
		}
		if tr.ToolUseID != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)]\n%s", prefix, tr.ToolUseID, text))
		} else {
			parts = append(parts, fmt.Sprintf("%s]\n%s", prefix, text))
		}
	}
	return strings.Join(parts, "\n\n")
}
