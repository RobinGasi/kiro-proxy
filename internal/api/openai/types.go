// Package openai implements the OpenAI-compatible API surface
// (/v1/chat/completions, /v1/models, /v1/usage) and its request/response types.
//
// Lenience is deliberate: message role is a plain string (not an enum), message
// content tolerates either a bare string or an array of content blocks, and
// unknown top-level fields are ignored.
package openai

import (
	"bytes"
	"encoding/json"
)

// ChatCompletionRequest is a lenient model of POST /v1/chat/completions.
//
// It accepts all standard OpenAI fields and silently ignores unknown ones for
// forward compatibility (encoding/json drops fields with no struct member).
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`

	// Generation parameters (accepted; most are passed through or ignored).
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	N                *int            `json:"n,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	MaxCompletion    *int            `json:"max_completion_tokens,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`

	// ReasoningEffort drives thinking-budget extraction:
	// "none" disables thinking; the other levels enable it.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// Tools (function calling).
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// ChatMessage is a chat message in OpenAI format. Role is a plain string and
// content tolerates string, array, or null shapes.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    ContentField    `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCallBlock `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ToolCallBlock is an assistant tool call in OpenAI format.
type ToolCallBlock struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries a tool call's name and JSON-string arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool in OpenAI format. It supports the standard
// {"type":"function","function":{...}} shape and the flat Cursor-style shape.
type Tool struct {
	Type     string        `json:"type"`
	Function *ToolFunction `json:"function,omitempty"`

	// Flat-format fields (Cursor-style).
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ToolFunction is the function description in the standard OpenAI tool shape.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ContentField is either a string or a list of content items. A plain string is
// preserved; an array is decoded into typed items.
type ContentField struct {
	set      bool
	isString bool
	str      string
	items    []ContentItem
}

// ContentItem is one block of OpenAI message content. Only the fields relevant
// to Type are meaningful.
type ContentItem struct {
	Type     string    `json:"type"`
	Text     string    `json:"text"`
	ImageURL *imageURL `json:"image_url,omitempty"`
	// tool_result fields (Anthropic-style blocks accepted inside user content).
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// UnmarshalJSON accepts a string, an array of items, or null.
func (c *ContentField) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	c.set = true
	if data[0] == '"' {
		c.isString = true
		return json.Unmarshal(data, &c.str)
	}
	if data[0] == '[' {
		return json.Unmarshal(data, &c.items)
	}
	// Tolerate a bare object by capturing nothing meaningful (extra="allow").
	return nil
}

// MarshalJSON re-emits the original shape.
func (c ContentField) MarshalJSON() ([]byte, error) {
	if !c.set {
		return []byte("null"), nil
	}
	if c.isString {
		return json.Marshal(c.str)
	}
	return json.Marshal(c.items)
}

// IsString reports whether the content was a bare string.
func (c ContentField) IsString() bool { return c.isString }

// Items returns the decoded content items (empty for string content).
func (c ContentField) Items() []ContentItem { return c.items }

// Text returns the concatenated text: the bare string, or the text of every
// text item in an array.
func (c ContentField) Text() string {
	if !c.set {
		return ""
	}
	if c.isString {
		return c.str
	}
	var b bytes.Buffer
	for _, it := range c.items {
		if it.Type == "text" && it.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(it.Text)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

// Usage is OpenAI token-usage information plus the Kiro-specific credits_used.
type Usage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CreditsUsed      *float64 `json:"credits_used,omitempty"`
}

// ChatCompletionResponse is a full non-streaming response.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   Usage                  `json:"usage"`
}

// ChatCompletionChoice is a single non-streaming choice.
type ChatCompletionChoice struct {
	Index        int            `json:"index"`
	Message      map[string]any `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// ChatCompletionChunk is a single streaming chunk.
type ChatCompletionChunk struct {
	ID      string                      `json:"id"`
	Object  string                      `json:"object"`
	Created int64                       `json:"created"`
	Model   string                      `json:"model"`
	Choices []ChatCompletionChunkChoice `json:"choices"`
	Usage   *Usage                      `json:"usage,omitempty"`
}

// ChatCompletionChunkChoice is a single choice within a streaming chunk.
type ChatCompletionChunkChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// Model describes one AI model in the /v1/models listing.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the response of GET /v1/models.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
