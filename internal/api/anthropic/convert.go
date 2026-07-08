package anthropic

import (
	"encoding/json"
	"log/slog"

	"github.com/RobinGasi/kiro-proxy/internal/converter"
)

// buildConverterRequest turns a parsed Anthropic *MessagesRequest into the
// API-agnostic converter.Request consumed by converter.BuildKiroPayload.
//
// The headline fix lives here: NormalizeRoles folds any inline role:"system"
// (or "developer") messages into the system prompt and coerces unknown roles to
// "user", so newer Claude Code clients that place a system message inside
// messages[] no longer trigger an upstream HTTP 422.
//
// Each turn's content blocks are parsed from their raw JSON into unified
// content parts: text and image blocks become ContentParts, tool_use blocks
// become assistant ToolCalls (Anthropic input is an object, mapped to
// Function.Arguments as map[string]any), and tool_result blocks become user
// ToolResults. Images embedded inside tool_result content are surfaced onto the
// message so they reach the model. Tools are sanitized via
// converter.SanitizeToolSchema, and the thinking config mirrors Anthropic's
// extended-thinking parameter.
func buildConverterRequest(req *MessagesRequest) converter.Request {
	norm := req.NormalizeRoles()

	messages := make([]converter.Message, 0, len(norm.Turns))
	for _, turn := range norm.Turns {
		messages = append(messages, convertTurn(turn))
	}

	return converter.Request{
		Model:      req.Model,
		System:     norm.System,
		Messages:   messages,
		Tools:      convertTools(req.Tools),
		ToolChoice: decodeToolChoice(req.ToolChoice),
		Thinking:   thinkingConfig(req.Thinking),
	}
}

// convertTurn maps a single normalized user/assistant message into the unified
// converter.Message shape.
func convertTurn(turn Message) converter.Message {
	msg := converter.Message{Role: turn.Role}

	// Bare-string content is a single text block.
	if turn.Content.IsString() {
		text := turn.Content.Text()
		if text != "" {
			msg.Content = []converter.ContentPart{{Type: "text", Text: text}}
		}
		return msg
	}

	var images []converter.Image
	for _, raw := range turn.Content.Blocks() {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		switch head.Type {
		case "text":
			var b struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &b) == nil && b.Text != "" {
				msg.Content = append(msg.Content, converter.ContentPart{Type: "text", Text: b.Text})
			}
		case "image":
			if img, ok := parseImageBlock(raw); ok {
				images = append(images, img)
			}
		case "tool_use":
			var b struct {
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			}
			if json.Unmarshal(raw, &b) == nil && b.ID != "" && b.Name != "" {
				if b.Input == nil {
					b.Input = map[string]any{}
				}
				msg.ToolCalls = append(msg.ToolCalls, converter.ToolCall{
					ID:   b.ID,
					Type: "function",
					Function: converter.FunctionCall{
						Name:      b.Name,
						Arguments: b.Input,
					},
				})
			}
		case "tool_result":
			tr, trImages, ok := parseToolResultBlock(raw)
			if ok {
				msg.ToolResults = append(msg.ToolResults, tr)
				images = append(images, trImages...)
			}
		}
	}

	if len(images) > 0 {
		msg.Images = images
	}
	return msg
}

// parseImageBlock extracts a base64 image source from an Anthropic image block.
// URL sources are skipped (Kiro requires base64-encoded image data).
func parseImageBlock(raw json.RawMessage) (converter.Image, bool) {
	var b struct {
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return converter.Image{}, false
	}
	if b.Source.Type == "url" || b.Source.Data == "" {
		slog.Debug("skipping non-base64 image source", "type", b.Source.Type)
		return converter.Image{}, false
	}
	return converter.Image{MediaType: b.Source.MediaType, Data: b.Source.Data}, true
}

// parseToolResultBlock extracts a unified ToolResult from an Anthropic
// tool_result block. The block's content may be a plain string or a list of
// sub-blocks (text/image); text is preserved as content parts and any images
// are returned separately so the caller can surface them on the message.
func parseToolResultBlock(raw json.RawMessage) (converter.ToolResult, []converter.Image, bool) {
	var b struct {
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &b); err != nil || b.ToolUseID == "" {
		return converter.ToolResult{}, nil, false
	}

	tr := converter.ToolResult{ToolUseID: b.ToolUseID, IsError: b.IsError}

	if len(b.Content) == 0 {
		return tr, nil, true
	}

	// content may be a string or an array of sub-blocks.
	var asString string
	if json.Unmarshal(b.Content, &asString) == nil {
		tr.Content = asString
		return tr, nil, true
	}

	var subBlocks []json.RawMessage
	if json.Unmarshal(b.Content, &subBlocks) != nil {
		// Unknown shape: stringify the raw JSON so nothing is lost.
		tr.Content = string(b.Content)
		return tr, nil, true
	}

	var parts []converter.ContentPart
	var images []converter.Image
	for _, sub := range subBlocks {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(sub, &head) != nil {
			continue
		}
		switch head.Type {
		case "text":
			var t struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(sub, &t) == nil && t.Text != "" {
				parts = append(parts, converter.ContentPart{Type: "text", Text: t.Text})
			}
		case "image":
			if img, ok := parseImageBlock(sub); ok {
				images = append(images, img)
			}
		}
	}
	if len(parts) > 0 {
		tr.Content = parts
	}
	return tr, images, true
}

// convertTools parses the raw Anthropic tools array into unified converter.Tools.
// Each tool's input_schema is sanitized via converter.SanitizeToolSchema. Tools
// without a usable name are skipped.
func convertTools(raw json.RawMessage) []converter.Tool {
	if len(raw) == 0 {
		return nil
	}
	var defs []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		slog.Debug("failed to parse anthropic tools, ignoring", "error", err)
		return nil
	}
	tools := make([]converter.Tool, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			continue
		}
		tools = append(tools, converter.Tool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: converter.SanitizeToolSchema(d.InputSchema),
			Type:        "function",
		})
	}
	if len(tools) == 0 {
		return nil
	}
	return tools
}

// decodeToolChoice decodes the raw tool_choice value into a generic structure.
// Kiro does not consume tool_choice, but it is forwarded on the unified request
// for completeness.
func decodeToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// thinkingConfig interprets the Anthropic thinking parameter:
// an absent or malformed thinking parameter defaults to enabled (subject to
// config.FakeReasoningEnabled), an explicit {"type":"disabled"} disables it, and
// {"type":"enabled","budget_tokens":N} enables it with the requested budget.
func thinkingConfig(raw json.RawMessage) converter.ThinkingConfig {
	if len(raw) == 0 {
		return converter.ThinkingConfig{Enabled: true}
	}
	var t struct {
		Type         string `json:"type"`
		BudgetTokens *int   `json:"budget_tokens"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return converter.ThinkingConfig{Enabled: true}
	}
	switch t.Type {
	case "disabled":
		return converter.ThinkingConfig{Enabled: false}
	case "enabled":
		return converter.ThinkingConfig{Enabled: true, BudgetTokens: t.BudgetTokens}
	default:
		return converter.ThinkingConfig{Enabled: true}
	}
}
