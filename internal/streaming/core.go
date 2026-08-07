// Package streaming contains the core streaming logic for parsing Kiro API
// responses into a unified, API-agnostic event stream.
//
// The unified KiroEvent format produced here is consumed by both the OpenAI and
// Anthropic API formatters, which convert it into their respective SSE wire
// formats.
//
// The package provides three pieces:
//
//   - KiroEvent / StreamResult: the unified event and the collected full-stream
//     result.
//   - StreamWithFirstTokenRetry: a streaming driver that reads an upstream
//     *http.Response, drives an awsstream.Parser, and re-issues the request when
//     the model fails to produce a first token within FirstTokenTimeout.
//   - CollectStreamResult: consumes the whole event stream into a StreamResult.
package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/awsstream"
	"git.gasi.dev/robin/kiro-proxy/internal/config"
	"git.gasi.dev/robin/kiro-proxy/internal/debuglog"
	"git.gasi.dev/robin/kiro-proxy/internal/thinking"
	"git.gasi.dev/robin/kiro-proxy/internal/tokenizer"
)

// Default streaming timeouts, applied when the corresponding config value is
// non-positive.
const (
	DefaultFirstTokenTimeout    = 15 * time.Second
	DefaultStreamingReadTimeout = 300 * time.Second
	DefaultFirstTokenMaxRetries = 3
)

// Unified KiroEvent type identifiers.
const (
	EventContent      = "content"
	EventThinking     = "thinking"
	EventToolUse      = "tool_use"
	EventUsage        = "usage"
	EventContextUsage = "context_usage"
)

// readBufferSize is the chunk size used for bounded reads of the response body.
const readBufferSize = 64 * 1024

// KiroEvent is a unified event from the Kiro API stream. It is API-agnostic and
// can be converted to both OpenAI and Anthropic formats by the API layer.
//
// Only the fields relevant to Type are populated:
//
//   - EventContent:      Content
//   - EventThinking:     ThinkingContent, IsFirstThinkingChunk, IsLastThinkingChunk
//   - EventToolUse:      ToolUse (OpenAI-shaped map) and ToolCall (typed)
//   - EventUsage:        Usage
//   - EventContextUsage: ContextUsagePercentage
type KiroEvent struct {
	// Type is one of the Event* constants.
	Type string
	// Content is the text content fragment (EventContent).
	Content string
	// ThinkingContent is the reasoning text fragment (EventThinking).
	ThinkingContent string
	// ToolUse is the reconstructed tool call in OpenAI map shape
	// ({"id","type","function":{"name","arguments"}}), for EventToolUse.
	ToolUse map[string]any
	// ToolCall is the typed form of the same tool call, carried alongside ToolUse
	// so collectors can accumulate awsstream.ToolCall values without re-parsing.
	ToolCall *awsstream.ToolCall
	// Usage is the metering payload for EventUsage.
	Usage map[string]any
	// ContextUsagePercentage is the Kiro context-usage percentage for
	// EventContextUsage.
	ContextUsagePercentage float64
	// IsFirstThinkingChunk reports whether ThinkingContent is the first delta of
	// the thinking block.
	IsFirstThinkingChunk bool
	// IsLastThinkingChunk reports whether the thinking block just closed.
	IsLastThinkingChunk bool
}

// StreamResult is the accumulated result of consuming a complete stream.
type StreamResult struct {
	// Content is the full assistant text.
	Content string
	// ThinkingContent is the full reasoning text.
	ThinkingContent string
	// ToolCalls is the deduplicated set of tool calls (native + bracket-style).
	ToolCalls []awsstream.ToolCall
	// Usage is the last usage payload seen, if any.
	Usage map[string]any
	// ContextUsagePercentage is the context-usage percentage reported by Kiro.
	ContextUsagePercentage float64
	// StopReason is left empty by the core layer; API formatters set the value
	// appropriate to their wire format.
	StopReason string
}

// CorrectedTokens derives prompt/total/completion token counts from the Kiro
// context-usage percentage (bug #13: these corrected counts are made available
// to callers rather than only logged).
//
// completionTokens is counted from the accumulated content and thinking text.
// contextLimit is the model's maximum input tokens (obtained from the model
// cache by the caller). See tokenizer.CalculateTokensFromContextUsage for the
// derivation and the meaning of the source strings.
func (r *StreamResult) CorrectedTokens(contextLimit int) (promptTokens, totalTokens, completionTokens int, promptSource, totalSource string) {
	completionTokens = tokenizer.CountTokens(r.Content) + tokenizer.CountTokens(r.ThinkingContent)
	promptTokens, totalTokens, promptSource, totalSource = tokenizer.CalculateTokensFromContextUsage(
		r.ContextUsagePercentage, completionTokens, contextLimit)
	return
}

// FirstTokenTimeoutError is returned when the model produces no first token
// within the configured FirstTokenTimeout. It is retryable by the streaming
// driver.
type FirstTokenTimeoutError struct {
	Timeout time.Duration
}

func (e *FirstTokenTimeoutError) Error() string {
	return fmt.Sprintf("no response within %s (first token timeout)", e.Timeout)
}

// StreamingReadTimeoutError is returned when a subsequent read (after the first
// token) exceeds StreamingReadTimeout. It is not retryable.
type StreamingReadTimeoutError struct {
	Timeout time.Duration
}

func (e *StreamingReadTimeoutError) Error() string {
	return fmt.Sprintf("stream stalled for %s (streaming read timeout)", e.Timeout)
}

// UpstreamStatusError reports a non-200 response from the upstream Kiro API. The
// body has already been read and the response closed.
type UpstreamStatusError struct {
	StatusCode int
	Body       string
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream API error (%d): %s", e.StatusCode, e.Body)
}

// StreamOptions tunes the streaming driver.
type StreamOptions struct {
	// EnableThinkingParser toggles fake-reasoning (thinking-tag) parsing for this
	// stream. It is ANDed with cfg.FakeReasoningEnabled. Callers disable it for
	// models that ignore the injected reasoning tags (bug #178).
	EnableThinkingParser bool
}

// StreamWithFirstTokenRetry starts an upstream request via start and returns a
// channel of unified KiroEvents plus a single-error channel.
//
// start must return a fresh *http.Response each time it is invoked, so the
// driver can transparently retry by re-issuing the request when the model fails
// to emit a first token within cfg.FirstTokenTimeout. Up to
// cfg.FirstTokenMaxRetries attempts are made. Once a first token is received the
// stream is committed; later stalls surface as StreamingReadTimeoutError without
// retry.
//
// The events channel is closed when the stream ends. After it closes, callers
// must read the error channel: a nil/absent value means success, otherwise it
// carries the terminal error (FirstTokenTimeoutError exhaustion,
// StreamingReadTimeoutError, UpstreamStatusError, or a transport error).
//
// Thinking-tag parsing is enabled (subject to cfg.FakeReasoningEnabled). Use
// StreamWithFirstTokenRetryOpts to control it.
func StreamWithFirstTokenRetry(ctx context.Context, cfg *config.Config, start func(context.Context) (*http.Response, error)) (<-chan KiroEvent, <-chan error, error) {
	return StreamWithFirstTokenRetryOpts(ctx, cfg, start, StreamOptions{EnableThinkingParser: true})
}

// StreamWithFirstTokenRetryOpts is StreamWithFirstTokenRetry with explicit
// options.
func StreamWithFirstTokenRetryOpts(ctx context.Context, cfg *config.Config, start func(context.Context) (*http.Response, error), opts StreamOptions) (<-chan KiroEvent, <-chan error, error) {
	if cfg == nil {
		return nil, nil, errors.New("streaming: cfg must not be nil")
	}
	if start == nil {
		return nil, nil, errors.New("streaming: start function must not be nil")
	}

	events := make(chan KiroEvent)
	errc := make(chan error, 1)

	firstTokenTimeout := seconds(cfg.FirstTokenTimeout, DefaultFirstTokenTimeout)
	maxRetries := cfg.FirstTokenMaxRetries
	if maxRetries < 1 {
		maxRetries = DefaultFirstTokenMaxRetries
	}

	go func() {
		defer close(events)
		defer close(errc)

		var lastErr error
		for attempt := 0; attempt < maxRetries; attempt++ {
			if err := ctx.Err(); err != nil {
				errc <- err
				return
			}
			if attempt > 0 {
				slog.Warn("streaming: retry after first token timeout",
					"attempt", attempt+1, "max", maxRetries)
			}

			resp, err := start(ctx)
			if err != nil {
				errc <- err
				return
			}

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				// Record the upstream error body verbatim: for a Kiro 400 this is
				// the only place the real reason appears.
				debuglog.FromContext(ctx).LogRawChunk(body)
				slog.ErrorContext(ctx, "streaming: upstream error",
					"status", resp.StatusCode, "body", string(body))
				errc <- &UpstreamStatusError{StatusCode: resp.StatusCode, Body: string(body)}
				return
			}

			err = parseKiroStream(ctx, cfg, resp, opts, events)
			if err == nil {
				return // completed successfully
			}

			var fte *FirstTokenTimeoutError
			if errors.As(err, &fte) {
				lastErr = err
				slog.Warn("streaming: first token timeout",
					"attempt", attempt+1, "max", maxRetries, "timeout", firstTokenTimeout)
				continue // retry with a fresh request
			}

			// Any other error is terminal.
			errc <- err
			return
		}

		slog.Error("streaming: all first-token attempts exhausted",
			"attempts", maxRetries, "timeout", firstTokenTimeout)
		errc <- fmt.Errorf("model did not respond within %s after %d attempts: %w",
			firstTokenTimeout, maxRetries, lastErr)
	}()

	return events, errc, nil
}

// parseKiroStream reads resp.Body in bounded chunks, drives the awsstream and
// thinking parsers, and emits unified events on out. It enforces the first-token
// gate (FirstTokenTimeout) and the per-read STREAMING_READ_TIMEOUT. The response
// body is always closed before returning.
func parseKiroStream(ctx context.Context, cfg *config.Config, resp *http.Response, opts StreamOptions, out chan<- KiroEvent) (retErr error) {
	defer func() { _ = resp.Body.Close() }()
	dbg := debuglog.FromContext(ctx)

	firstTokenTimeout := seconds(cfg.FirstTokenTimeout, DefaultFirstTokenTimeout)
	readTimeout := seconds(cfg.StreamingReadTimeout, DefaultStreamingReadTimeout)

	parser := awsstream.NewParser()
	parser.TruncationRecovery = cfg.TruncationRecovery

	var tp *thinking.Parser
	if opts.EnableThinkingParser {
		tp = thinking.NewFromConfig(cfg)
		if !tp.Enabled() {
			tp = nil
		}
	}

	// Reader goroutine: performs the blocking Read calls and forwards chunks so
	// the main loop can apply timeouts via select.
	type readChunk struct {
		data []byte
		err  error
	}
	chunks := make(chan readChunk, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(chunks)
		buf := make([]byte, readBufferSize)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				c := make([]byte, n)
				copy(c, buf[:n])
				select {
				case chunks <- readChunk{data: c}:
				case <-readerDone:
					return
				}
			}
			if err != nil {
				select {
				case chunks <- readChunk{err: err}:
				case <-readerDone:
				}
				return
			}
		}
	}()
	// Closing readerDone first (registered last) lets the reader exit; closing
	// the body (registered first) unblocks any in-progress Read.
	defer close(readerDone)

	firstReceived := false
	for {
		timeout := readTimeout
		if !firstReceived {
			timeout = firstTokenTimeout
		}
		timer := time.NewTimer(timeout)

		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()

		case <-timer.C:
			if !firstReceived {
				slog.Warn("streaming: first token timeout", "timeout", firstTokenTimeout)
				return &FirstTokenTimeoutError{Timeout: firstTokenTimeout}
			}
			return &StreamingReadTimeoutError{Timeout: readTimeout}

		case rc, ok := <-chunks:
			timer.Stop()
			if !ok {
				// Reader closed without a sentinel error; treat as EOF.
				return finalizeStream(ctx, parser, tp, out)
			}
			if rc.err != nil {
				if rc.err == io.EOF {
					// EOF before any byte is a normal empty response.
					return finalizeStream(ctx, parser, tp, out)
				}
				return rc.err
			}
			firstReceived = true
			dbg.LogRawChunk(rc.data)
			if err := processChunk(ctx, parser, tp, rc.data, out); err != nil {
				return err
			}
		}
	}
}

// processChunk feeds a raw chunk to the awsstream parser and emits the resulting
// unified events, routing content through the thinking parser when enabled.
func processChunk(ctx context.Context, parser *awsstream.Parser, tp *thinking.Parser, chunk []byte, out chan<- KiroEvent) error {
	for _, ev := range parser.Feed(chunk) {
		switch ev.Type {
		case awsstream.EventContent:
			content, _ := ev.Data.(string)
			if tp != nil {
				res := tp.Feed(content)
				if res.ThinkingContent != "" {
					if processed, ok := tp.ProcessForOutput(res.ThinkingContent, res.IsFirstThinkingChunk, res.IsLastThinkingChunk); ok && processed != "" {
						if err := send(ctx, out, KiroEvent{
							Type:                 EventThinking,
							ThinkingContent:      processed,
							IsFirstThinkingChunk: res.IsFirstThinkingChunk,
							IsLastThinkingChunk:  res.IsLastThinkingChunk,
						}); err != nil {
							return err
						}
					}
				}
				if res.RegularContent != "" {
					if err := send(ctx, out, KiroEvent{Type: EventContent, Content: res.RegularContent}); err != nil {
						return err
					}
				}
			} else {
				if err := send(ctx, out, KiroEvent{Type: EventContent, Content: content}); err != nil {
					return err
				}
			}

		case awsstream.EventUsage:
			if err := send(ctx, out, KiroEvent{Type: EventUsage, Usage: usageToMap(ev.Data)}); err != nil {
				return err
			}

		case awsstream.EventContextUsage:
			if err := send(ctx, out, KiroEvent{Type: EventContextUsage, ContextUsagePercentage: toFloat(ev.Data)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalizeStream flushes any trailing thinking content and emits the
// reconstructed tool calls once the stream has ended.
func finalizeStream(ctx context.Context, parser *awsstream.Parser, tp *thinking.Parser, out chan<- KiroEvent) error {
	if tp != nil {
		fin := tp.Finalize()
		if fin.ThinkingContent != "" {
			if processed, ok := tp.ProcessForOutput(fin.ThinkingContent, fin.IsFirstThinkingChunk, fin.IsLastThinkingChunk); ok && processed != "" {
				if err := send(ctx, out, KiroEvent{
					Type:                 EventThinking,
					ThinkingContent:      processed,
					IsFirstThinkingChunk: fin.IsFirstThinkingChunk,
					IsLastThinkingChunk:  fin.IsLastThinkingChunk,
				}); err != nil {
					return err
				}
			}
		}
		if fin.RegularContent != "" {
			if err := send(ctx, out, KiroEvent{Type: EventContent, Content: fin.RegularContent}); err != nil {
				return err
			}
		}
	}

	for _, tc := range parser.GetToolCalls() {
		tc := tc
		if err := send(ctx, out, KiroEvent{
			Type:     EventToolUse,
			ToolUse:  toolCallToMap(tc),
			ToolCall: &tc,
		}); err != nil {
			return err
		}
	}
	return nil
}

// CollectStreamResult consumes the entire event stream into a StreamResult. It
// ranges over events until the channel closes, then reads any terminal error
// from errc. Bracket-style tool calls embedded in the accumulated text are
// detected and merged (deduplicated) with the native tool calls.
//
// On a terminal error the partially accumulated result is still returned
// alongside the error.
func CollectStreamResult(events <-chan KiroEvent, errc <-chan error) (*StreamResult, error) {
	result := &StreamResult{}
	var full strings.Builder

	for ev := range events {
		switch ev.Type {
		case EventContent:
			result.Content += ev.Content
			full.WriteString(ev.Content)
		case EventThinking:
			result.ThinkingContent += ev.ThinkingContent
			full.WriteString(ev.ThinkingContent)
		case EventToolUse:
			if ev.ToolCall != nil {
				result.ToolCalls = append(result.ToolCalls, *ev.ToolCall)
			}
		case EventUsage:
			if ev.Usage != nil {
				result.Usage = ev.Usage
			}
		case EventContextUsage:
			result.ContextUsagePercentage = ev.ContextUsagePercentage
		}
	}

	var streamErr error
	if errc != nil {
		if err, ok := <-errc; ok {
			streamErr = err
		}
	}
	if streamErr != nil {
		return result, streamErr
	}

	if bracket := awsstream.ParseBracketToolCalls(full.String()); len(bracket) > 0 {
		result.ToolCalls = awsstream.DeduplicateToolCalls(append(result.ToolCalls, bracket...))
	}

	return result, nil
}

// send forwards an event, honouring context cancellation.
func send(ctx context.Context, out chan<- KiroEvent, ev KiroEvent) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// toolCallToMap renders a typed tool call in the OpenAI map shape carried by a
// KiroEvent tool_use.
func toolCallToMap(tc awsstream.ToolCall) map[string]any {
	return map[string]any{
		"id":   tc.ID,
		"type": tc.Type,
		"function": map[string]any{
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		},
	}
}

// usageToMap normalizes a raw usage value (object or scalar) into a map.
func usageToMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"usage": v}
}

// toFloat coerces a JSON-decoded numeric value to float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

// seconds converts a float-seconds config value to a Duration, falling back to
// def when the value is non-positive.
func seconds(v float64, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return time.Duration(v * float64(time.Second))
}
