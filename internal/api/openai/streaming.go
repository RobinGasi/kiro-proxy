package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/awsstream"
	"git.gasi.dev/robin/kiro-proxy/internal/converter"
	"git.gasi.dev/robin/kiro-proxy/internal/mcp"
	"git.gasi.dev/robin/kiro-proxy/internal/streaming"
	"git.gasi.dev/robin/kiro-proxy/internal/usage"
	"git.gasi.dev/robin/kiro-proxy/internal/util"
)

const (
	objectChunk      = "chat.completion.chunk"
	objectCompletion = "chat.completion"
)

// streamAccumulator gathers the running state of a translated stream so token
// counts, tool calls and credits can be emitted in the final chunk.
type streamAccumulator struct {
	content       string
	thinking      string
	toolCalls     []awsstream.ToolCall
	usageRaw      map[string]any
	contextPct    float64
	reasoningMode string // cfg.FakeReasoningHandling
	// toolNameReverse restores tool names that were aliased to satisfy Kiro's
	// 64-character limit (nil when nothing was shortened).
	toolNameReverse map[string]string
}

func newAccumulator(reasoningMode string, toolNameReverse map[string]string) *streamAccumulator {
	return &streamAccumulator{reasoningMode: reasoningMode, toolNameReverse: toolNameReverse}
}

// apply records one KiroEvent and returns the OpenAI delta to emit for it (nil
// when the event produces no client-visible delta).
func (a *streamAccumulator) apply(ev streaming.KiroEvent) map[string]any {
	switch ev.Type {
	case streaming.EventContent:
		if ev.Content == "" {
			return nil
		}
		a.content += ev.Content
		return map[string]any{"content": ev.Content}
	case streaming.EventThinking:
		if ev.ThinkingContent == "" {
			return nil
		}
		a.thinking += ev.ThinkingContent
		if a.reasoningMode == "as_reasoning_content" {
			return map[string]any{"reasoning_content": ev.ThinkingContent}
		}
		return map[string]any{"content": ev.ThinkingContent}
	case streaming.EventToolUse:
		if ev.ToolCall != nil {
			a.toolCalls = append(a.toolCalls, *ev.ToolCall)
		}
		return nil
	case streaming.EventUsage:
		a.usageRaw = ev.Usage
		return nil
	case streaming.EventContextUsage:
		a.contextPct = ev.ContextUsagePercentage
		return nil
	}
	return nil
}

// finishReason returns the OpenAI finish_reason for the accumulated stream.
func (a *streamAccumulator) finishReason() string {
	if len(a.toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// usage builds the final OpenAI usage object (with credits_used when known).
func (a *streamAccumulator) usage(contextLimit int) Usage {
	res := &streaming.StreamResult{
		Content:                a.content,
		ThinkingContent:        a.thinking,
		Usage:                  a.usageRaw,
		ContextUsagePercentage: a.contextPct,
	}
	prompt, total, completion, _, _ := res.CorrectedTokens(contextLimit)
	u := Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
	if a.usageRaw != nil {
		if credits, ok := usage.CreditsFromUsageEvent(a.usageRaw); ok {
			u.CreditsUsed = &credits
		}
	}
	return u
}

// indexedToolCalls returns the tool calls in OpenAI streaming delta shape (with
// the required per-call index field).
func (a *streamAccumulator) indexedToolCalls() []map[string]any {
	out := make([]map[string]any, 0, len(a.toolCalls))
	for i, tc := range a.toolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		out = append(out, map[string]any{
			"index": i,
			"id":    tc.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      converter.RestoreToolName(a.toolNameReverse, tc.Function.Name),
				"arguments": args,
			},
		})
	}
	return out
}

// streamToWriter translates the KiroEvent stream into OpenAI SSE chunks written
// to w. Headers must already be committed by the caller. The terminal stream
// error (if any) is returned for logging; partial output may already have been
// written.
func streamToWriter(ctx context.Context, mcpClient *mcp.Client, am mcp.AuthManager, w http.ResponseWriter, model string, contextLimit int, reasoningMode string, toolNameReverse map[string]string, first streaming.KiroEvent, evCh <-chan streaming.KiroEvent, errCh <-chan error) error {
	flusher, _ := w.(http.Flusher)
	completionID := util.GenerateCompletionID()
	created := time.Now().Unix()
	acc := newAccumulator(reasoningMode, toolNameReverse)
	firstChunk := true

	writeDelta := func(delta map[string]any) {
		if firstChunk {
			delta["role"] = "assistant"
			firstChunk = false
		}
		writeChunk(w, flusher, ChatCompletionChunk{
			ID:      completionID,
			Object:  objectChunk,
			Created: created,
			Model:   model,
			Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: delta}},
		})
	}

	emit := func(ev streaming.KiroEvent) {
		delta := acc.apply(ev)
		if delta == nil {
			return
		}
		writeDelta(delta)
	}

	// intercept services the model's web_search tool_use (Path B / MCP emulation):
	// it calls the Kiro MCP API and streams the summary as ordinary content chunks
	// (OpenAI has no server-tool concept), accumulating it for token counting.
	// It returns true when fully handled; an empty query or an MCP failure falls
	// back to the normal tool_use path (returns false).
	intercept := func(ev streaming.KiroEvent) bool {
		if ev.Type != streaming.EventToolUse || mcpClient == nil || am == nil {
			return false
		}
		if openaiToolName(ev) != mcp.WebSearchToolName {
			return false
		}
		query := openaiWebSearchQuery(ev)
		if query == "" {
			return false
		}
		_, results, err := mcpClient.CallKiroMCPAPI(ctx, am, query)
		if err != nil || results == nil {
			return false
		}
		summary := mcp.GenerateSearchSummary(query, results)
		acc.content += summary
		for _, chunk := range mcp.ChunkSummary(summary) {
			writeDelta(map[string]any{"content": chunk})
		}
		return true
	}

	process := func(ev streaming.KiroEvent) {
		if intercept(ev) {
			return
		}
		emit(ev)
	}

	process(first)
	for ev := range evCh {
		process(ev)
	}
	streamErr := <-errCh

	// Tool-calls chunk (single chunk carrying all calls).
	if len(acc.toolCalls) > 0 {
		writeChunk(w, flusher, ChatCompletionChunk{
			ID:      completionID,
			Object:  objectChunk,
			Created: created,
			Model:   model,
			Choices: []ChatCompletionChunkChoice{{
				Index: 0,
				Delta: map[string]any{"tool_calls": acc.indexedToolCalls()},
			}},
		})
	}

	// Final chunk: finish_reason + usage.
	finish := acc.finishReason()
	u := acc.usage(contextLimit)
	writeChunk(w, flusher, ChatCompletionChunk{
		ID:      completionID,
		Object:  objectChunk,
		Created: created,
		Model:   model,
		Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: map[string]any{}, FinishReason: &finish}},
		Usage:   &u,
	})

	writeRaw(w, flusher, "data: [DONE]\n\n")
	return streamErr
}

// buildNonStreamingResponse collects a complete StreamResult into one
// chat.completion object.
func buildNonStreamingResponse(model string, contextLimit int, reasoningMode string, toolNameReverse map[string]string, res *streaming.StreamResult) ChatCompletionResponse {
	message := map[string]any{"role": "assistant", "content": res.Content}
	if res.ThinkingContent != "" && reasoningMode == "as_reasoning_content" {
		message["reasoning_content"] = res.ThinkingContent
	}

	finish := "stop"
	if len(res.ToolCalls) > 0 {
		finish = "tool_calls"
		calls := make([]map[string]any, 0, len(res.ToolCalls))
		for _, tc := range res.ToolCalls {
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      converter.RestoreToolName(toolNameReverse, tc.Function.Name),
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
	}

	prompt, total, completion, _, _ := res.CorrectedTokens(contextLimit)
	u := Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
	if res.Usage != nil {
		if credits, ok := usage.CreditsFromUsageEvent(res.Usage); ok {
			u.CreditsUsed = &credits
		}
	}

	return ChatCompletionResponse{
		ID:      util.GenerateCompletionID(),
		Object:  objectCompletion,
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{{Index: 0, Message: message, FinishReason: finish}},
		Usage:   u,
	}
}

// creditsRemaining extracts a remaining-credits hint from a usage payload for
// the x-kiro-credits-remaining response header, when present.
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

// openaiToolName extracts a tool call's function name from a tool-use event,
// preferring the typed ToolCall and falling back to the OpenAI-shaped map.
func openaiToolName(ev streaming.KiroEvent) string {
	if ev.ToolCall != nil && ev.ToolCall.Function.Name != "" {
		return ev.ToolCall.Function.Name
	}
	if ev.ToolUse != nil {
		if fn, ok := ev.ToolUse["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				return n
			}
		}
		if n, ok := ev.ToolUse["name"].(string); ok {
			return n
		}
	}
	return ""
}

// openaiWebSearchQuery extracts the "query" argument from a tool-use event's
// JSON-string arguments, returning "" when absent or unparseable.
func openaiWebSearchQuery(ev streaming.KiroEvent) string {
	var args string
	if ev.ToolCall != nil {
		args = ev.ToolCall.Function.Arguments
	}
	if args == "" && ev.ToolUse != nil {
		if fn, ok := ev.ToolUse["function"].(map[string]any); ok {
			args, _ = fn["arguments"].(string)
		}
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return ""
	}
	var parsed struct {
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(args), &parsed) != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query)
}

func writeChunk(w http.ResponseWriter, flusher http.Flusher, chunk ChatCompletionChunk) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	writeRaw(w, flusher, fmt.Sprintf("data: %s\n\n", b))
}

func writeRaw(w http.ResponseWriter, flusher http.Flusher, s string) {
	_, _ = w.Write([]byte(s))
	if flusher != nil {
		flusher.Flush()
	}
}
