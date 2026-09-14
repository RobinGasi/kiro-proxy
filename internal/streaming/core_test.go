package streaming

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
)

func TestStreamRejectsResponseWithoutRecognizedEvents(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantDetail string
	}{
		{
			name:       "upstream exception frame",
			body:       `binary-header-bytes{"message":"Model is temporarily unavailable","reason":"INSUFFICIENT_MODEL_CAPACITY"}`,
			wantDetail: "Model is temporarily unavailable",
		},
		{
			name:       "empty response",
			body:       "",
			wantDetail: "without any recognized events",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{FirstTokenMaxRetries: 1}
			start := func(context.Context) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(tt.body)),
				}, nil
			}

			events, errc, err := StreamWithFirstTokenRetry(context.Background(), cfg, start)
			if err != nil {
				t.Fatalf("start stream: %v", err)
			}
			_, err = CollectStreamResult(events, errc)
			if err == nil {
				t.Fatal("stream succeeded without a recognized event")
			}
			if !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("error = %q, want detail %q", err, tt.wantDetail)
			}
		})
	}
}

func TestStreamRetriesImmediateEOFBeforeFirstEvent(t *testing.T) {
	attempts := 0
	cfg := &config.Config{FirstTokenMaxRetries: 2}
	start := func(context.Context) (*http.Response, error) {
		attempts++
		body := ""
		if attempts == 2 {
			body = `{"content":"recovered"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}

	events, errc, err := StreamWithFirstTokenRetry(context.Background(), cfg, start)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	result, err := CollectStreamResult(events, errc)
	if err != nil {
		t.Fatalf("collect retried stream: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if result.Content != "recovered" {
		t.Fatalf("content = %q, want %q", result.Content, "recovered")
	}
}

func TestStreamAcceptsRecognizedResponses(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantText string
		wantTool string
	}{
		{
			name:     "content",
			body:     `{"content":"hello"}`,
			wantText: "hello",
		},
		{
			name:     "tool only",
			body:     `{"name":"Bash","toolUseId":"tool-1","input":{"command":"pwd"},"stop":true}`,
			wantTool: "Bash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := collectBody(t, tt.body)
			if err != nil {
				t.Fatalf("collect stream: %v", err)
			}
			if result.Content != tt.wantText {
				t.Errorf("content = %q, want %q", result.Content, tt.wantText)
			}
			if tt.wantTool != "" {
				if len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Name != tt.wantTool {
					t.Errorf("tool calls = %+v, want one %q call", result.ToolCalls, tt.wantTool)
				}
			}
		})
	}
}

func TestStreamReturnsTerminalExceptionAfterPartialContent(t *testing.T) {
	result, err := collectBody(t, `{"content":"partial"}{"message":"Opus capacity exhausted","reason":"INSUFFICIENT_MODEL_CAPACITY"}`)
	if err == nil || !strings.Contains(err.Error(), "Opus capacity exhausted") {
		t.Fatalf("error = %v, want terminal exception detail", err)
	}
	if result.Content != "partial" {
		t.Fatalf("partial content = %q, want %q", result.Content, "partial")
	}
}

func collectBody(t *testing.T, body string) (*StreamResult, error) {
	t.Helper()
	cfg := &config.Config{FirstTokenMaxRetries: 1}
	start := func(context.Context) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	events, errc, err := StreamWithFirstTokenRetry(context.Background(), cfg, start)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	return CollectStreamResult(events, errc)
}
