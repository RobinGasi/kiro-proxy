// Package awsstream parses Kiro's AWS event-stream HTTP response chunks.
//
// Despite the upstream name, this parser does NOT decode the binary AWS
// event-stream frame envelope. Instead it decodes each incoming chunk as UTF-8
// (dropping undecodable bytes) and scans the accumulated text for embedded JSON
// events, reconstructing streaming tool calls as it goes.
//
// Supported event types extracted from the stream:
//
//   - content       text content of the response
//   - usage         credit-consumption information
//   - context_usage context-usage percentage
//
// Tool-call related JSON fragments (tool_start / tool_input / tool_stop) do not
// produce Events; they are accumulated internally and retrieved via
// (*Parser).GetToolCalls.
package awsstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/RobinGasi/kiro-proxy/internal/util"
)

// Event type identifiers returned by (*Parser).Feed.
const (
	EventContent      = "content"
	EventUsage        = "usage"
	EventContextUsage = "context_usage"
)

// Event is a single parsed stream event. Data depends on Type:
//
//   - EventContent:      string content fragment
//   - EventUsage:        the raw "usage" value (number/object), 0 if absent
//   - EventContextUsage: the raw "contextUsagePercentage" value, 0 if absent
type Event struct {
	Type string
	Data any
}

// ToolCallFunction is the function payload of a reconstructed tool call.
type ToolCallFunction struct {
	Name      string
	Arguments string // JSON-encoded arguments object
}

// ToolCall is a reconstructed tool call in OpenAI format.
type ToolCall struct {
	ID       string
	Type     string // always "function"
	Function ToolCallFunction

	// TruncationDetected is set when the tool call's arguments failed to parse
	// as JSON and the failure looked like upstream truncation by the Kiro API.
	TruncationDetected bool
	// TruncationInfo carries diagnostics when TruncationDetected is true.
	TruncationInfo *TruncationInfo
}

// TruncationInfo describes why a malformed arguments string was deemed truncated.
type TruncationInfo struct {
	IsTruncated bool
	Reason      string
	SizeBytes   int
}

// eventPatterns are the JSON prefixes scanned for, in priority order. On a tie
// for earliest position the first pattern in this slice wins.
var eventPatterns = []struct{ pattern, typ string }{
	{`{"content":`, "content"},
	{`{"name":`, "tool_start"},
	{`{"input":`, "tool_input"},
	{`{"stop":`, "tool_stop"},
	{`{"followupPrompt":`, "followup"},
	{`{"usage":`, "usage"},
	{`{"contextUsagePercentage":`, "context_usage"},
}

var bracketRe = regexp.MustCompile(`(?i)\[Called\s+(\w+)\s+with\s+args:\s*`)

// Parser is a stateful AWS event-stream parser. It is not safe for concurrent
// use. Construct one with NewParser.
type Parser struct {
	buffer          string
	lastContent     *string // last emitted content, for deduplication
	currentToolCall *ToolCall
	toolCalls       []*ToolCall

	// TruncationRecovery only influences the wording of truncation log
	// messages; it mirrors the TRUNCATION_RECOVERY config flag. Defaults false.
	TruncationRecovery bool
}

// NewParser returns a ready-to-use Parser.
func NewParser() *Parser {
	return &Parser{}
}

// Feed appends a chunk to the internal buffer and returns any newly completed
// events. Incomplete JSON is retained for subsequent calls.
func (p *Parser) Feed(chunk []byte) []Event {
	p.buffer += decodeIgnore(chunk)

	var events []Event
	for {
		// Find the earliest occurrence of any known pattern.
		earliestPos := -1
		earliestType := ""
		for _, ep := range eventPatterns {
			pos := strings.Index(p.buffer, ep.pattern)
			if pos != -1 && (earliestPos == -1 || pos < earliestPos) {
				earliestPos = pos
				earliestType = ep.typ
			}
		}
		if earliestPos == -1 {
			break
		}

		// Locate the end of this JSON object.
		jsonEnd := FindMatchingBrace(p.buffer, earliestPos)
		if jsonEnd == -1 {
			// JSON not yet complete; wait for more data.
			break
		}

		jsonStr := p.buffer[earliestPos : jsonEnd+1]
		p.buffer = p.buffer[jsonEnd+1:]

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			slog.Warn("awsstream: failed to parse JSON event", "raw", util.Truncate(jsonStr, 100))
			continue
		}

		if ev, ok := p.processEvent(data, earliestType); ok {
			events = append(events, ev)
		}
	}
	return events
}

// processEvent dispatches a decoded JSON object by its detected type.
func (p *Parser) processEvent(data map[string]any, eventType string) (Event, bool) {
	switch eventType {
	case "content":
		return p.processContentEvent(data)
	case "tool_start":
		p.processToolStartEvent(data)
	case "tool_input":
		p.processToolInputEvent(data)
	case "tool_stop":
		p.processToolStopEvent(data)
	case "usage":
		v, ok := data["usage"]
		if !ok {
			v = 0
		}
		return Event{Type: EventUsage, Data: v}, true
	case "context_usage":
		v, ok := data["contextUsagePercentage"]
		if !ok {
			v = 0
		}
		return Event{Type: EventContextUsage, Data: v}, true
	}
	return Event{}, false
}

// processContentEvent emits content, skipping followup prompts and duplicates.
func (p *Parser) processContentEvent(data map[string]any) (Event, bool) {
	content, _ := data["content"].(string)

	// Skip followupPrompt entries.
	if isTruthy(data["followupPrompt"]) {
		return Event{}, false
	}

	// Deduplicate identical consecutive content.
	if p.lastContent != nil && *p.lastContent == content {
		return Event{}, false
	}
	c := content
	p.lastContent = &c

	return Event{Type: EventContent, Data: content}, true
}

// processToolStartEvent begins a new tool call, finalizing any in progress.
func (p *Parser) processToolStartEvent(data map[string]any) {
	if p.currentToolCall != nil {
		p.finalizeToolCall()
	}

	// toolUseId default applies only when the key is absent.
	var id string
	if v, ok := data["toolUseId"]; ok {
		id, _ = v.(string)
	} else {
		id = util.GenerateToolCallID()
	}
	name, _ := data["name"].(string)

	p.currentToolCall = &ToolCall{
		ID:   id,
		Type: "function",
		Function: ToolCallFunction{
			Name:      name,
			Arguments: inputToString(data["input"]),
		},
	}

	if isTruthy(data["stop"]) {
		p.finalizeToolCall()
	}
}

// processToolInputEvent appends streamed input to the current tool call.
func (p *Parser) processToolInputEvent(data map[string]any) {
	if p.currentToolCall != nil {
		p.currentToolCall.Function.Arguments += inputToString(data["input"])
	}
}

// processToolStopEvent finalizes the current tool call when stop is set.
func (p *Parser) processToolStopEvent(data map[string]any) {
	if p.currentToolCall != nil && isTruthy(data["stop"]) {
		p.finalizeToolCall()
	}
}

// finalizeToolCall normalizes the current tool call's arguments and stores it.
func (p *Parser) finalizeToolCall() {
	tc := p.currentToolCall
	if tc == nil {
		return
	}

	args := tc.Function.Arguments
	toolName := tc.Function.Name
	if toolName == "" {
		toolName = "unknown"
	}
	slog.Debug("awsstream: finalizing tool call", "tool", toolName, "raw_arguments", util.Truncate(args, 200))

	if strings.TrimSpace(args) != "" {
		if normalized, ok := reencodeJSON(args); ok {
			tc.Function.Arguments = normalized
		} else {
			info := diagnoseJSONTruncation(args)
			if info.IsTruncated {
				// Mark for the recovery system.
				tc.TruncationDetected = true
				tc.TruncationInfo = &info

				id := tc.ID
				if id == "" {
					id = "unknown"
				}
				note := "Set TRUNCATION_RECOVERY=true in .env to auto-notify model about truncation."
				if p.TruncationRecovery {
					note = "Model will be notified automatically about truncation."
				}
				slog.Error("awsstream: tool call truncated by Kiro API",
					"tool", toolName, "id", id, "size_bytes", info.SizeBytes,
					"reason", info.Reason, "note", note)
			} else {
				slog.Warn("awsstream: failed to parse tool arguments",
					"tool", toolName, "reason", info.Reason, "raw", util.Truncate(args, 200))
			}
			tc.Function.Arguments = "{}"
		}
	} else {
		// Empty string - normal for duplicate tool calls from Kiro.
		slog.Debug("awsstream: tool has empty arguments (will be deduplicated)", "tool", toolName)
		tc.Function.Arguments = "{}"
	}

	p.toolCalls = append(p.toolCalls, tc)
	p.currentToolCall = nil
}

// GetToolCalls finalizes any in-progress tool call and returns the deduplicated
// list of reconstructed tool calls.
func (p *Parser) GetToolCalls() []ToolCall {
	if p.currentToolCall != nil {
		p.finalizeToolCall()
	}
	all := make([]ToolCall, 0, len(p.toolCalls))
	for _, tc := range p.toolCalls {
		if tc != nil {
			all = append(all, *tc)
		}
	}
	return DeduplicateToolCalls(all)
}

// Reset clears all parser state so the Parser can be reused.
func (p *Parser) Reset() {
	p.buffer = ""
	p.lastContent = nil
	p.currentToolCall = nil
	p.toolCalls = nil
}

// FindMatchingBrace returns the index of the closing brace matching the opening
// brace at startPos, accounting for nesting, quoted strings and escapes. It
// returns -1 if startPos is not '{' or no match is found.
func FindMatchingBrace(text string, startPos int) int {
	if startPos < 0 || startPos >= len(text) || text[startPos] != '{' {
		return -1
	}

	braceCount := 0
	inString := false
	escapeNext := false

	for i := startPos; i < len(text); i++ {
		ch := text[i]

		if escapeNext {
			escapeNext = false
			continue
		}
		if ch == '\\' && inString {
			escapeNext = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if !inString {
			switch ch {
			case '{':
				braceCount++
			case '}':
				braceCount--
				if braceCount == 0 {
					return i
				}
			}
		}
	}
	return -1
}

// ParseBracketToolCalls extracts tool calls written in the textual
// "[Called func_name with args: {...}]" format that some models emit instead of
// structured JSON. The returned calls are in OpenAI format.
func ParseBracketToolCalls(responseText string) []ToolCall {
	if responseText == "" || !strings.Contains(responseText, "[Called") {
		return nil
	}

	var toolCalls []ToolCall
	for _, m := range bracketRe.FindAllStringSubmatchIndex(responseText, -1) {
		funcName := responseText[m[2]:m[3]]
		argsStart := m[1]

		// Find JSON start.
		rel := strings.IndexByte(responseText[argsStart:], '{')
		if rel == -1 {
			continue
		}
		jsonStart := argsStart + rel

		// Find JSON end accounting for nesting.
		jsonEnd := FindMatchingBrace(responseText, jsonStart)
		if jsonEnd == -1 {
			continue
		}

		jsonStr := responseText[jsonStart : jsonEnd+1]
		normalized, ok := reencodeJSON(jsonStr)
		if !ok {
			slog.Warn("awsstream: failed to parse bracket tool call arguments", "raw", util.Truncate(jsonStr, 100))
			continue
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:   util.GenerateToolCallID(),
			Type: "function",
			Function: ToolCallFunction{
				Name:      funcName,
				Arguments: normalized,
			},
		})
	}
	return toolCalls
}

// DeduplicateToolCalls removes duplicate tool calls. It first deduplicates by id
// (keeping the entry with the richer, non-empty arguments) and then removes
// exact name+arguments duplicates. Original ordering is preserved by first
// appearance.
func DeduplicateToolCalls(toolCalls []ToolCall) []ToolCall {
	// Deduplicate by id, keeping the call with the better arguments.
	byID := make(map[string]ToolCall)
	var order []string
	for _, tc := range toolCalls {
		if tc.ID == "" {
			// Without id - handled later by name+args dedup.
			continue
		}
		existing, ok := byID[tc.ID]
		if !ok {
			byID[tc.ID] = tc
			order = append(order, tc.ID)
			continue
		}
		existingArgs := argsOrEmpty(existing)
		currentArgs := argsOrEmpty(tc)
		if currentArgs != "{}" && (existingArgs == "{}" || len(currentArgs) > len(existingArgs)) {
			byID[tc.ID] = tc
		}
	}

	withID := make([]ToolCall, 0, len(order))
	for _, id := range order {
		withID = append(withID, byID[id])
	}
	var withoutID []ToolCall
	for _, tc := range toolCalls {
		if tc.ID == "" {
			withoutID = append(withoutID, tc)
		}
	}

	// Deduplicate by name+arguments across all.
	combined := make([]ToolCall, 0, len(withID)+len(withoutID))
	combined = append(combined, withID...)
	combined = append(combined, withoutID...)

	seen := make(map[string]struct{})
	unique := make([]ToolCall, 0, len(combined))
	for _, tc := range combined {
		key := tc.Function.Name + "-" + argsOrEmpty(tc)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, tc)
	}
	return unique
}

// diagnoseJSONTruncation analyzes a malformed JSON string to determine whether
// it appears to have been truncated upstream.
func diagnoseJSONTruncation(jsonStr string) TruncationInfo {
	sizeBytes := len(jsonStr)
	stripped := strings.TrimSpace(jsonStr)

	if stripped == "" {
		return TruncationInfo{IsTruncated: false, Reason: "empty string", SizeBytes: sizeBytes}
	}

	openBraces := strings.Count(stripped, "{")
	closeBraces := strings.Count(stripped, "}")
	openBrackets := strings.Count(stripped, "[")
	closeBrackets := strings.Count(stripped, "]")

	if strings.HasPrefix(stripped, "{") && !strings.HasSuffix(stripped, "}") {
		return TruncationInfo{
			IsTruncated: true,
			Reason:      fmt.Sprintf("missing %d closing brace(s)", openBraces-closeBraces),
			SizeBytes:   sizeBytes,
		}
	}
	if strings.HasPrefix(stripped, "[") && !strings.HasSuffix(stripped, "]") {
		return TruncationInfo{
			IsTruncated: true,
			Reason:      fmt.Sprintf("missing %d closing bracket(s)", openBrackets-closeBrackets),
			SizeBytes:   sizeBytes,
		}
	}
	if openBraces != closeBraces {
		return TruncationInfo{
			IsTruncated: true,
			Reason:      fmt.Sprintf("unbalanced braces (%d open, %d close)", openBraces, closeBraces),
			SizeBytes:   sizeBytes,
		}
	}
	if openBrackets != closeBrackets {
		return TruncationInfo{
			IsTruncated: true,
			Reason:      fmt.Sprintf("unbalanced brackets (%d open, %d close)", openBrackets, closeBrackets),
			SizeBytes:   sizeBytes,
		}
	}

	// Heuristic: count unescaped quotes to detect an unclosed string literal.
	quoteCount := 0
	for i := 0; i < len(stripped); {
		if stripped[i] == '\\' && i+1 < len(stripped) {
			i += 2 // Skip escaped character.
			continue
		}
		if stripped[i] == '"' {
			quoteCount++
		}
		i++
	}
	if quoteCount%2 != 0 {
		return TruncationInfo{IsTruncated: true, Reason: "unclosed string literal", SizeBytes: sizeBytes}
	}

	return TruncationInfo{IsTruncated: false, Reason: "malformed JSON", SizeBytes: sizeBytes}
}

// argsOrEmpty returns the call's arguments, substituting "{}" for an empty value.
func argsOrEmpty(tc ToolCall) string {
	if tc.Function.Arguments == "" {
		return "{}"
	}
	return tc.Function.Arguments
}

// inputToString coerces a tool "input" value to a string:
// dicts are JSON-serialized (empty dict -> ""), strings are returned as-is, and
// other falsy values become "".
func inputToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		if len(t) == 0 {
			return ""
		}
		b, _ := json.Marshal(t)
		return string(b)
	default:
		if !isTruthy(t) {
			return ""
		}
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// reencodeJSON validates s as JSON and returns a compact re-encoding (HTML
// escaping disabled). It reports false if s is not valid JSON.
func reencodeJSON(s string) (string, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

// isTruthy reports whether a decoded JSON value is truthy (non-empty, non-zero,
// non-null).
func isTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	default:
		return true
	}
}

// decodeIgnore decodes bytes as UTF-8, dropping undecodable bytes.
func decodeIgnore(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			b = b[1:]
			continue
		}
		sb.Write(b[:size])
		b = b[size:]
	}
	return sb.String()
}
