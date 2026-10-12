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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/awsstream"
	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/debuglog"
	"github.com/RobinGasi/kiro-proxy/internal/thinking"
	"github.com/RobinGasi/kiro-proxy/internal/tokenizer"
	"github.com/RobinGasi/kiro-proxy/internal/util"
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
	EventMetadata     = "metadata"
)

// readBufferSize is the chunk size used for bounded reads of the response body.
const readBufferSize = 64 * 1024

// KiroEvent is a unified event from the Kiro API stream. It is API-agnostic and
// can be converted to both OpenAI and Anthropic formats by the API layer.
//
// Only the fields relevant to Type are populated:
//
//   - EventThinking:     ThinkingContent plus opaque signature/redaction metadata
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
	// ThinkingSignature is opaque provider metadata for replay. It is never
	// interpreted or promoted to ordinary assistant text.
	ThinkingSignature string
	// RedactedThinkingData is opaque encrypted reasoning returned when the
	// provider redacts a thinking block.
	RedactedThinkingData string
	// NativeThinking distinguishes Kiro reasoningContentEvent data from the
	// legacy XML-tag emulation path.
	NativeThinking bool
	// ToolUse is the reconstructed tool call in OpenAI map shape
	// ({"id","type","function":{"name","arguments"}}), for EventToolUse.
	ToolUse map[string]any
	// ToolCall is the typed form of the same tool call, carried alongside ToolUse
	// so collectors can accumulate awsstream.ToolCall values without re-parsing.
	ToolCall *awsstream.ToolCall
	// Usage is the metering/token payload for EventUsage.
	Usage map[string]any
	// ContextUsagePercentage is the Kiro context-usage percentage for
	// EventContextUsage.
	ContextUsagePercentage float64
	// StopReason and StopDetails carry Kiro's terminal metadata verdict.
	StopReason  string
	StopDetails map[string]any
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
	// ThinkingSignature and RedactedThinkingData preserve opaque native Kiro
	// reasoning metadata for Anthropic-compatible clients and history replay.
	ThinkingSignature    string
	RedactedThinkingData string
	// ToolCalls is the deduplicated set of tool calls (native + text-encoded).
	ToolCalls []awsstream.ToolCall
	// Usage is the last usage payload seen, if any.
	Usage map[string]any
	// ContextUsagePercentage is the context-usage percentage reported by Kiro.
	ContextUsagePercentage float64
	// StopReason and StopDetails preserve Kiro's terminal metadata.
	StopReason  string
	StopDetails map[string]any
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

// UpstreamStreamError reports an HTTP 200 response that ended with an embedded
// upstream exception or without any client-visible output (content, thinking or
// a tool call). Kiro can encode model/capacity failures in the event stream
// instead of the HTTP status, and can end a stream with only usage metadata.
type UpstreamStreamError struct {
	Message      string
	Reason       string
	FailureClass string
	StopReason   string
	StopDetails  map[string]any
	// BeforeFirstEvent is true when no answer event was emitted, making the
	// request safe to retry without duplicating client-visible output.
	BeforeFirstEvent  bool
	BytesRead         int64
	ChunksRead        int
	RecognizedEvents  int
	UnmatchedBytes    int
	Termination       string
	Duration          time.Duration
	ContentType       string
	ContentLength     int64
	UpstreamRequestID string
}

func (e *UpstreamStreamError) Error() string {
	if e.Message != "" && e.Reason != "" {
		return fmt.Sprintf("%s (reason: %s)", e.Message, e.Reason)
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Reason != "" {
		return e.Reason
	}
	switch e.FailureClass {
	case "reasoning_only":
		return "Kiro produced reasoning but no final answer"
	case "metadata_only":
		return "Kiro completed without a final answer"
	}
	return "Kiro stream ended without assistant output"
}

// IsEmptyResponse reports the zero-byte HTTP 200 response Kiro emits when it
// closes a request before producing any event.
func IsEmptyResponse(err error) bool {
	var streamErr *UpstreamStreamError
	return errors.As(err, &streamErr) &&
		streamErr.BeforeFirstEvent &&
		streamErr.BytesRead == 0
}

// StreamOptions tunes the streaming driver.
type StreamOptions struct {
	// EnableThinkingParser toggles fake-reasoning (thinking-tag) parsing for this
	// stream. It is ANDed with cfg.FakeReasoningEnabled. Callers disable it for
	// models that ignore the injected reasoning tags (bug #178).
	EnableThinkingParser bool
	// AdvertisedTools enables validated text-emitted tool recovery. Names are the
	// exact names sent to Kiro.
	AdvertisedTools []string
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
// carries the terminal error (first-event retry exhaustion,
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
	ctx = util.WithAttemptBudget(ctx, maxRetries)

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
				slog.WarnContext(ctx, "streaming: retry before first event",
					"req_id", util.RequestID(ctx), "attempt", attempt+1, "max", maxRetries,
					"previous_error_type", fmt.Sprintf("%T", lastErr))
			}

			resp, err := start(ctx)
			if err != nil {
				errc <- err
				return
			}

			if resp.StatusCode != http.StatusOK {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				// The body is retained in DEBUG_MODE artifacts and returned to the
				// client, but never printed in normal logs because it may contain data.
				debuglog.FromContext(ctx).LogRawChunk(body)
				slog.ErrorContext(ctx, "streaming: upstream HTTP error",
					"req_id", util.RequestID(ctx), "attempt", attempt+1, "max", maxRetries,
					"status", resp.StatusCode, "body_bytes", len(body), "body_read_error", readErr,
					"content_type", resp.Header.Get("Content-Type"),
					"content_length", resp.ContentLength,
					"upstream_request_id", util.UpstreamRequestID(resp.Header))
				errc <- &UpstreamStatusError{StatusCode: resp.StatusCode, Body: string(body)}
				return
			}

			err = parseKiroStream(ctx, cfg, resp, opts, events)
			if err == nil {
				return // completed successfully
			}

			var fte *FirstTokenTimeoutError
			var use *UpstreamStreamError
			isFirstTokenTimeout := errors.As(err, &fte)
			isBeforeAnswer := errors.As(err, &use) && use.BeforeFirstEvent
			if isBeforeAnswer && nonRetryableStreamFailure(use.FailureClass) {
				slog.WarnContext(ctx, "streaming: non-retryable upstream stream failure",
					"req_id", util.RequestID(ctx), "attempt", attempt+1,
					"failure_class", use.FailureClass, "stop_reason", use.StopReason,
					"upstream_request_id", use.UpstreamRequestID)
				errc <- err
				return
			}
			if (isFirstTokenTimeout || isBeforeAnswer) && util.AttemptsRemaining(ctx) {
				lastErr = err
				attrs := []any{
					"req_id", util.RequestID(ctx), "attempt", attempt + 1, "max", maxRetries,
					"error_type", fmt.Sprintf("%T", err),
				}
				if use != nil {
					attrs = append(attrs,
						"failure_class", use.FailureClass, "termination", use.Termination,
						"bytes_read", use.BytesRead, "chunks_read", use.ChunksRead,
						"recognized_events", use.RecognizedEvents,
						"unmatched_bytes", use.UnmatchedBytes, "reason", use.Reason,
						"stop_reason", use.StopReason, "has_upstream_message", use.Message != "",
						"duration_ms", use.Duration.Milliseconds(),
						"content_type", use.ContentType, "content_length", use.ContentLength,
						"upstream_request_id", use.UpstreamRequestID)
				}
				slog.WarnContext(ctx, "streaming: attempt ended before final answer", attrs...)
				if attempt+1 < maxRetries && util.AttemptsRemaining(ctx) {
					delay := time.Second << attempt
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						errc <- ctx.Err()
						return
					case <-timer.C:
					}
					continue
				}
				errc <- err
				return
			}

			// Any error after answer output is terminal: replay would duplicate text
			// or tool side effects.
			errc <- err
			return
		}

		slog.ErrorContext(ctx, "streaming: all first-event attempts exhausted",
			"req_id", util.RequestID(ctx), "attempts", maxRetries,
			"last_error_type", fmt.Sprintf("%T", lastErr))
		var fte *FirstTokenTimeoutError
		if errors.As(lastErr, &fte) {
			errc <- fmt.Errorf("model did not respond within %s after %d attempts: %w",
				firstTokenTimeout, maxRetries, lastErr)
			return
		}
		errc <- fmt.Errorf("upstream stream failed before its first event after %d attempts: %w",
			maxRetries, lastErr)
	}()

	return events, errc, nil
}

type streamAttemptStats struct {
	started         time.Time
	bytesRead       int64
	chunksRead      int
	contentEvents   int
	reasoningEvents int
	usageEvents     int
	contextEvents   int
	toolEvents      int
	answerEvents    int
	pendingEvents   []KiroEvent
	stopReason      string
	stopDetails     map[string]any
	errorMessage    string
	errorReason     string
	eventTypes      eventTypeCounter
	textTools       *taggedToolBuffer
}

func (s *streamAttemptStats) recognizedEvents() int {
	return s.contentEvents + s.reasoningEvents + s.usageEvents + s.contextEvents + s.toolEvents
}

// emitDeferred preserves wire order while keeping an answerless attempt private
// from the client. Once text or a tool call commits the turn, buffered reasoning
// and metadata are released before the answer event that followed them.
func (s *streamAttemptStats) emitDeferred(ctx context.Context, out chan<- KiroEvent, ev KiroEvent, answer bool) error {
	if !answer && s.answerEvents == 0 {
		s.pendingEvents = append(s.pendingEvents, ev)
		return nil
	}
	if answer && s.answerEvents == 0 {
		for _, pending := range s.pendingEvents {
			if err := send(ctx, out, pending); err != nil {
				return err
			}
		}
		s.pendingEvents = nil
	}
	if err := send(ctx, out, ev); err != nil {
		return err
	}
	if answer {
		s.answerEvents++
	}
	return nil
}

func streamLogAttrs(ctx context.Context, resp *http.Response, stats *streamAttemptStats, parser *awsstream.Parser, termination string) []any {
	return []any{
		"req_id", util.RequestID(ctx), "termination", termination,
		"duration_ms", time.Since(stats.started).Milliseconds(),
		"bytes_read", stats.bytesRead, "chunks_read", stats.chunksRead,
		"recognized_events", stats.recognizedEvents(), "answer_events", stats.answerEvents,
		"content_events", stats.contentEvents, "reasoning_events", stats.reasoningEvents,
		"usage_events", stats.usageEvents, "context_events", stats.contextEvents,
		"tool_events", stats.toolEvents, "unmatched_bytes", len(parser.BufferedText()),
		"event_types", stats.eventTypes.String(), "stop_reason", stats.stopReason,
		"status", resp.StatusCode, "content_type", resp.Header.Get("Content-Type"),
		"content_length", resp.ContentLength,
		"upstream_request_id", util.UpstreamRequestID(resp.Header),
	}
}

func streamFailureClass(stats *streamAttemptStats, parser *awsstream.Parser) string {
	stop := strings.ToUpper(strings.TrimSpace(stats.stopReason))
	if stop == "REFUSAL" || stop == "CONTENT_FILTERED" || stop == "GUARDRAIL_INTERVENED" {
		return "content_filter"
	}
	reason := strings.ToUpper(stats.errorReason)
	if strings.Contains(reason, "THROTTL") || strings.Contains(reason, "SERVICE_UNAVAILABLE") ||
		strings.Contains(reason, "INTERNAL_SERVER") || strings.Contains(reason, "CAPACITY") ||
		strings.Contains(reason, "TRANSPORT") {
		return "transient_upstream"
	}
	if stats.errorReason == "TRUNCATED_EVENT_STREAM" {
		return "truncated_stream"
	}
	if stats.errorMessage != "" || stats.errorReason != "" {
		return "upstream_exception"
	}
	if stop == "MAX_TOKENS" {
		return "max_tokens"
	}
	if stats.bytesRead == 0 {
		return "empty_response"
	}
	if stats.reasoningEvents > 0 && stats.answerEvents == 0 {
		return "reasoning_only"
	}
	if stats.recognizedEvents() == 0 && len(parser.BufferedText()) > 0 {
		return "unrecognized_stream"
	}
	if stats.answerEvents == 0 {
		return "metadata_only"
	}
	return "terminal_exception"
}

func nonRetryableStreamFailure(class string) bool {
	return class == "content_filter" || class == "upstream_exception"
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
	stats := &streamAttemptStats{started: time.Now(), textTools: newTaggedToolBuffer(opts.AdvertisedTools)}

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
	finish := func(termination string) error {
		if err := parser.FlushStream(); err != nil {
			stats.errorReason = "TRUNCATED_EVENT_STREAM"
			stats.errorMessage = err.Error()
		}
		if err := finalizeStream(ctx, parser, tp, out, stats); err != nil {
			return err
		}
		failureClass := streamFailureClass(stats, parser)
		if stats.answerEvents == 0 || stats.errorMessage != "" || stats.errorReason != "" {
			attrs := streamLogAttrs(ctx, resp, stats, parser, termination)
			attrs = append(attrs, "failure_class", failureClass,
				"reason", stats.errorReason, "has_upstream_message", stats.errorMessage != "")
			slog.WarnContext(ctx, "streaming: upstream stream ended unsuccessfully", attrs...)
			return &UpstreamStreamError{
				Message: stats.errorMessage, Reason: stats.errorReason,
				FailureClass: failureClass, StopReason: stats.stopReason, StopDetails: stats.stopDetails,
				BeforeFirstEvent: stats.answerEvents == 0,
				BytesRead:        stats.bytesRead, ChunksRead: stats.chunksRead,
				RecognizedEvents: stats.recognizedEvents(), UnmatchedBytes: len(parser.BufferedText()),
				Termination: termination, Duration: time.Since(stats.started),
				ContentType: resp.Header.Get("Content-Type"), ContentLength: resp.ContentLength,
				UpstreamRequestID: util.UpstreamRequestID(resp.Header),
			}
		}
		slog.DebugContext(ctx, "streaming: upstream stream completed",
			streamLogAttrs(ctx, resp, stats, parser, termination)...)
		return nil
	}
	for {
		timeout := readTimeout
		if !firstReceived {
			timeout = firstTokenTimeout
		}
		timer := time.NewTimer(timeout)

		select {
		case <-ctx.Done():
			timer.Stop()
			slog.WarnContext(ctx, "streaming: request context ended",
				append(streamLogAttrs(ctx, resp, stats, parser, "context_done"), "err", ctx.Err())...)
			return ctx.Err()

		case <-timer.C:
			termination := "read_timeout"
			if !firstReceived {
				termination = "first_event_timeout"
			}
			slog.WarnContext(ctx, "streaming: upstream stream timed out",
				append(streamLogAttrs(ctx, resp, stats, parser, termination), "timeout", timeout)...)
			if !firstReceived {
				return &FirstTokenTimeoutError{Timeout: firstTokenTimeout}
			}
			return &StreamingReadTimeoutError{Timeout: readTimeout}

		case rc, ok := <-chunks:
			timer.Stop()
			if !ok {
				return finish("reader_closed")
			}
			if rc.err != nil {
				if rc.err == io.EOF {
					return finish("eof")
				}
				slog.WarnContext(ctx, "streaming: upstream body read failed",
					append(streamLogAttrs(ctx, resp, stats, parser, "read_error"),
						"error_type", fmt.Sprintf("%T", rc.err), "err", rc.err)...)
				stats.errorReason = "UPSTREAM_TRANSPORT_ERROR"
				stats.errorMessage = rc.err.Error()
				return finish("read_error")
			}
			firstReceived = true
			stats.bytesRead += int64(len(rc.data))
			stats.chunksRead++
			stats.eventTypes.feed(rc.data)
			dbg.LogRawChunk(rc.data)
			if err := processChunk(ctx, parser, tp, rc.data, out, stats); err != nil {
				stats.errorReason = "EVENT_STREAM_PROTOCOL_ERROR"
				stats.errorMessage = err.Error()
				return finish("parse_error")
			}
		}
	}
}

// processChunk feeds a raw chunk to the awsstream parser and emits the resulting
// unified events, routing content through the thinking parser when enabled.
func processChunk(ctx context.Context, parser *awsstream.Parser, tp *thinking.Parser, chunk []byte, out chan<- KiroEvent, stats *streamAttemptStats) error {
	events, err := parser.FeedStream(chunk)
	if err != nil {
		return err
	}
	for _, ev := range events {
		switch ev.Type {
		case awsstream.EventContent:
			content, _ := ev.Data.(string)
			if content == "" {
				continue
			}
			stats.contentEvents++
			if tp == nil {
				content = stats.textTools.Feed(content)
				if content != "" {
					if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventContent, Content: content}, true); err != nil {
						return err
					}
				}
				continue
			}
			res := tp.Feed(content)
			if res.ThinkingContent != "" {
				if processed, ok := tp.ProcessForOutput(res.ThinkingContent, res.IsFirstThinkingChunk, res.IsLastThinkingChunk); ok && processed != "" {
					stats.reasoningEvents++
					if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventThinking, ThinkingContent: processed,
						IsFirstThinkingChunk: res.IsFirstThinkingChunk, IsLastThinkingChunk: res.IsLastThinkingChunk}, false); err != nil {
						return err
					}
				}
			}
			if res.RegularContent != "" {
				regular := stats.textTools.Feed(res.RegularContent)
				if regular != "" {
					if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventContent, Content: regular}, true); err != nil {
						return err
					}
				}
			}

		case awsstream.EventReasoning:
			data, _ := ev.Data.(map[string]any)
			text, _ := data["text"].(string)
			signature, _ := data["signature"].(string)
			redacted, _ := data["redactedContent"].(string)
			if text != "" || signature != "" || redacted != "" {
				stats.reasoningEvents++
				if err := stats.emitDeferred(ctx, out, KiroEvent{
					Type: EventThinking, ThinkingContent: text,
					ThinkingSignature: signature, RedactedThinkingData: redacted,
					NativeThinking: true,
				}, false); err != nil {
					return err
				}
			}

		case awsstream.EventUsage:
			stats.usageEvents++
			if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventUsage, Usage: usageToMap(ev.Data)}, false); err != nil {
				return err
			}

		case awsstream.EventContextUsage:
			stats.contextEvents++
			if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventContextUsage, ContextUsagePercentage: toFloat(ev.Data)}, false); err != nil {
				return err
			}

		case awsstream.EventMetadata:
			data, _ := ev.Data.(map[string]any)
			stats.stopReason = firstString(data, "stopReason")
			if details, ok := data["stopDetails"].(map[string]any); ok {
				stats.stopDetails = details
			}
			if tokenUsage, ok := data["tokenUsage"].(map[string]any); ok {
				stats.usageEvents++
				if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventUsage, Usage: tokenUsage}, false); err != nil {
					return err
				}
			}
			if stats.stopReason != "" || stats.stopDetails != nil {
				if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventMetadata,
					StopReason: stats.stopReason, StopDetails: stats.stopDetails}, false); err != nil {
					return err
				}
			}

		case awsstream.EventError:
			data, _ := ev.Data.(map[string]any)
			stats.errorMessage = firstString(data, "message", "Message", "originalMessage")
			stats.errorReason = firstString(data, "reason", "code", "errorCode", "__type")
			if stats.errorReason == "" {
				stats.errorReason = ev.UpstreamType
			}
		}
	}
	return nil
}

// finalizeStream flushes trailing tag-based reasoning and reconstructs tool calls.
func finalizeStream(ctx context.Context, parser *awsstream.Parser, tp *thinking.Parser, out chan<- KiroEvent, stats *streamAttemptStats) error {
	if tp != nil {
		fin := tp.Finalize()
		if fin.ThinkingContent != "" {
			if processed, ok := tp.ProcessForOutput(fin.ThinkingContent, fin.IsFirstThinkingChunk, fin.IsLastThinkingChunk); ok && processed != "" {
				stats.reasoningEvents++
				if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventThinking, ThinkingContent: processed,
					IsFirstThinkingChunk: fin.IsFirstThinkingChunk, IsLastThinkingChunk: fin.IsLastThinkingChunk}, false); err != nil {
					return err
				}
			}
		}
		if fin.RegularContent != "" {
			stats.contentEvents++
			regular := stats.textTools.Feed(fin.RegularContent)
			if regular != "" {
				if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventContent, Content: regular}, true); err != nil {
					return err
				}
			}
		}
	}

	if stats.textTools != nil {
		cleaned, recovered := stats.textTools.Finalize()
		if cleaned != "" {
			stats.contentEvents++
			if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventContent, Content: cleaned}, true); err != nil {
				return err
			}
		}
		if len(parser.GetToolCalls()) == 0 {
			for _, tc := range recovered {
				tc := tc
				stats.toolEvents++
				if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventToolUse, ToolUse: toolCallToMap(tc), ToolCall: &tc}, true); err != nil {
					return err
				}
			}
		}
	}

	for _, tc := range parser.GetToolCalls() {
		tc := tc
		stats.toolEvents++
		if err := stats.emitDeferred(ctx, out, KiroEvent{Type: EventToolUse,
			ToolUse: toolCallToMap(tc), ToolCall: &tc}, true); err != nil {
			return err
		}
	}
	return nil
}

// extractStreamError recovers the useful fields from an exception JSON object
// embedded in the otherwise binary AWS event-stream envelope.
func extractStreamError(raw string) (message, reason string) {
	for offset := 0; offset < len(raw); {
		rel := strings.IndexByte(raw[offset:], '{')
		if rel < 0 {
			break
		}
		start := offset + rel
		end := awsstream.FindMatchingBrace(raw, start)
		if end < 0 {
			offset = start + 1
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(raw[start:end+1]), &payload) == nil {
			message = firstString(payload, "message", "originalMessage")
			reason = firstString(payload, "reason", "code", "errorCode")
			if message != "" || reason != "" {
				return message, reason
			}
		}
		offset = end + 1
	}
	return "", ""
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
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
			if ev.ThinkingSignature != "" {
				result.ThinkingSignature = ev.ThinkingSignature
			}
			if ev.RedactedThinkingData != "" {
				result.RedactedThinkingData = ev.RedactedThinkingData
			}
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
		case EventMetadata:
			result.StopReason = ev.StopReason
			result.StopDetails = ev.StopDetails
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
