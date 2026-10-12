// Package converter is the API-agnostic core that turns a unified chat request
// into the Kiro / CodeWhisperer conversationState payload.
//
// API-specific adapters (internal/api/anthropic, internal/api/openai) translate
// their wire formats into the unified types defined here ([Request], [Message],
// [ContentPart], [Tool], ...) and then call [BuildKiroPayload] to obtain the
// serialized Kiro request body. The unified representation uses an internal dict
// shape: a message carries text/image content blocks plus separate tool-call,
// tool-result and image lists, and the core layer is responsible for folding the
// system prompt, sanitizing tool schemas, normalizing roles, and assembling the
// conversation.
package converter

// ThinkingConfig is the unified "fake reasoning" configuration created by the API
// adapter and consumed by the core layer for thinking-tag injection.
//
// Note the Go zero value has Enabled=false: adapters that want thinking-tag
// injection (subject to config.FakeReasoningEnabled) MUST set Enabled=true.
type ThinkingConfig struct {
	// Enabled reports whether reasoning was requested.
	Enabled bool
	// BudgetTokens is used only by the legacy emulated-thinking fallback.
	BudgetTokens *int
	// Effort is the normalized client effort (low/medium/high/xhigh/max).
	Effort string
}

// Request is the unified, API-agnostic chat request consumed by
// [BuildKiroPayload].
type Request struct {
	// Model is the externally requested model name (before Kiro resolution).
	Model string
	// System is the already-extracted system prompt (may be empty).
	System string
	// Messages is the ordered conversation, excluding system messages.
	Messages []Message
	// Tools is the list of tools available to the model (may be empty).
	Tools []Tool
	// ToolChoice controls which tools are exposed for this turn. Kiro has no
	// native tool-choice field, so BuildKiroPayload implements it by filtering.
	ToolChoice ToolChoice
	// Generation controls are serialized only when Kiro's live schema advertises
	// the corresponding field.
	Generation GenerationConfig
	// Thinking configures native reasoning when Kiro advertises a schema, or
	// legacy emulated reasoning otherwise.
	Thinking ThinkingConfig
	// ModelRequestSchema is Kiro's live additionalModelRequestFieldsSchema.
	ModelRequestSchema []byte
}

// Message is a single unified conversation message.
//
// content is held as a list of [ContentPart] blocks; tool calls, tool results
// and images are carried in dedicated fields.
type Message struct {
	// Role is "user", "assistant", "system", or any client-supplied role. Unknown
	// roles are normalized to "user" by [NormalizeMessageRoles].
	Role string
	// Content is the ordered list of content blocks.
	Content []ContentPart
	// ToolCalls holds assistant tool invocations (OpenAI-style tool_calls).
	ToolCalls []ToolCall
	// ToolResults holds user-supplied results for prior tool calls.
	ToolResults []ToolResult
	// Images holds images already extracted into unified form. When empty, images
	// embedded in Content (Type == "image") are used instead.
	Images []Image
}

// ContentPart is one block of unified message content. Only the fields relevant
// to Type are meaningful.
type ContentPart struct {
	// Type is "text", "image", "tool_use", or "tool_result". An empty Type with a
	// non-empty Text is treated as text.
	Type string

	// Text is the block text, when Type == "text".
	Text string

	// MediaType and Data describe an image block (Type == "image"). Data may be a
	// bare base64 string or a full "data:<media-type>;base64,<...>" data URL;
	// [BuildKiroPayload] strips the prefix when present.
	MediaType string
	Data      string

	// ID, Name and Input describe an assistant tool_use block
	// (Type == "tool_use").
	ID    string
	Name  string
	Input map[string]any

	// ToolUseID and Content describe a user tool_result block
	// (Type == "tool_result"). Content is a string or a []ContentPart.
	ToolUseID string
	Content   any
}

// Image is an image in unified form: a media type plus base64 (or data-URL) data.
type Image struct {
	MediaType string
	Data      string
}

// ToolCall is an assistant tool invocation in unified (OpenAI-shaped) form.
type ToolCall struct {
	// ID is the tool-call id (becomes toolUseId in Kiro form).
	ID string
	// Type is the tool-call type, conventionally "function".
	Type string
	// Function carries the invoked function's name and arguments.
	Function FunctionCall
}

// FunctionCall is the function portion of a [ToolCall].
type FunctionCall struct {
	// Name is the tool/function name.
	Name string
	// Arguments is the call arguments as either a JSON string (OpenAI) or a
	// map[string]any (Anthropic unified). Both forms are accepted.
	Arguments any
}

// GenerationConfig carries client generation controls to the schema-gated Kiro
// request builder. Nil fields mean the client did not specify a value.
type GenerationConfig struct {
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	TopK        *int
}

// ToolResult is a user-supplied result for a prior tool call, in unified form.
type ToolResult struct {
	// ToolUseID references the originating tool call.
	ToolUseID string
	// Content is the result content as a string or a []ContentPart.
	Content any
	// IsError marks a failed tool result; Kiro receives status "error".
	IsError bool
}

// ToolChoice is the API-agnostic tool selection policy.
type ToolChoice struct {
	Type               string
	Name               string
	DisableParallelUse bool
}

// Tool is a unified tool definition.
type Tool struct {
	// Name is the tool name. Kiro rejects names longer than 64 characters, so
	// [BuildKiroPayload] transparently aliases over-long names via ShortenToolName
	// and restores the originals on the response path.
	Name string
	// Description is the human-readable tool description. Long descriptions are
	// moved to the system prompt (see config.ToolDescriptionMaxLength).
	Description string
	// InputSchema is the JSON Schema for the tool's parameters. It is sanitized by
	// [SanitizeToolSchema] before being sent to Kiro.
	InputSchema map[string]any
	// Type is the API-specific tool type (e.g. "function"); unused by Kiro.
	Type string
}
