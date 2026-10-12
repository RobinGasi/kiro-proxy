package streaming

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
)

func TestStreamRetriesReasoningOnlyThenReturnsAnswer(t *testing.T) {
	attempts := 0
	cfg := &config.Config{FirstTokenMaxRetries: 2}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return framedResponse(
				eventStreamFrame("event", "reasoningContentEvent", map[string]any{"text": "private analysis"}),
				eventStreamFrame("event", "contextUsageEvent", map[string]any{"contextUsagePercentage": 41.9}),
			), nil
		}
		return framedResponse(
			eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "recovered"}),
			eventStreamFrame("event", "metadataEvent", map[string]any{"stopReason": "END_TURN"}),
		), nil
	}

	result, err := collectStarted(t, cfg, start)
	if err != nil {
		t.Fatalf("collect stream: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want recovery on second attempt", attempts)
	}
	if result.Content != "recovered" || result.ThinkingContent != "" {
		t.Fatalf("result = %+v, want only recovered answer; discarded reasoning must not leak", result)
	}
	if result.StopReason != "END_TURN" {
		t.Fatalf("stop reason = %q, want END_TURN", result.StopReason)
	}
}

func TestStreamRetriesMetadataOnlyThenReturnsAnswer(t *testing.T) {
	attempts := 0
	cfg := &config.Config{FirstTokenMaxRetries: 2}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return framedResponse(eventStreamFrame("event", "contextUsageEvent", map[string]any{"contextUsagePercentage": 41.9})), nil
		}
		return framedResponse(eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "ok"})), nil
	}

	result, err := collectStarted(t, cfg, start)
	if err != nil || result.Content != "ok" || attempts != 2 {
		t.Fatalf("result = %+v, error = %v, attempts = %d; want transparent recovery", result, err, attempts)
	}
}

func TestStreamDoesNotRetryStructuredRefusal(t *testing.T) {
	attempts := 0
	cfg := &config.Config{FirstTokenMaxRetries: 3}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		return framedResponse(eventStreamFrame("event", "metadataEvent", map[string]any{
			"stopReason":  "REFUSAL",
			"stopDetails": map[string]any{"type": "refusal", "category": "cyber"},
		})), nil
	}

	_, err := collectStarted(t, cfg, start)
	var streamErr *UpstreamStreamError
	if !errors.As(err, &streamErr) || streamErr.FailureClass != "content_filter" {
		t.Fatalf("error = %v, want content_filter UpstreamStreamError", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want no replay of a refusal", attempts)
	}
}

func TestStreamReturnsTerminalExceptionAfterCommittedAnswer(t *testing.T) {
	result, err := collectFrames(t,
		eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "partial"}),
		eventStreamFrame("exception", "ServiceUnavailableException", map[string]any{
			"message": "Opus capacity exhausted", "reason": "INSUFFICIENT_MODEL_CAPACITY",
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "Opus capacity exhausted") {
		t.Fatalf("error = %v, want terminal exception detail", err)
	}
	if result.Content != "partial" {
		t.Fatalf("partial content = %q, want partial", result.Content)
	}
}

func TestStreamAcceptsContentToolAndMetadata(t *testing.T) {
	result, err := collectFrames(t,
		eventStreamFrame("event", "reasoningContentEvent", map[string]any{"text": "analysis"}),
		eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "hello"}),
		eventStreamFrame("event", "toolUseEvent", map[string]any{
			"name": "Bash", "toolUseId": "tool-1", "input": map[string]any{"command": "pwd"}, "stop": true,
		}),
		eventStreamFrame("event", "metadataEvent", map[string]any{
			"stopReason": "TOOL_USE", "tokenUsage": map[string]any{"outputTokens": 42},
		}),
		eventStreamFrame("event", "contextUsageEvent", map[string]any{"contextUsagePercentage": 12.5}),
	)
	if err != nil {
		t.Fatalf("collect stream: %v", err)
	}
	if result.Content != "hello" || result.ThinkingContent != "analysis" || len(result.ToolCalls) != 1 {
		t.Fatalf("result = %+v, want reasoning, content, and one tool", result)
	}
	if result.ToolCalls[0].Function.Name != "Bash" || result.ContextUsagePercentage != 12.5 || result.StopReason != "TOOL_USE" {
		t.Fatalf("result metadata/tool = %+v", result)
	}
	if result.Usage["outputTokens"] != float64(42) {
		t.Fatalf("usage = %#v, want outputTokens=42", result.Usage)
	}
}

func TestStreamRejectsCorruptFrameAndRetries(t *testing.T) {
	attempts := 0
	corrupt := eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "bad"})
	corrupt[len(corrupt)-1] ^= 0xff
	cfg := &config.Config{FirstTokenMaxRetries: 2}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return framedResponse(corrupt), nil
		}
		return framedResponse(eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "good"})), nil
	}
	result, err := collectStarted(t, cfg, start)
	if err != nil || result.Content != "good" || attempts != 2 {
		t.Fatalf("result = %+v, error = %v, attempts = %d", result, err, attempts)
	}
}

func TestStreamRetriesReadErrorBeforeAnswer(t *testing.T) {
	attempts := 0
	cfg := &config.Config{FirstTokenMaxRetries: 2}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return responseEndingWithError(
				eventStreamFrame("event", "reasoningContentEvent", map[string]any{"text": "private"}),
				io.ErrUnexpectedEOF,
			), nil
		}
		return framedResponse(eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "recovered"})), nil
	}
	result, err := collectStarted(t, cfg, start)
	if err != nil || result.Content != "recovered" || attempts != 2 {
		t.Fatalf("result=%+v err=%v attempts=%d, want pre-answer replay", result, err, attempts)
	}
}

func TestStreamDoesNotRetryReadErrorAfterAnswer(t *testing.T) {
	attempts := 0
	cfg := &config.Config{FirstTokenMaxRetries: 2}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		return responseEndingWithError(
			eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": "partial"}),
			io.ErrUnexpectedEOF,
		), nil
	}
	result, err := collectStarted(t, cfg, start)
	if err == nil || result.Content != "partial" || attempts != 1 {
		t.Fatalf("result=%+v err=%v attempts=%d, want terminal committed failure", result, err, attempts)
	}
}

type errorAfterReader struct {
	data []byte
	err  error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *errorAfterReader) Close() error { return nil }

func responseEndingWithError(frame []byte, err error) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       &errorAfterReader{data: frame, err: err},
		Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
	}
}

func TestStreamRecoversAdvertisedTaggedToolCall(t *testing.T) {
	frames := []byte(strings.Join([]string{
		"Before ",
		`<tool_use>{"tool_name":"read_file","tool_input":{"path":"README.md"}}</tool_use>`,
	}, ""))
	start := func(context.Context) (*http.Response, error) {
		return framedResponse(eventStreamFrame("event", "assistantResponseEvent", map[string]any{"content": string(frames)})), nil
	}
	events, errc, err := StreamWithFirstTokenRetryOpts(context.Background(), &config.Config{FirstTokenMaxRetries: 1}, start, StreamOptions{
		EnableThinkingParser: true,
		AdvertisedTools:      []string{"read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := CollectStreamResult(events, errc)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Before " || len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Name != "read" || result.ToolCalls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("result = %+v", result)
	}
}

func TestTaggedToolBufferHandlesSplitFunctionCallOpener(t *testing.T) {
	buffer := newTaggedToolBuffer([]string{"read"})
	visible := buffer.Feed("before <funct") + buffer.Feed(`ion_call>{"name":"read","arguments":{"path":"x"}}</function_call>`)
	cleaned, calls := buffer.Finalize()
	if visible+cleaned != "before " || len(calls) != 1 || calls[0].Function.Name != "read" {
		t.Fatalf("visible=%q cleaned=%q calls=%#v", visible, cleaned, calls)
	}
}

func TestTaggedToolBufferDoesNotRecoverFencedExample(t *testing.T) {
	buffer := newTaggedToolBuffer([]string{"read"})
	visible := buffer.Feed("```json\n<tool_use>{\"name\":\"read\",\"input\":{}}</tool_use>\n```")
	cleaned, calls := buffer.Finalize()
	if visible+cleaned != "```json\n<tool_use>{\"name\":\"read\",\"input\":{}}</tool_use>\n```" || len(calls) != 0 {
		t.Fatalf("visible=%q cleaned=%q calls=%#v", visible, cleaned, calls)
	}
}

func collectFrames(t *testing.T, frames ...[]byte) (*StreamResult, error) {
	t.Helper()
	return collectStarted(t, &config.Config{FirstTokenMaxRetries: 1}, func(context.Context) (*http.Response, error) {
		return framedResponse(frames...), nil
	})
}

func collectStarted(t *testing.T, cfg *config.Config, start func(context.Context) (*http.Response, error)) (*StreamResult, error) {
	t.Helper()
	events, errc, err := StreamWithFirstTokenRetry(context.Background(), cfg, start)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	return CollectStreamResult(events, errc)
}

func framedResponse(frames ...[]byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(bytes.Join(frames, nil))),
		Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
	}
}
