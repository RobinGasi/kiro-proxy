// Package awsstream parses Kiro's AWS EventStream responses.
//
// FeedStream decodes and CRC-validates the binary frame envelope, routes frames
// by :message-type / :event-type, and preserves metadata, reasoning, errors and
// unknown union members. Feed retains the legacy embedded-JSON scanner for
// plain-JSON fixtures and compatible legacy upstreams.
//
// Tool-call frames are accumulated internally and retrieved through
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

// Event type identifiers returned by (*Parser).FeedStream.
const (
	EventContent      = "content"
	EventReasoning    = "reasoning"
	EventUsage        = "usage"
	EventContextUsage = "context_usage"
	EventMetadata     = "metadata"
	EventError        = "error"
	EventIgnored      = "ignored"
)

// Event is one parsed stream event. Data depends on Type; UpstreamType retains
// the Smithy union member carried by the :event-type frame header.
type Event struct {
	Type         string
	UpstreamType string
	Data         any
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
	buffer      string
	frameBuffer []byte
	streamMode  streamMode
	lastContent *string // last emitted content, for deduplication

	// currentToolCall is retained for the legacy plain-JSON scanner.
	currentToolCall *ToolCall
	toolCalls       []*ToolCall
	// streamTools assembles binary toolUseEvent fragments by identity. Multiple
	// calls may be interleaved; streamToolOrder preserves first-seen order.
	streamTools     map[string]*streamToolState
	streamToolOrder []string

	// TruncationRecovery only influences the wording of truncation log
	// messages; it mirrors the TRUNCATION_RECOVERY config flag. Defaults false.
	TruncationRecovery bool
}

type streamToolState struct {
	call        ToolCall
	arguments   string
	complete    bool
	generatedID bool
}

// NewParser returns a ready-to-use Parser.
func NewParser() *Parser {
	return &Parser{}
}

// BufferedText returns unmatched data retained by the legacy plain-JSON parser.
// Binary framing errors are returned directly by FeedStream and FlushStream.
func (p *Parser) BufferedText() string {
	if p == nil {
		return ""
	}
	return p.buffer
}

// Feed runs the legacy embedded-JSON scanner. Production streaming uses
// FeedStream, which decodes the actual AWS EventStream envelope.
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

// processToolFrame assembles one Kiro toolUseEvent. Kiro repeats name and
// toolUseId on input-delta frames, so field presence cannot distinguish start
// from continuation. Calls are keyed by ID because Kiro may interleave them.
func (p *Parser) processToolFrame(data map[string]any) error {
	if len(data) == 0 {
		return fmt.Errorf("awsstream: empty toolUseEvent")
	}
	idValue, idPresent := data["toolUseId"]
	toolID, idOK := idValue.(string)
	if idPresent && (!idOK || strings.TrimSpace(toolID) == "") {
		return fmt.Errorf("awsstream: invalid toolUseId in toolUseEvent")
	}
	nameValue, namePresent := data["name"]
	name, nameOK := nameValue.(string)
	if namePresent && (!nameOK || strings.TrimSpace(name) == "") {
		return fmt.Errorf("awsstream: invalid tool name in toolUseEvent")
	}

	state, err := p.findStreamTool(toolID, name)
	if err != nil {
		return err
	}
	if state == nil {
		if name == "" {
			return fmt.Errorf("awsstream: new toolUseEvent is missing a tool name")
		}
		generated := toolID == ""
		if generated {
			toolID = util.GenerateToolCallID()
		}
		if p.streamTools == nil {
			p.streamTools = make(map[string]*streamToolState)
		}
		state = &streamToolState{
			call:        ToolCall{ID: toolID, Type: "function", Function: ToolCallFunction{Name: name}},
			generatedID: generated,
		}
		p.streamTools[toolID] = state
		p.streamToolOrder = append(p.streamToolOrder, toolID)
	} else if name != "" && state.call.Function.Name != name {
		return fmt.Errorf("awsstream: tool name changed for toolUseId %q", state.call.ID)
	}
	if state.complete {
		if isTruthy(data["stop"]) {
			return nil // idempotent duplicate terminal frame
		}
		return fmt.Errorf("awsstream: toolUseEvent for completed toolUseId %q", state.call.ID)
	}

	if input, ok := data["input"]; ok {
		if err := appendStreamToolInput(state, input); err != nil {
			return fmt.Errorf("awsstream: toolUseId %q: %w", state.call.ID, err)
		}
	}
	if isTruthy(data["stop"]) {
		normalized := "{}"
		if strings.TrimSpace(state.arguments) != "" {
			var ok bool
			normalized, ok = reencodeJSON(state.arguments)
			if !ok {
				return fmt.Errorf("awsstream: invalid completed tool arguments: %s", diagnoseJSONTruncation(state.arguments).Reason)
			}
		}
		state.call.Function.Arguments = normalized
		state.complete = true
	}
	return nil
}

func (p *Parser) findStreamTool(toolID, name string) (*streamToolState, error) {
	if toolID != "" {
		if state := p.streamTools[toolID]; state != nil {
			return state, nil
		}
		var generatedKey string
		for id, state := range p.streamTools {
			if state.generatedID && !state.complete && (name == "" || state.call.Function.Name == name) {
				if generatedKey != "" {
					return nil, fmt.Errorf("awsstream: ambiguous toolUseId rekey")
				}
				generatedKey = id
			}
		}
		if generatedKey == "" {
			return nil, nil
		}
		state := p.streamTools[generatedKey]
		delete(p.streamTools, generatedKey)
		p.streamTools[toolID] = state
		state.call.ID = toolID
		state.generatedID = false
		for i, id := range p.streamToolOrder {
			if id == generatedKey {
				p.streamToolOrder[i] = toolID
				break
			}
		}
		return state, nil
	}

	var found *streamToolState
	for _, state := range p.streamTools {
		if state.complete || (name != "" && state.call.Function.Name != name) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("awsstream: ambiguous toolUseEvent without toolUseId")
		}
		found = state
	}
	return found, nil
}

func appendStreamToolInput(state *streamToolState, input any) error {
	switch value := input.(type) {
	case nil:
		return nil
	case string:
		state.arguments += value
		return nil
	case map[string]any:
		// Kiro commonly starts a tool with input:{} and streams the actual JSON
		// afterward. An empty object is a start marker, not an argument snapshot.
		if len(value) == 0 {
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode tool input snapshot: %w", err)
		}
		return replaceStreamToolSnapshot(state, string(encoded))
	case []any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode tool input snapshot: %w", err)
		}
		return replaceStreamToolSnapshot(state, string(encoded))
	default:
		return fmt.Errorf("unsupported tool input type %T", input)
	}
}

func replaceStreamToolSnapshot(state *streamToolState, snapshot string) error {
	if state.arguments == "" || state.arguments == snapshot {
		state.arguments = snapshot
		return nil
	}
	if _, ok := reencodeJSON(state.arguments); ok {
		state.arguments = snapshot
		return nil
	}
	return fmt.Errorf("input snapshot followed an incomplete string delta")
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

// GetToolCalls returns completed binary calls in first-seen order. Legacy
// plain-JSON parsing retains its historical single-call finalization behavior.
func (p *Parser) GetToolCalls() []ToolCall {
	if p.streamMode == streamModeBinary {
		all := make([]ToolCall, 0, len(p.streamToolOrder))
		for _, id := range p.streamToolOrder {
			if state := p.streamTools[id]; state != nil && state.complete {
				all = append(all, state.call)
			}
		}
		return DeduplicateToolCalls(all)
	}
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
	p.frameBuffer = nil
	p.streamMode = streamModeUnknown
	p.lastContent = nil
	p.currentToolCall = nil
	p.toolCalls = nil
	p.streamTools = nil
	p.streamToolOrder = nil
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

var taggedToolNames = []string{"tool_use", "tool_call", "function_call", "tool"}

// ParseTaggedToolCalls recovers Opus-class tool descriptors emitted as tagged
// text. Only advertised tool names (or narrow unambiguous aliases) are accepted;
// malformed, fenced, incomplete, or unknown calls remain ordinary text.
func ParseTaggedToolCalls(text string, advertised []string) (string, []ToolCall) {
	allowed := make(map[string]string, len(advertised))
	for _, name := range advertised {
		allowed[strings.ToLower(name)] = name
	}
	if len(allowed) == 0 || text == "" {
		return text, nil
	}
	type span struct{ start, end int }
	var spans []span
	var calls []ToolCall
	for cursor := 0; cursor < len(text); {
		start, tag := nextTaggedTool(text, cursor)
		if start < 0 {
			break
		}
		open, close := "<"+tag+">", "</"+tag+">"
		bodyStart := start + len(open)
		closeAt := strings.Index(text[bodyStart:], close)
		if closeAt < 0 {
			break
		}
		closeAt += bodyStart
		end := closeAt + len(close)
		if insideFence(text, start) {
			cursor = end
			continue
		}
		body := strings.TrimSpace(text[bodyStart:closeAt])
		if len(body) == 0 || body[0] != '{' || FindMatchingBrace(body, 0) != len(body)-1 {
			cursor = end
			continue
		}
		var raw any
		if json.Unmarshal([]byte(body), &raw) != nil {
			cursor = end
			continue
		}
		descriptors, ok := taggedDescriptors(raw, allowed)
		if !ok || len(descriptors) == 0 {
			cursor = end
			continue
		}
		calls = append(calls, descriptors...)
		spans = append(spans, span{start, end})
		cursor = end
	}
	for i := len(spans) - 1; i >= 0; i-- {
		text = text[:spans[i].start] + text[spans[i].end:]
	}
	return text, calls
}

func nextTaggedTool(text string, from int) (int, string) {
	best, tag := -1, ""
	for _, candidate := range taggedToolNames {
		at := strings.Index(text[from:], "<"+candidate+">")
		if at >= 0 {
			at += from
			if best < 0 || at < best {
				best, tag = at, candidate
			}
		}
	}
	return best, tag
}

func insideFence(text string, position int) bool {
	return strings.Count(text[:position], "```")%2 == 1
}

func taggedDescriptors(raw any, allowed map[string]string) ([]ToolCall, bool) {
	values := []any{raw}
	if object, ok := raw.(map[string]any); ok {
		if wrapped, exists := object["tool_calls"]; exists {
			values, ok = wrapped.([]any)
			if !ok {
				return nil, false
			}
		} else if wrapped, exists := object["calls"]; exists {
			values, ok = wrapped.([]any)
			if !ok {
				return nil, false
			}
		}
	}
	result := make([]ToolCall, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		name, _ := object["tool_name"].(string)
		if name == "" {
			name, _ = object["name"].(string)
		}
		canonical, ok := advertisedToolName(name, allowed)
		if !ok {
			return nil, false
		}
		arguments := object["tool_input"]
		if arguments == nil {
			arguments = object["input"]
		}
		if arguments == nil {
			arguments = object["arguments"]
		}
		if arguments == nil {
			arguments = map[string]any{}
		}
		if _, ok := arguments.(map[string]any); !ok {
			return nil, false
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, false
		}
		result = append(result, ToolCall{ID: util.GenerateToolCallID(), Type: "function", Function: ToolCallFunction{Name: canonical, Arguments: string(encoded)}})
	}
	return result, true
}

func advertisedToolName(name string, allowed map[string]string) (string, bool) {
	if canonical, ok := allowed[strings.ToLower(name)]; ok {
		return canonical, true
	}
	aliases := map[string]string{
		"read_file": "read", "readfile": "read", "fs_read": "read", "view_file": "read",
		"write_file": "write", "writefile": "write", "fs_write": "write", "create_file": "write",
		"edit_file": "edit", "editfile": "edit", "str_replace": "edit", "str_replace_editor": "edit", "apply_patch": "edit",
		"list_directory": "ls", "list_dir": "ls", "list_files": "ls", "find_files": "find", "file_search": "find",
	}
	target, ok := aliases[strings.ToLower(name)]
	if !ok {
		return "", false
	}
	canonical, ok := allowed[target]
	return canonical, ok
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
