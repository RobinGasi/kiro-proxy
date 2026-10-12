package transport

import (
	"net/http"
	"testing"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

func TestNewAppliesResponseHeaderTimeout(t *testing.T) {
	client, err := New(&config.Config{UpstreamResponseHeaderTimeout: 12.5})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if tr.ResponseHeaderTimeout != 12500*time.Millisecond {
		t.Fatalf("ResponseHeaderTimeout = %v, want 12.5s", tr.ResponseHeaderTimeout)
	}
}
