package openai

import (
	"encoding/json"
	"strings"

	"github.com/RobinGasi/kiro-proxy/internal/converter"
)

// toConverterRequest builds a converter.Request from a ChatCompletionRequest.
//
// System/developer messages are folded into the system prompt, tool-role
// messages are accumulated and flushed as a user message carrying tool_results,
// assistant tool_calls are extracted, and images embedded in user or tool
// content are carried through. Unknown roles are left for
// converter.NormalizeMessageRoles to coerce.
func toConverterRequest(req *ChatCompletionRequest) converter.Request {
	system, messages := convertMessages(req.Messages)
	messages = converter.NormalizeMessageRoles(messages)

	out := converter.Request{
		Model:    req.Model,
		System:   system,
		Messages: messages,
		Tools:    convertTools(req.Tools),
		Generation: converter.GenerationConfig{
			MaxTokens:   firstNonNilInt(req.MaxCompletion, req.MaxTokens),
			Temperature: req.Temperature, TopP: req.TopP,
		},
		Thinking: extractThinking(req),
	}
	if len(req.ToolChoice) > 0 {
		var tc struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			if tc.Name == "" {
				tc.Name = tc.Function.Name
			}
			out.ToolChoice = converter.ToolChoice{Type: strings.ToLower(tc.Type), Name: tc.Name}
		}
	}
	return out
}

func firstNonNilInt(values ...*int) *int {
	for _, value := range values {
		if value != nil && *value > 0 {
			return value
		}
	}
	return nil
}

// convertMessages converts OpenAI messages to (systemPrompt, unifiedMessages).
func convertMessages(msgs []ChatMessage) (string, []converter.Message) {
	var sys []string
	var nonSystem []ChatMessage
	for _, m := range msgs {
		if strings.EqualFold(strings.TrimSpace(m.Role), "system") {
			if t := m.Content.Text(); t != "" {
				sys = append(sys, t)
			}
			continue
		}
		nonSystem = append(nonSystem, m)
	}

	var processed []converter.Message
	var pendingResults []converter.ToolResult
	var pendingImages []converter.Image

	flush := func() {
		if len(pendingResults) == 0 {
			return
		}
		processed = append(processed, converter.Message{
			Role:        "user",
			ToolResults: pendingResults,
			Images:      pendingImages,
		})
		pendingResults = nil
		pendingImages = nil
	}

	for _, m := range nonSystem {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "tool" {
			pendingResults = append(pendingResults, converter.ToolResult{
				ToolUseID: m.ToolCallID,
				Content:   contentOrEmpty(m.Content.Text()),
			})
			pendingImages = append(pendingImages, extractImages(m.Content)...)
			continue
		}

		// A non-tool message ends any run of tool results.
		flush()

		um := converter.Message{
			Role:    role,
			Content: textPart(m.Content.Text()),
		}
		switch role {
		case "assistant":
			um.ToolCalls = extractToolCalls(m.ToolCalls)
		case "user":
			um.ToolResults = extractToolResults(m.Content)
			um.Images = extractImages(m.Content)
		}
		processed = append(processed, um)
	}

	flush()

	return strings.TrimSpace(strings.Join(sys, "\n")), processed
}

// contentOrEmpty applies the "(empty result)" fallback for tool results.
func contentOrEmpty(s string) string {
	if s == "" {
		return "(empty result)"
	}
	return s
}

// textPart wraps a non-empty string as a single text content block.
func textPart(s string) []converter.ContentPart {
	if s == "" {
		return nil
	}
	return []converter.ContentPart{{Type: "text", Text: s}}
}

// extractToolCalls converts OpenAI assistant tool_calls to unified form. OpenAI
// arguments are a JSON string, kept as a string in FunctionCall.Arguments.
func extractToolCalls(calls []ToolCallBlock) []converter.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]converter.ToolCall, 0, len(calls))
	for _, tc := range calls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		out = append(out, converter.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: converter.FunctionCall{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		})
	}
	return out
}

// extractToolResults pulls Anthropic-style tool_result blocks out of OpenAI user
// content (some clients embed them there).
func extractToolResults(c ContentField) []converter.ToolResult {
	if c.IsString() {
		return nil
	}
	var out []converter.ToolResult
	for _, it := range c.Items() {
		if it.Type != "tool_result" {
			continue
		}
		out = append(out, converter.ToolResult{
			ToolUseID: it.ToolUseID,
			Content:   contentOrEmpty(rawText(it.Content)),
		})
	}
	return out
}

// extractImages pulls image_url data URLs out of content items.
func extractImages(c ContentField) []converter.Image {
	if c.IsString() {
		return nil
	}
	var out []converter.Image
	for _, it := range c.Items() {
		if it.Type != "image_url" || it.ImageURL == nil {
			continue
		}
		media, data := parseDataURL(it.ImageURL.URL)
		if data == "" {
			continue
		}
		out = append(out, converter.Image{MediaType: media, Data: data})
	}
	return out
}

// parseDataURL splits a "data:<media>;base64,<data>" URL into media type and
// base64 data. A bare URL or non-data URL yields empty data (skipped).
func parseDataURL(url string) (mediaType, data string) {
	if !strings.HasPrefix(url, "data:") {
		return "", ""
	}
	rest := strings.TrimPrefix(url, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", ""
	}
	meta := rest[:comma]
	data = rest[comma+1:]
	mediaType = "image/png"
	if semi := strings.IndexByte(meta, ';'); semi >= 0 {
		if m := meta[:semi]; m != "" {
			mediaType = m
		}
	} else if meta != "" {
		mediaType = meta
	}
	return mediaType, data
}

// rawText extracts plain text from a tool_result content field that may be a
// string or an array of {"type":"text","text":...} blocks.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []ContentItem
	if json.Unmarshal(raw, &items) == nil {
		var b strings.Builder
		for _, it := range items {
			if it.Type == "text" && it.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(it.Text)
			}
		}
		return b.String()
	}
	return ""
}

// convertTools converts OpenAI tools to unified tools. The standard
// {"function":{...}} shape wins; the flat Cursor-style shape is a fallback.
// Schemas are sanitized via converter.SanitizeToolSchema.
func convertTools(tools []Tool) []converter.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]converter.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		switch {
		case t.Function != nil:
			out = append(out, converter.Tool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: converter.SanitizeToolSchema(t.Function.Parameters),
				Type:        "function",
			})
		case t.Name != "":
			out = append(out, converter.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: converter.SanitizeToolSchema(t.InputSchema),
				Type:        "function",
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reasoningEffortBudget maps a reasoning_effort level to a thinking budget as a
// percentage of max output tokens.
func reasoningEffortBudget(maxTokens int, effort string) int {
	pct := map[string]float64{
		"minimal": 0.10,
		"low":     0.20,
		"medium":  0.50,
		"high":    0.80,
		"xhigh":   0.95,
	}
	p, ok := pct[effort]
	if !ok {
		return 0
	}
	return int(float64(maxTokens) * p)
}

// extractThinking builds the unified ThinkingConfig from reasoning_effort.
func extractThinking(req *ChatCompletionRequest) converter.ThinkingConfig {
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort == "" {
		// No reasoning_effort: enable with default budget.
		return converter.ThinkingConfig{Enabled: true}
	}
	if effort == "none" {
		return converter.ThinkingConfig{Enabled: false}
	}
	maxTokens := 0
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletion != nil {
		maxTokens = *req.MaxCompletion
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	budget := reasoningEffortBudget(maxTokens, effort)
	cfg := converter.ThinkingConfig{Enabled: true, Effort: effort}
	if budget > 0 {
		cfg.BudgetTokens = &budget
	}
	return cfg
}
