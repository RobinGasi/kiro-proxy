package upstream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/util"
)

type testAuth struct{}

func (testAuth) GetAccessToken(context.Context) (string, error) { return "token", nil }
func (testAuth) ForceRefresh(context.Context) (string, error)   { return "token", nil }
func (testAuth) ProfileARN() string                             { return "" }
func (testAuth) APIHost() string                                { return "https://example.invalid" }
func (testAuth) QHost() string                                  { return "https://example.invalid" }
func (testAuth) Fingerprint() string                            { return "fp" }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStreamingTransportConsumesSharedAttemptBudget(t *testing.T) {
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"message":"busy"}`)),
		}, nil
	})}
	client := New(hc, &config.Config{FirstTokenMaxRetries: 3, BaseRetryDelay: 0}, slog.New(slog.DiscardHandler))
	ctx := util.WithAttemptBudget(context.Background(), 2)

	resp, err := client.SendConversation(ctx, testAuth{}, []byte(`{}`), true)
	if err != nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %#v, error = %v; want final upstream 503", resp, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("physical requests = %d, want 2", got)
	}
}
