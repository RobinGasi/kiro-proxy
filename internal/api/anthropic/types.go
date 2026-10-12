// Package anthropic implements the Anthropic Messages API surface
// (/v1/messages, /v1/messages/count_tokens) and its request/response types.
//
// Lenience is deliberate: message role is a plain string (NOT an enum), unknown
// fields are tolerated, and inline role:"system" messages are accepted — newer
// Claude Code clients (2.1.168+, incl. claude-opus-4-8) place a system message
// inside messages[], which strict validation would otherwise reject with HTTP
// 422 (issues #226/#217/#211/#219/#190).
package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MessagesRequest is a lenient model of POST /v1/messages.
type MessagesRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	System        SystemField     `json:"system,omitzero"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      json.RawMessage `json:"thinking,omitempty"`
	OutputConfig  json.RawMessage `json:"output_config,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	// Unknown top-level fields are ignored by encoding/json (forward compat).
}

// Message has a string role (the key fix) and string-or-blocks content.
type Message struct {
	Role    string       `json:"role"`
	Content ContentField `json:"content"`
}

// SystemField is either a string or a list of system content blocks.
type SystemField struct {
	set    bool
	str    string
	blocks []json.RawMessage
}

// UnmarshalJSON accepts a string or an array of content blocks.
func (s *SystemField) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	s.set = true
	if data[0] == '"' {
		return json.Unmarshal(data, &s.str)
	}
	if data[0] == '[' {
		return json.Unmarshal(data, &s.blocks)
	}
	return fmt.Errorf("system: expected string or array, got %s", truncate(data))
}

// MarshalJSON re-emits the original shape.
func (s SystemField) MarshalJSON() ([]byte, error) {
	if !s.set {
		return []byte("null"), nil
	}
	if s.blocks != nil {
		return json.Marshal(s.blocks)
	}
	return json.Marshal(s.str)
}

// Text returns the concatenated system text (string or the text of each block).
func (s SystemField) Text() string {
	if !s.set {
		return ""
	}
	if s.blocks == nil {
		return s.str
	}
	return joinBlockText(s.blocks)
}

// ContentField is either a string or a list of content blocks. It is stored as
// raw blocks; a plain string becomes a single synthetic text block.
type ContentField struct {
	set      bool
	isString bool
	str      string
	blocks   []json.RawMessage
}

// UnmarshalJSON accepts a string or an array of blocks.
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
		return json.Unmarshal(data, &c.blocks)
	}
	return fmt.Errorf("content: expected string or array, got %s", truncate(data))
}

// MarshalJSON re-emits the original shape.
func (c ContentField) MarshalJSON() ([]byte, error) {
	if !c.set {
		return []byte("null"), nil
	}
	if c.isString {
		return json.Marshal(c.str)
	}
	return json.Marshal(c.blocks)
}

// Text returns the concatenated text of string content or text blocks.
func (c ContentField) Text() string {
	if !c.set {
		return ""
	}
	if c.isString {
		return c.str
	}
	return joinBlockText(c.blocks)
}

// Blocks returns the raw content blocks (empty for string content; callers that
// need a block view can wrap a string as a text block).
func (c ContentField) Blocks() []json.RawMessage { return c.blocks }

// IsString reports whether the content was a bare string.
func (c ContentField) IsString() bool { return c.isString }

// blockHeader peeks at a block's "type" and "text".
type blockHeader struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func joinBlockText(blocks []json.RawMessage) string {
	var b bytes.Buffer
	for _, raw := range blocks {
		var h blockHeader
		if json.Unmarshal(raw, &h) == nil && h.Type == "text" && h.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(h.Text)
		}
	}
	return b.String()
}

func truncate(b []byte) string {
	const n = 40
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
