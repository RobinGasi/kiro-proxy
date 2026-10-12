package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"git.gasi.dev/robin/kiro-proxy/internal/converter"
	"git.gasi.dev/robin/kiro-proxy/internal/mcp"
	"git.gasi.dev/robin/kiro-proxy/internal/streaming"
	"git.gasi.dev/robin/kiro-proxy/internal/thinking"
	"git.gasi.dev/robin/kiro-proxy/internal/tokenizer"
	"git.gasi.dev/robin/kiro-proxy/internal/usage"
	"git.gasi.dev/robin/kiro-proxy/internal/util"
	"time"
)

// streamState tracks the open content blocks and accumulated output while
// translating a unified Kiro event stream into Anthropic SSE.
type streamState struct {
	messageID string
	model     string

	// ctx and am support the Path B (MCP emulation) interception of the model's
	// web_search tool_use; they are nil only when web search is unavailable.
	ctx context.Context
	am  mcp.AuthManager

	currentIndex int

	// toolNameReverse restores tool names that were aliased to satisfy Kiro's
	// 64-character limit (nil when nothing was shortened).
	toolNameReverse map[string]string

	thinkingStarted bool
	thinkingIndex   int
	textStarted     bool
	textIndex       int
	signature       string
	nativeThinking  bool
	redactedPending string

	toolCount    int
	stopMatcher  *stopMatcher
	stopSequence string

	content  strings.Builder
	thinking strings.Builder

	contextPct  float64
	creditsUsed *float64
	cacheRead   *int
	cacheCreate *int
	stopReason  string
	stopDetails map[string]any
}

// streamMessages translates the unified Kiro event stream (evCh/errCh, produced
// by streaming.StreamWithFirstTokenRetry) into the Anthropic SSE event sequence
// and writes it to w.
//
// It defers writing any SSE until the first event arrives, so that an upstream
// failure that produces no events (e.g. a non-200 status surfaced as
// *streaming.UpstreamStatusError) can be reported by the caller as a proper HTTP
// error response. The returned committed flag reports whether any SSE bytes were
// written: when false the caller may still send a JSON error with a real status
// code; when true the response is already an event stream and a mid-stream error
// is emitted as an Anthropic error event.
func (h *Handler) streamMessages(ctx context.Context, w http.ResponseWriter, am mcp.AuthManager, model string, inputTokens, contextLimit int, asReasoning bool, toolNameReverse map[string]string, stopSequences []string, evCh <-chan streaming.KiroEvent, errCh <-chan error) (committed bool, err error) {
	interval := 10 * time.Second
	if h.cfg != nil && h.cfg.AnthropicPingInterval > 0 {
		interval = time.Duration(h.cfg.AnthropicPingInterval * float64(time.Second))
	}
	return h.streamMessagesWithPing(ctx, w, am, model, inputTokens, contextLimit, asReasoning, toolNameReverse, stopSequences, evCh, errCh, interval)
}

func (h *Handler) streamMessagesWithPing(ctx context.Context, w http.ResponseWriter, am mcp.AuthManager, model string, inputTokens, contextLimit int, asReasoning bool, toolNameReverse map[string]string, stopSequences []string, evCh <-chan streaming.KiroEvent, errCh <-chan error, pingInterval time.Duration) (committed bool, err error) {
	st := &streamState{
		messageID:       util.GenerateMessageID(),
		model:           model,
		ctx:             ctx,
		am:              am,
		toolNameReverse: toolNameReverse,
		stopMatcher:     newStopMatcher(stopSequences),
		signature:       genThinkingSignature(),
	}

	started := false
	ensureStart := func() error {
		if started {
			return nil
		}
		setSSEHeaders(w)
		started = true
		return writeSSE(w, "message_start", messageStartEvent{
			Type: "message_start",
			Message: messageStartBody{
				ID:      st.messageID,
				Type:    "message",
				Role:    "assistant",
				Content: []any{},
				Model:   model,
				Usage:   Usage{InputTokens: inputTokens, OutputTokens: 0},
			},
		})
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for evCh != nil {
		select {
		case <-ctx.Done():
			drain(evCh)
			return started, ctx.Err()
		case <-ping.C:
			if e := ensureStart(); e != nil {
				drain(evCh)
				return true, e
			}
			if e := writeSSE(w, "ping", pingEvent{Type: "ping"}); e != nil {
				drain(evCh)
				return true, e
			}
		case ev, ok := <-evCh:
			if !ok {
				evCh = nil
				continue
			}
			if e := ensureStart(); e != nil {
				drain(evCh)
				return true, e
			}
			if e := h.processEvent(w, st, asReasoning, ev); e != nil {
				drain(evCh)
				return true, e
			}
		}
	}

	var streamErr error
	if errCh != nil {
		if e, ok := <-errCh; ok {
			streamErr = e
		}
	}

	// No SSE written yet: let the caller produce a real HTTP error/response.
	if !started {
		if streamErr != nil {
			return false, streamErr
		}
		if e := ensureStart(); e != nil {
			return true, e
		}
	}

	if streamErr != nil {
		_, errType, _, message := classifyUpstreamError(streamErr)
		_ = writeSSE(w, "error", errorEvent{
			Type:  "error",
			Error: errorBody{Type: errType, Message: message},
		})
		return true, streamErr
	}

	if e := h.finalizeStream(w, st, contextLimit); e != nil {
		return true, e
	}
	return true, nil
}

// processEvent translates a single KiroEvent into the appropriate Anthropic SSE
// content-block events, mutating st as blocks open and close.
func (h *Handler) processEvent(w io.Writer, st *streamState, asReasoning bool, ev streaming.KiroEvent) error {
	switch ev.Type {
	case streaming.EventThinking:
		if ev.NativeThinking {
			st.nativeThinking = true
			if ev.ThinkingSignature != "" {
				st.signature = ev.ThinkingSignature
			}
			if ev.RedactedThinkingData != "" {
				st.redactedPending = ev.RedactedThinkingData
			}
		}
		return h.handleThinking(w, st, asReasoning, ev.ThinkingContent)
	case streaming.EventContent:
		return h.handleContent(w, st, ev.Content)

	case streaming.EventToolUse:
		return h.handleToolUse(w, st, ev)

	case streaming.EventContextUsage:
		st.contextPct = ev.ContextUsagePercentage
		return nil

	case streaming.EventUsage:
		applyUsage(st, ev.Usage)
		return nil
	case streaming.EventMetadata:
		if st.stopSequence == "" {
			st.stopReason = ev.StopReason
		}
		st.stopDetails = ev.StopDetails
		return nil
	}
	return nil
}

// handleThinking emits thinking content either as a native Anthropic thinking
// block (as_reasoning_content) or folded into a text block (otherwise).
func (h *Handler) handleThinking(w io.Writer, st *streamState, asReasoning bool, content string) error {
	if content != "" {
		st.thinking.WriteString(content)
	}
	if !asReasoning || (content == "" && st.signature == "" && st.redactedPending == "") {
		return nil
	}

	if st.redactedPending != "" {
		if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
			Type: "content_block_start", Index: st.currentIndex,
			ContentBlock: RedactedThinkingBlock{Type: "redacted_thinking", Data: st.redactedPending},
		}); err != nil {
			return err
		}
		if err := writeSSE(w, "content_block_stop", contentBlockStopEvent{Type: "content_block_stop", Index: st.currentIndex}); err != nil {
			return err
		}
		st.currentIndex++
		st.redactedPending = ""
		return nil
	}

	if !st.thinkingStarted {
		st.thinkingIndex = st.currentIndex
		if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
			Type:         "content_block_start",
			Index:        st.thinkingIndex,
			ContentBlock: ThinkingBlock{Type: "thinking", Thinking: "", Signature: ""},
		}); err != nil {
			return err
		}
		st.thinkingStarted = true
	}
	if content == "" {
		return nil
	}
	return writeSSE(w, "content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: st.thinkingIndex,
		Delta: thinkingDelta{Type: "thinking_delta", Thinking: content},
	})
}

// handleContent emits regular text content, closing any open thinking block and
// opening a text block on demand.
func (h *Handler) handleContent(w io.Writer, st *streamState, content string) error {
	if st.stopSequence != "" {
		return nil
	}
	if st.stopMatcher != nil {
		var matched string
		content, matched = st.stopMatcher.Feed(content)
		if matched != "" {
			st.stopSequence = matched
			st.stopReason = "STOP_SEQUENCE"
		}
	}
	if content == "" {
		return nil
	}
	return h.emitText(w, st, content)
}

func (h *Handler) emitText(w io.Writer, st *streamState, content string) error {
	if err := st.closeThinking(w); err != nil {
		return err
	}
	if !st.textStarted {
		st.textIndex = st.currentIndex
		if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
			Type: "content_block_start", Index: st.textIndex,
			ContentBlock: TextBlock{Type: "text", Text: ""},
		}); err != nil {
			return err
		}
		st.textStarted = true
	}
	st.content.WriteString(content)
	return writeSSE(w, "content_block_delta", contentBlockDeltaEvent{
		Type: "content_block_delta", Index: st.textIndex,
		Delta: textDelta{Type: "text_delta", Text: content},
	})
}

// handleToolUse emits a complete tool_use block (start, input delta, stop) after
// closing any open thinking/text blocks.
func (h *Handler) handleToolUse(w io.Writer, st *streamState, ev streaming.KiroEvent) error {
	if err := st.closeThinking(w); err != nil {
		return err
	}
	if err := st.closeText(w); err != nil {
		return err
	}

	id, name, argsJSON := toolCallParts(ev)

	// Restore any name that was aliased to satisfy Kiro's tool-name length limit,
	// so the client sees the original tool name it sent (web_search is never
	// aliased, so this is a no-op for the interception path below).
	name = converter.RestoreToolName(st.toolNameReverse, name)

	// Path B (MCP emulation): intercept the model's web_search tool_use, call the
	// Kiro MCP API directly, and emit server_tool_use + web_search_tool_result +
	// text-summary blocks in place of the raw tool_use. On MCP failure we fall
	// through to the normal tool_use block; an empty query drops the call entirely.
	if name == mcp.WebSearchToolName && h.mcp != nil && st.am != nil {
		handled, err := h.interceptWebSearch(w, st, argsJSON)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}

	input := parseToolInput(argsJSON)

	if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        st.currentIndex,
		ContentBlock: ToolUseBlock{Type: "tool_use", ID: id, Name: name, Input: map[string]any{}},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: st.currentIndex,
		Delta: inputJSONDelta{Type: "input_json_delta", PartialJSON: input},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_stop", contentBlockStopEvent{
		Type: "content_block_stop", Index: st.currentIndex,
	}); err != nil {
		return err
	}
	st.currentIndex++
	st.toolCount++
	return nil
}

// interceptWebSearch services an intercepted web_search tool call (Path B). It
// calls the Kiro MCP API and emits the server_tool_use, web_search_tool_result
// and text-summary blocks in place of the raw tool_use. It returns handled=true
// when the call was fully serviced or deliberately dropped (empty query), and
// handled=false when the caller should fall back to the normal tool_use block
// (e.g. the MCP API failed). Intercepted blocks are emitted inline and are NOT
// counted toward output tokens or the tool count, so the final stop_reason is
// unaffected.
func (h *Handler) interceptWebSearch(w io.Writer, st *streamState, argsJSON string) (handled bool, err error) {
	query := webSearchQuery(argsJSON)
	if query == "" {
		// No query: skip the MCP call and drop the tool.
		return true, nil
	}

	_, results, callErr := h.mcp.CallKiroMCPAPI(st.ctx, st.am, query)
	if callErr != nil || results == nil {
		// Fall back to the normal tool_use block so the failure surfaces.
		return false, nil
	}

	toolUseID := "srvtoolu_" + util.UUIDHex()
	queryJSON, jerr := marshalNoEscape(map[string]string{"query": query})
	if jerr != nil {
		return false, nil
	}

	// server_tool_use block.
	if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
		Type:  "content_block_start",
		Index: st.currentIndex,
		ContentBlock: map[string]any{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  mcp.WebSearchToolName,
			"input": map[string]any{},
		},
	}); err != nil {
		return true, err
	}
	if err := writeSSE(w, "content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: st.currentIndex,
		Delta: inputJSONDelta{Type: "input_json_delta", PartialJSON: string(queryJSON)},
	}); err != nil {
		return true, err
	}
	if err := writeSSE(w, "content_block_stop", contentBlockStopEvent{
		Type: "content_block_stop", Index: st.currentIndex,
	}); err != nil {
		return true, err
	}
	st.currentIndex++

	// web_search_tool_result block.
	if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
		Type:  "content_block_start",
		Index: st.currentIndex,
		ContentBlock: map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     mcp.SearchResultBlocks(results),
		},
	}); err != nil {
		return true, err
	}
	if err := writeSSE(w, "content_block_stop", contentBlockStopEvent{
		Type: "content_block_stop", Index: st.currentIndex,
	}); err != nil {
		return true, err
	}
	st.currentIndex++

	// text summary block (streamed in fixed-size chunks).
	if err := writeSSE(w, "content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        st.currentIndex,
		ContentBlock: TextBlock{Type: "text", Text: ""},
	}); err != nil {
		return true, err
	}
	for _, chunk := range mcp.ChunkSummary(mcp.GenerateSearchSummary(query, results)) {
		if err := writeSSE(w, "content_block_delta", contentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: st.currentIndex,
			Delta: textDelta{Type: "text_delta", Text: chunk},
		}); err != nil {
			return true, err
		}
	}
	if err := writeSSE(w, "content_block_stop", contentBlockStopEvent{
		Type: "content_block_stop", Index: st.currentIndex,
	}); err != nil {
		return true, err
	}
	st.currentIndex++

	return true, nil
}

// webSearchQuery extracts the "query" argument from a tool call's raw JSON
// arguments, returning "" when absent or unparseable.
func webSearchQuery(argsJSON string) string {
	s := strings.TrimSpace(argsJSON)
	if s == "" {
		return ""
	}
	var args struct {
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(s), &args) != nil {
		return ""
	}
	return strings.TrimSpace(args.Query)
}

// finalizeStream closes any open blocks and emits the terminal message_delta and
// message_stop events, computing output tokens and the final stop reason.
func (h *Handler) finalizeStream(w io.Writer, st *streamState, contextLimit int) error {
	if st.stopMatcher != nil && st.stopSequence == "" {
		if trailing := st.stopMatcher.Flush(); trailing != "" {
			if err := h.emitText(w, st, trailing); err != nil {
				return err
			}
		}
	}
	if err := st.closeThinking(w); err != nil {
		return err
	}
	if err := st.closeText(w); err != nil {
		return err
	}

	outputTokens := tokenizer.CountTokens(st.content.String()) + tokenizer.CountTokens(st.thinking.String())

	stopReason := anthropicStopReason(st.stopReason, st.toolCount > 0)
	if stopReason == "end_turn" && st.contextPct == 0 && st.content.Len() > 0 && st.toolCount == 0 {
		stopReason = "max_tokens"
	}

	var matchedSequence *string
	if st.stopSequence != "" {
		matchedSequence = &st.stopSequence
	}
	if err := writeSSE(w, "message_delta", messageDeltaEvent{
		Type:  "message_delta",
		Delta: messageDeltaBody{StopReason: stopReason, StopSequence: matchedSequence, StopDetails: st.stopDetails},
		Usage: DeltaUsage{
			OutputTokens:             outputTokens,
			CacheReadInputTokens:     st.cacheRead,
			CacheCreationInputTokens: st.cacheCreate,
			CreditsUsed:              st.creditsUsed,
		},
	}); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", messageStopEvent{Type: "message_stop"})
}

func anthropicStopReason(stopReason string, hasTools bool) string {
	if hasTools || strings.EqualFold(stopReason, "TOOL_USE") {
		return "tool_use"
	}
	switch strings.ToUpper(strings.TrimSpace(stopReason)) {
	case "MAX_TOKENS":
		return "max_tokens"
	case "STOP_SEQUENCE":
		return "stop_sequence"
	case "REFUSAL", "CONTENT_FILTERED", "GUARDRAIL_INTERVENED":
		return "refusal"
	default:
		return "end_turn"
	}
}

// closeThinking writes the opaque signature before closing a thinking block.
func (st *streamState) closeThinking(w io.Writer) error {
	if !st.thinkingStarted {
		return nil
	}
	if st.signature != "" {
		if err := writeSSE(w, "content_block_delta", contentBlockDeltaEvent{
			Type: "content_block_delta", Index: st.thinkingIndex,
			Delta: signatureDelta{Type: "signature_delta", Signature: st.signature},
		}); err != nil {
			return err
		}
	}
	st.thinkingStarted = false
	st.currentIndex++
	return writeSSE(w, "content_block_stop", contentBlockStopEvent{
		Type: "content_block_stop", Index: st.thinkingIndex,
	})
}

// closeText closes the text block if it is open and advances the index.
func (st *streamState) closeText(w io.Writer) error {
	if !st.textStarted {
		return nil
	}
	st.textStarted = false
	st.currentIndex++
	return writeSSE(w, "content_block_stop", contentBlockStopEvent{
		Type: "content_block_stop", Index: st.textIndex,
	})
}

// --- non-streaming collection ---

// buildMessagesResponse assembles the non-streaming Messages response from a
// collected StreamResult, ordering output blocks as thinking, text, tool_use.
func buildMessagesResponse(result *streaming.StreamResult, model string, inputTokens, contextLimit int, asReasoning bool, toolNameReverse map[string]string, stopSequences ...string) MessagesResponse {
	blocks := make([]any, 0, 3+len(result.ToolCalls))

	if result.RedactedThinkingData != "" && asReasoning {
		blocks = append(blocks, RedactedThinkingBlock{Type: "redacted_thinking", Data: result.RedactedThinkingData})
	}
	if (result.ThinkingContent != "" || result.ThinkingSignature != "") && asReasoning {
		signature := result.ThinkingSignature
		if signature == "" {
			signature = genThinkingSignature()
		}
		blocks = append(blocks, ThinkingBlock{
			Type: "thinking", Thinking: result.ThinkingContent, Signature: signature,
		})
	}

	text, matchedSequence := truncateAtStopSequence(result.Content, stopSequences)
	if text != "" {
		blocks = append(blocks, TextBlock{Type: "text", Text: text})
	}

	for _, tc := range result.ToolCalls {
		id := tc.ID
		if id == "" {
			id = "toolu_" + util.UUIDHex()
		}
		blocks = append(blocks, ToolUseBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  converter.RestoreToolName(toolNameReverse, tc.Function.Name),
			Input: parseToolInputMap(tc.Function.Arguments),
		})
	}

	outputTokens := tokenizer.CountTokens(result.Content) + tokenizer.CountTokens(result.ThinkingContent)
	if result.ContextUsagePercentage > 0 {
		prompt, _, source, _ := tokenizer.CalculateTokensFromContextUsage(result.ContextUsagePercentage, outputTokens, contextLimit)
		if source != "unknown" {
			inputTokens = prompt
		}
	}

	stopReason := anthropicStopReason(result.StopReason, len(result.ToolCalls) > 0)
	var stopSequence *string
	if matchedSequence != "" {
		stopReason = "stop_sequence"
		stopSequence = &matchedSequence
	} else if stopReason == "end_turn" && result.ContextUsagePercentage == 0 && len(result.Content) > 0 && len(result.ToolCalls) == 0 {
		stopReason = "max_tokens"
	}

	u := Usage{InputTokens: inputTokens, OutputTokens: outputTokens}
	cacheRead, cacheCreate := cacheUsage(result.Usage)
	u.CacheReadInputTokens = cacheRead
	u.CacheCreationInputTokens = cacheCreate
	if credits, ok := creditsFromUsage(result.Usage); ok {
		u.CreditsUsed = &credits
	}

	return MessagesResponse{
		ID:           util.GenerateMessageID(),
		Type:         "message",
		Role:         "assistant",
		Content:      blocks,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: stopSequence,
		StopDetails:  result.StopDetails,
		Usage:        u,
	}
}

// --- helpers ---

// applyUsage records cache and credit usage from a Kiro usage event payload.
func applyUsage(st *streamState, u map[string]any) {
	if cacheRead, cacheCreate := cacheUsage(u); cacheRead != nil || cacheCreate != nil {
		if cacheRead != nil {
			st.cacheRead = cacheRead
		}
		if cacheCreate != nil {
			st.cacheCreate = cacheCreate
		}
	}
	if credits, ok := creditsFromUsage(u); ok {
		st.creditsUsed = &credits
	}
}

// creditsFromUsage extracts the credits consumed from a usage map, unwrapping
// the {"usage": <number>} envelope the streaming core uses for bare numeric
// usage values.
func creditsFromUsage(u map[string]any) (float64, bool) {
	if u == nil {
		return 0, false
	}
	if v, ok := u["usage"]; ok && len(u) == 1 {
		return usage.CreditsFromUsageEvent(v)
	}
	return usage.CreditsFromUsageEvent(u)
}

// creditsRemaining extracts a remaining-credits hint from a usage payload for
// the x-kiro-credits-remaining response header, when present. It mirrors the
// OpenAI surface's helper (openai/streaming.go) exactly so both surfaces report
// the same value from the same upstream keys.
func creditsRemaining(raw map[string]any) (string, bool) {
	if raw == nil {
		return "", false
	}
	for _, k := range []string{"remainingCredits", "remaining", "credits_remaining", "available"} {
		if v, ok := raw[k]; ok {
			switch n := v.(type) {
			case float64:
				return strconv.FormatFloat(n, 'f', -1, 64), true
			case json.Number:
				return n.String(), true
			}
		}
	}
	return "", false
}

// cacheUsage extracts cache read/creation token counts from a usage map,
// accepting both snake_case and camelCase keys.
func cacheUsage(u map[string]any) (read, create *int) {
	if u == nil {
		return nil, nil
	}
	read = firstIntKey(u, "cache_read_input_tokens", "cacheReadInputTokens")
	create = firstIntKey(u, "cache_creation_input_tokens", "cacheCreationInputTokens")
	return read, create
}

// firstIntKey returns a pointer to the integer value of the first present key,
// or nil when none of the keys hold a numeric value.
func firstIntKey(m map[string]any, keys ...string) *int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		case json.Number:
			if f, err := n.Float64(); err == nil {
				i := int(f)
				return &i
			}
		}
	}
	return nil
}

// toolCallParts extracts the id, name and raw JSON arguments string from a
// tool-use KiroEvent, preferring the typed ToolCall and falling back to the map.
func toolCallParts(ev streaming.KiroEvent) (id, name, argsJSON string) {
	if ev.ToolCall != nil {
		id = ev.ToolCall.ID
		name = ev.ToolCall.Function.Name
		argsJSON = ev.ToolCall.Function.Arguments
	}
	if ev.ToolUse != nil {
		if id == "" {
			id, _ = ev.ToolUse["id"].(string)
		}
		if fn, ok := ev.ToolUse["function"].(map[string]any); ok {
			if name == "" {
				name, _ = fn["name"].(string)
			}
			if argsJSON == "" {
				argsJSON, _ = fn["arguments"].(string)
			}
		}
	}
	if id == "" {
		id = "toolu_" + util.UUIDHex()
	}
	return id, name, argsJSON
}

// parseToolInput normalizes a raw tool arguments JSON string for an
// input_json_delta payload, returning "{}" when it is empty or invalid.
func parseToolInput(argsJSON string) string {
	s := strings.TrimSpace(argsJSON)
	if s == "" {
		return "{}"
	}
	if !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

// parseToolInputMap parses tool-call arguments (a JSON string or a map) into a
// map for a non-streaming tool_use block, returning an empty map on failure.
func parseToolInputMap(args any) map[string]any {
	switch v := args.(type) {
	case map[string]any:
		if v == nil {
			return map[string]any{}
		}
		return v
	case string:
		if strings.TrimSpace(v) == "" {
			return map[string]any{}
		}
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return m
		}
	}
	return map[string]any{}
}

// genThinkingSignature returns a placeholder signature for thinking blocks (this
// proxy emits fake-reasoning content, which carries no real signature).
func genThinkingSignature() string { return "sig_" + util.UUIDHex() }

// asReasoningContent reports whether the configured fake-reasoning handling emits
// native Anthropic thinking blocks (vs folding reasoning into text).
func asReasoningContent(handling string) bool {
	return handling == thinking.HandleAsReasoningContent
}

// drain consumes any remaining events so the producing goroutine can exit after
// the consumer has stopped early (e.g. on a write error).
func drain(evCh <-chan streaming.KiroEvent) {
	go func() {
		for range evCh {
		}
	}()
}

// --- SSE / JSON writing ---

// writeSSE writes one "event: <type>\ndata: <json>\n\n" frame and flushes when
// possible.
func writeSSE(w io.Writer, eventType string, v any) error {
	payload, err := marshalNoEscape(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
		return err
	}
	flush(w)
	return nil
}

// flush flushes w when it implements http.Flusher so SSE frames reach the client
// immediately.
func flush(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// setSSEHeaders sets the event-stream headers and writes a 200 status.
func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// marshalNoEscape marshals v to compact JSON without HTML escaping, matching the
// reference json.dumps(..., ensure_ascii=False) so characters like '<' survive.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
