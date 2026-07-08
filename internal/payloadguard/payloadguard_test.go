package payloadguard

import (
	"encoding/json"
	"fmt"
	"testing"
)

// makePayload builds a conversationState payload with n history entries
// (alternating user/assistant), each padded so the whole payload is large.
func makePayload(n int) map[string]any {
	hist := make([]any, 0, n)
	for i := range n {
		pad := fmt.Sprintf("%0*d", 200, i) // 200-byte filler per entry
		if i%2 == 0 {
			hist = append(hist, map[string]any{
				"userInputMessage": map[string]any{"content": "u" + pad},
			})
		} else {
			hist = append(hist, map[string]any{
				"assistantResponseMessage": map[string]any{"content": "a" + pad},
			})
		}
	}
	return map[string]any{
		"conversationState": map[string]any{
			"history":        hist,
			"currentMessage": map[string]any{"userInputMessage": map[string]any{"content": "now"}},
		},
	}
}

func mustSize(t *testing.T, p map[string]any) int {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(b)
}

// The incremental-size trim must (a) bring the payload under the limit when
// achievable and (b) report a FinalBytes that equals the true serialized size —
// i.e. the O(payload) accounting matches an exact re-measure.
func TestTrimReducesAndReportsExactSize(t *testing.T) {
	p := makePayload(20)
	full := mustSize(t, p)
	limit := full / 2

	stats, err := TrimPayloadToLimit(p, limit)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if !stats.Trimmed {
		t.Fatalf("expected trimming for limit %d (full %d)", limit, full)
	}
	if got := mustSize(t, p); got != stats.FinalBytes {
		t.Errorf("FinalBytes=%d but actual serialized size=%d (incremental accounting drifted)", stats.FinalBytes, got)
	}
	if stats.FinalBytes > limit {
		t.Errorf("FinalBytes=%d still exceeds limit=%d", stats.FinalBytes, limit)
	}
	if stats.FinalEntries >= stats.OriginalEntries {
		t.Errorf("expected fewer entries: orig=%d final=%d", stats.OriginalEntries, stats.FinalEntries)
	}
}

// A payload already under the limit is left untouched by Guard.
func TestGuardNoopUnderLimit(t *testing.T) {
	p := makePayload(4)
	full := mustSize(t, p)
	stats, err := Guard(p, full+1000, true)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if stats.Trimmed {
		t.Errorf("did not expect trimming under the limit")
	}
}
