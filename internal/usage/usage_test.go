package usage

import (
	"context"
	"encoding/json"
	"testing"
)

// realResponse is the shape Kiro's own IDE receives from GetUsageLimitsCommand
// (captured in q-client.log): a usageBreakdownList carrying currentUsage /
// usageLimit per resourceType.
const realResponse = `{
  "usageBreakdownList": [
    {
      "resourceType": "AGENTIC_REQUEST",
      "currentUsage": 1915,
      "usageLimit": 10000,
      "overageCharges": 0
    }
  ]
}`

func TestExtractLimits_RealBreakdownShape(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(realResponse), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lim, ok := extractLimits(m)
	if !ok {
		t.Fatal("expected credit fields to be recognized")
	}
	if lim.TotalCredits != 10000 {
		t.Errorf("total: got %v, want 10000", lim.TotalCredits)
	}
	if lim.UsedCredits != 1915 {
		t.Errorf("used: got %v, want 1915", lim.UsedCredits)
	}
	if lim.RemainingCredits != 8085 {
		t.Errorf("remaining: got %v, want 8085", lim.RemainingCredits)
	}
}

// The AGENTIC_REQUEST pool must be selected even when other resourceTypes are
// listed first.
func TestExtractLimits_PrefersAgenticPool(t *testing.T) {
	const multi = `{
      "usageBreakdownList": [
        {"resourceType": "SOMETHING_ELSE", "currentUsage": 5, "usageLimit": 50},
        {"resourceType": "AGENTIC_REQUEST", "currentUsage": 1915, "usageLimit": 10000}
      ]
    }`
	var m map[string]any
	if err := json.Unmarshal([]byte(multi), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lim, ok := extractLimits(m)
	if !ok {
		t.Fatal("expected credit fields to be recognized")
	}
	if lim.TotalCredits != 10000 || lim.UsedCredits != 1915 {
		t.Errorf("wrong pool selected: got %v/%v, want 1915/10000", lim.UsedCredits, lim.TotalCredits)
	}
}

// The generic key-based fallback must still work for older/flat shapes.
func TestExtractLimits_FlatFallback(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(`{"usageLimit": 10000, "currentUsage": 1915}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lim, ok := extractLimits(m)
	if !ok {
		t.Fatal("expected credit fields to be recognized")
	}
	if lim.TotalCredits != 10000 || lim.UsedCredits != 1915 || lim.RemainingCredits != 8085 {
		t.Errorf("got %v/%v (rem %v), want 1915/10000 (rem 8085)", lim.UsedCredits, lim.TotalCredits, lim.RemainingCredits)
	}
}

func TestUsageHost_DerivesRegionFromARN(t *testing.T) {
	got := usageHost(stubAuth{
		arn:   "arn:aws:codewhisperer:eu-central-1:123456789012:profile/EXAMPLEPROFILE",
		qhost: "https://runtime.us-east-1.kiro.dev",
	})
	want := "https://q.eu-central-1.amazonaws.com"
	if got != want {
		t.Errorf("host: got %q, want %q", got, want)
	}
}

func TestUsageHost_FallsBackToUsEast1(t *testing.T) {
	got := usageHost(stubAuth{arn: "", qhost: "https://runtime.us-east-1.kiro.dev/"})
	want := "https://q.us-east-1.amazonaws.com"
	if got != want {
		t.Errorf("host: got %q, want %q", got, want)
	}
}

type stubAuth struct {
	arn   string
	qhost string
}

func (s stubAuth) QHost() string                                  { return s.qhost }
func (s stubAuth) GetAccessToken(context.Context) (string, error) { return "", nil }
func (s stubAuth) Fingerprint() string                            { return "" }
func (s stubAuth) ProfileARN() string                             { return s.arn }
