package anthropic

// This file defines the response and streaming-event types of the Anthropic
// Messages API. Only the shapes the proxy actually emits are modeled; unknown
// request fields are tolerated elsewhere (see types.go). A non-standard
// credits_used field is added to the usage object to surface Kiro credit
// consumption.

// Usage is the token-usage object returned in a Messages response and in the
// streaming message_start event. The cache fields are forwarded only when the
// upstream Kiro API reports them; credits_used carries Kiro credit consumption
// (a kiro-proxy extension, omitted when unknown).
type Usage struct {
	InputTokens              int      `json:"input_tokens"`
	OutputTokens             int      `json:"output_tokens"`
	CacheReadInputTokens     *int     `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int     `json:"cache_creation_input_tokens,omitempty"`
	CreditsUsed              *float64 `json:"credits_used,omitempty"`
}

// DeltaUsage is the usage object carried by the streaming message_delta event.
// Unlike Usage it omits input_tokens (only output tokens are known at that
// point), but it still forwards cache and credit information when available.
type DeltaUsage struct {
	OutputTokens             int      `json:"output_tokens"`
	CacheReadInputTokens     *int     `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int     `json:"cache_creation_input_tokens,omitempty"`
	CreditsUsed              *float64 `json:"credits_used,omitempty"`
}

// --- Output content blocks ---

// ThinkingBlock is a thinking/reasoning output block. Signature is the opaque
// upstream signature when Kiro provides one; legacy emulated reasoning uses a
// generated compatibility signature.
type ThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

// RedactedThinkingBlock preserves opaque provider-redacted reasoning.
type RedactedThinkingBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// TextBlock is a text output block.
type TextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolUseBlock is a tool_use output block (a tool call made by the assistant).
type ToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// MessagesResponse is the non-streaming response from POST /v1/messages.
// Content holds the ordered output blocks (thinking, text, tool_use).
type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []any          `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	StopDetails  map[string]any `json:"stop_details,omitempty"`
	Usage        Usage          `json:"usage"`
}

// --- Streaming events ---

// messageStartEvent is the first event of a message stream.
type messageStartEvent struct {
	Type    string           `json:"type"`
	Message messageStartBody `json:"message"`
}

// messageStartBody is the initial message object carried by message_start (with
// empty content and best-effort input token estimate).
type messageStartBody struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Content      []any   `json:"content"`
	Model        string  `json:"model"`
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        Usage   `json:"usage"`
}

// contentBlockStartEvent opens a content block (text, thinking, or tool_use).
type contentBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

// contentBlockDeltaEvent carries an incremental update to a content block.
type contentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

// contentBlockStopEvent closes a content block.
type contentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

// textDelta is a text_delta payload.
type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// thinkingDelta is a thinking_delta payload.
type thinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

// signatureDelta carries the opaque thinking signature immediately before the
// thinking content block closes.
type signatureDelta struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
}

// inputJSONDelta is an input_json_delta payload for streaming tool input.
type inputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

// messageDeltaEvent carries the terminal stop_reason and output usage.
type messageDeltaEvent struct {
	Type  string           `json:"type"`
	Delta messageDeltaBody `json:"delta"`
	Usage DeltaUsage       `json:"usage"`
}

// messageDeltaBody holds Kiro's terminal verdict in Anthropic form.
type messageDeltaBody struct {
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	StopDetails  map[string]any `json:"stop_details,omitempty"`
}

// messageStopEvent ends a message stream.
type messageStopEvent struct {
	Type string `json:"type"`
}

// pingEvent is a keep-alive event.
type pingEvent struct {
	Type string `json:"type"`
}

// errorEvent is sent when an error occurs mid-stream.
type errorEvent struct {
	Type  string    `json:"type"`
	Error errorBody `json:"error"`
}

// errorBody is the error detail shared by error events and error responses.
type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// errorResponse is the non-streaming Anthropic error envelope.
type errorResponse struct {
	Type  string    `json:"type"`
	Error errorBody `json:"error"`
}
