package converter

import "log/slog"

// stripAllToolContent converts every tool call and tool result in the
// conversation into human-readable text, used when no tools are defined (Kiro
// rejects toolResults without tools). Existing text and images are preserved.
// The boolean reports whether any tool content was converted.
func stripAllToolContent(messages []Message) ([]Message, bool) {
	if len(messages) == 0 {
		return nil, false
	}

	result := make([]Message, 0, len(messages))
	totalCalls, totalResults := 0, 0

	for _, msg := range messages {
		hasCalls := len(msg.ToolCalls) > 0
		hasResults := len(msg.ToolResults) > 0

		if !hasCalls && !hasResults {
			result = append(result, msg)
			continue
		}

		if hasCalls {
			totalCalls += len(msg.ToolCalls)
		}
		if hasResults {
			totalResults += len(msg.ToolResults)
		}

		var contentParts []string
		if existing := ExtractTextContent(msg.Content); existing != "" {
			contentParts = append(contentParts, existing)
		}
		if hasCalls {
			if t := toolCallsToText(msg.ToolCalls); t != "" {
				contentParts = append(contentParts, t)
			}
		}
		if hasResults {
			if t := toolResultsToText(msg.ToolResults); t != "" {
				contentParts = append(contentParts, t)
			}
		}

		content := "(empty placeholder)"
		if len(contentParts) > 0 {
			content = joinDoubleNewline(contentParts)
		}

		result = append(result, Message{
			Role:    msg.Role,
			Content: textContent(content),
			Images:  msg.Images,
		})
	}

	had := totalCalls > 0 || totalResults > 0
	if had {
		slog.Debug("converted tool content to text (no tools defined)",
			"tool_calls", totalCalls, "tool_results", totalResults)
	}
	return result, had
}

// ensureAssistantBeforeToolResults converts orphaned tool results (those without
// a preceding assistant message carrying tool calls) into text appended to the
// message content. The boolean reports whether any tool results were converted.
func ensureAssistantBeforeToolResults(messages []Message) ([]Message, bool) {
	if len(messages) == 0 {
		return nil, false
	}

	result := make([]Message, 0, len(messages))
	convertedAny := false

	for _, msg := range messages {
		if len(msg.ToolResults) > 0 {
			hasPrecedingAssistant := len(result) > 0 &&
				result[len(result)-1].Role == "assistant" &&
				len(result[len(result)-1].ToolCalls) > 0

			if !hasPrecedingAssistant {
				slog.Debug("converting orphaned tool_results to text",
					"count", len(msg.ToolResults))

				resultsText := toolResultsToText(msg.ToolResults)
				original := ExtractTextContent(msg.Content)

				var newContent string
				switch {
				case original != "" && resultsText != "":
					newContent = original + "\n\n" + resultsText
				case resultsText != "":
					newContent = resultsText
				default:
					newContent = original
				}

				result = append(result, Message{
					Role:      msg.Role,
					Content:   textContent(newContent),
					ToolCalls: msg.ToolCalls,
					Images:    msg.Images,
				})
				convertedAny = true
				continue
			}
		}

		result = append(result, msg)
	}

	return result, convertedAny
}

// mergeAdjacentMessages merges consecutive messages with the same role, since
// Kiro does not accept multiple consecutive messages from one role. Content
// blocks, tool calls, tool results and images are concatenated.
func mergeAdjacentMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}

	merged := make([]Message, 0, len(messages))
	mergeCounts := map[string]int{"user": 0, "assistant": 0}
	totalCalls, totalResults := 0, 0

	for _, msg := range messages {
		if len(merged) == 0 {
			merged = append(merged, msg)
			continue
		}

		li := len(merged) - 1
		if msg.Role == merged[li].Role {
			// Concatenate content into a fresh slice to avoid mutating the caller's
			// backing arrays.
			nc := make([]ContentPart, 0, len(merged[li].Content)+len(msg.Content))
			nc = append(nc, merged[li].Content...)
			nc = append(nc, msg.Content...)
			merged[li].Content = nc

			if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
				merged[li].ToolCalls = append(append([]ToolCall{}, merged[li].ToolCalls...), msg.ToolCalls...)
				totalCalls += len(msg.ToolCalls)
			}
			if msg.Role == "user" && len(msg.ToolResults) > 0 {
				merged[li].ToolResults = append(append([]ToolResult{}, merged[li].ToolResults...), msg.ToolResults...)
				totalResults += len(msg.ToolResults)
			}
			if len(msg.Images) > 0 {
				merged[li].Images = append(append([]Image{}, merged[li].Images...), msg.Images...)
			}

			if _, ok := mergeCounts[msg.Role]; ok {
				mergeCounts[msg.Role]++
			}
		} else {
			merged = append(merged, msg)
		}
	}

	total := mergeCounts["user"] + mergeCounts["assistant"]
	if total > 0 {
		slog.Debug("merged adjacent messages",
			"count", total, "user", mergeCounts["user"], "assistant", mergeCounts["assistant"],
			"tool_calls", totalCalls, "tool_results", totalResults)
	}

	return merged
}

// ensureFirstMessageIsUser prepends a synthetic user message when the
// conversation does not start with a user message, as Kiro requires.
func ensureFirstMessageIsUser(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	if messages[0].Role != "user" {
		slog.Debug("prepending synthetic user message (conversation must start with user)",
			"first_role", messages[0].Role)
		synthetic := Message{Role: "user", Content: textContent("(empty placeholder)")}
		return append([]Message{synthetic}, messages...)
	}
	return messages
}

// NormalizeMessageRoles converts any role other than "user" or "assistant" to
// "user", since Kiro only supports those two roles in history. This must run
// before alternation enforcement so consecutive normalized messages are detected.
func NormalizeMessageRoles(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}

	normalized := make([]Message, 0, len(messages))
	converted := 0
	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			slog.Debug("normalizing unknown role to user", "role", msg.Role)
			m := msg
			m.Role = "user"
			normalized = append(normalized, m)
			converted++
		} else {
			normalized = append(normalized, msg)
		}
	}
	if converted > 0 {
		slog.Debug("normalized messages with unknown roles to user", "count", converted)
	}
	return normalized
}

// ensureAlternatingRoles inserts synthetic assistant messages between consecutive
// user messages so roles alternate, as Kiro requires.
func ensureAlternatingRoles(messages []Message) []Message {
	if len(messages) < 2 {
		return messages
	}

	result := make([]Message, 0, len(messages))
	result = append(result, messages[0])
	synthetic := 0

	for _, msg := range messages[1:] {
		prevRole := result[len(result)-1].Role
		if msg.Role == "user" && prevRole == "user" {
			result = append(result, Message{Role: "assistant", Content: textContent("(empty placeholder)")})
			synthetic++
		}
		result = append(result, msg)
	}

	if synthetic > 0 {
		slog.Debug("inserted synthetic assistant messages to ensure alternation", "count", synthetic)
	}
	return result
}

// buildKiroHistory converts unified messages into Kiro history entries
// (userInputMessage / assistantResponseMessage). All roles must already be
// normalized to user/assistant.
func buildKiroHistory(messages []Message, modelID string) []any {
	history := make([]any, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			content := ExtractTextContent(msg.Content)
			if content == "" {
				content = "(empty placeholder)"
			}

			userInput := map[string]any{
				"content": content,
				"modelId": modelID,
				"origin":  "AI_EDITOR",
			}

			images := msg.Images
			if len(images) == 0 {
				images = extractImagesFromContent(msg.Content)
			}
			if kiroImages := convertImagesToKiroFormat(images); len(kiroImages) > 0 {
				userInput["images"] = kiroImages
			}

			ctx := map[string]any{}
			if len(msg.ToolResults) > 0 {
				if kr := convertToolResultsToKiroFormat(msg.ToolResults); len(kr) > 0 {
					ctx["toolResults"] = kr
				}
			} else if tr := extractToolResultsFromContent(msg.Content); len(tr) > 0 {
				ctx["toolResults"] = tr
			}
			if len(ctx) > 0 {
				userInput["userInputMessageContext"] = ctx
			}

			history = append(history, map[string]any{"userInputMessage": userInput})

		case "assistant":
			content := ExtractTextContent(msg.Content)
			if content == "" {
				content = "(empty placeholder)"
			}

			assistant := map[string]any{"content": content}
			if toolUses := extractToolUsesFromMessage(msg.Content, msg.ToolCalls); len(toolUses) > 0 {
				assistant["toolUses"] = toolUses
			}

			history = append(history, map[string]any{"assistantResponseMessage": assistant})
		}
	}

	return history
}

// joinDoubleNewline joins parts with a blank line between them.
func joinDoubleNewline(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n\n"
		}
		out += p
	}
	return out
}
