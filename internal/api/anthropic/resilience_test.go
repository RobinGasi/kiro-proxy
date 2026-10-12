package anthropic

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/streaming"
)

func TestClassifyUpstreamErrorUsesAnthropicTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		errType string
	}{
		{"rate limit", &streaming.UpstreamStatusError{StatusCode: 429, Body: `{"message":"slow down","reason":"THROTTLING"}`}, 429, "rate_limit_error"},
		{"capacity", &streaming.UpstreamStreamError{FailureClass: "transient_upstream", Reason: "INSUFFICIENT_MODEL_CAPACITY"}, 529, "overloaded_error"},
		{"context", &streaming.UpstreamStatusError{StatusCode: 400, Body: `{"message":"too long","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`}, 400, "invalid_request_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errType, _, _ := classifyUpstreamError(tt.err)
			if status != tt.status || errType != tt.errType {
				t.Fatalf("status/type = %d/%q, want %d/%q", status, errType, tt.status, tt.errType)
			}
		})
	}
}

func TestStreamMessagesSendsPingWhileWaitingForAnswer(t *testing.T) {
	h := &Handler{}
	evCh := make(chan streaming.KiroEvent)
	errCh := make(chan error)
	w := newFlushRecorder()

	done := make(chan error, 1)
	go func() {
		_, err := h.streamMessagesWithPing(t.Context(), w, nil, "claude-opus-5.5", 1, 1_000_000, true, nil, nil, evCh, errCh, time.Millisecond)
		done <- err
	}()

	deadline := time.After(time.Second)
	for !strings.Contains(w.String(), "event: ping") {
		select {
		case <-deadline:
			t.Fatalf("stream did not emit ping: %q", w.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	evCh <- streaming.KiroEvent{Type: streaming.EventContent, Content: "ok"}
	close(evCh)
	close(errCh)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStreamMessagesStopsAcrossContentChunks(t *testing.T) {
	h := &Handler{}
	evCh := make(chan streaming.KiroEvent, 3)
	errCh := make(chan error)
	evCh <- streaming.KiroEvent{Type: streaming.EventContent, Content: "visible<ST"}
	evCh <- streaming.KiroEvent{Type: streaming.EventContent, Content: "OP>hidden"}
	evCh <- streaming.KiroEvent{Type: streaming.EventMetadata, StopReason: "END_TURN"}
	close(evCh)
	close(errCh)
	w := newFlushRecorder()

	_, err := h.streamMessagesWithPing(t.Context(), w, nil, "claude-opus-5.5", 1, 1_000_000, true, nil, []string{"<STOP>"}, evCh, errCh, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := w.String()
	if !strings.Contains(got, `"text":"visib"`) || !strings.Contains(got, `"text":"le"`) || strings.Contains(got, "hidden") {
		t.Fatalf("unexpected streamed content: %q", got)
	}
	if !strings.Contains(got, `"stop_reason":"stop_sequence"`) || !strings.Contains(got, `"stop_sequence":"<STOP>"`) {
		t.Fatalf("missing stop-sequence metadata: %s", got)
	}
}

func TestStopMatcherPreservesVisibleTextAcrossChunks(t *testing.T) {
	m := newStopMatcher([]string{"<STOP>"})
	first, matched := m.Feed("visible<ST")
	second, matchedSecond := m.Feed("OP>hidden")
	if first+second != "visible" || matched != "" || matchedSecond != "<STOP>" {
		t.Fatalf("first=%q second=%q matches=%q/%q", first, second, matched, matchedSecond)
	}
}

type flushRecorder struct {
	mu sync.Mutex
	bytes.Buffer
	header http.Header
}

func newFlushRecorder() *flushRecorder       { return &flushRecorder{header: make(http.Header)} }
func (w *flushRecorder) Header() http.Header { return w.header }
func (w *flushRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Buffer.Write(p)
}
func (w *flushRecorder) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Buffer.String()
}
func (w *flushRecorder) WriteHeader(int) {}
func (w *flushRecorder) Flush()          {}

var _ http.ResponseWriter = (*flushRecorder)(nil)
var _ http.Flusher = (*flushRecorder)(nil)
