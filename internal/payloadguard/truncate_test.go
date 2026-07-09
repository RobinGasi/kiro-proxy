package payloadguard

import (
	"strings"
	"testing"
)

func hugeToolResultPayload(toolText string) map[string]any {
	return map[string]any{
		"conversationState": map[string]any{
			"history": []any{
				map[string]any{
					"assistantResponseMessage": map[string]any{"content": "called tool"},
				},
				map[string]any{
					"userInputMessage": map[string]any{
						"content": "result",
						"userInputMessageContext": map[string]any{
							"toolResults": []any{
								map[string]any{
									"toolUseId": "toolu_1",
									"content":   []any{map[string]any{"text": toolText}},
								},
							},
						},
					},
				},
			},
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{"content": "continue"},
			},
		},
	}
}

func TestEnforceLimitTruncatesOversizedToolResult(t *testing.T) {
	p := hugeToolResultPayload(strings.Repeat("x", 800_000))
	limit := 600_000

	stats, err := enforceLimit(p, limit)
	if err != nil {
		t.Fatalf("enforceLimit: %v", err)
	}
	if !stats.ContentTruncated {
		t.Fatalf("expected content truncation, stats=%+v", stats)
	}
	if got := mustSize(t, p); got > limit {
		t.Fatalf("payload still %d bytes, limit %d", got, limit)
	}
}

func TestEnforceLimitReducesHugeMixedPayload(t *testing.T) {
	p := map[string]any{
		"conversationState": map[string]any{
			"history": []any{
				map[string]any{"userInputMessage": map[string]any{"content": strings.Repeat("a", 400_000)}},
				map[string]any{"assistantResponseMessage": map[string]any{"content": strings.Repeat("b", 400_000)}},
			},
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": "now",
					"images": []any{
						map[string]any{
							"format": "png",
							"source": map[string]any{"bytes": strings.Repeat("Z", 500_000)},
						},
					},
				},
			},
		},
	}
	limit := 600_000

	stats, err := enforceLimit(p, limit)
	if err != nil {
		t.Fatalf("enforceLimit: %v", err)
	}
	if stats.FinalBytes >= stats.OriginalBytes {
		t.Fatalf("expected payload reduction, stats=%+v", stats)
	}
	if got := mustSize(t, p); got > limit {
		t.Fatalf("payload still %d bytes, limit %d", got, limit)
	}
}

func TestClearAllHistory(t *testing.T) {
	p := map[string]any{
		"conversationState": map[string]any{
			"history": []any{
				map[string]any{"userInputMessage": map[string]any{"content": "old"}},
			},
		},
	}
	if !clearAllHistory(p) {
		t.Fatal("expected history clear")
	}
	if got := historyEntries(p); got != 0 {
		t.Fatalf("history entries=%d, want 0", got)
	}
	if clearAllHistory(p) {
		t.Fatal("expected second clear to be a no-op")
	}
}

func TestStripAllImages(t *testing.T) {
	p := map[string]any{
		"conversationState": map[string]any{
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": "hi",
					"images": []any{
						map[string]any{
							"format": "png",
							"source": map[string]any{"bytes": strings.Repeat("A", 500_000)},
						},
					},
				},
			},
		},
	}
	before := mustSize(t, p)
	if !stripAllImages(p) {
		t.Fatal("expected images stripped")
	}
	after := mustSize(t, p)
	if after >= before {
		t.Fatalf("expected smaller payload after stripping images: before=%d after=%d", before, after)
	}
}

func TestTruncateStringPreservesHeadAndTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		maxLen  int
		wantLen int
	}{
		{
			name:    "noop under limit",
			input:   "short",
			maxLen:  100,
			wantLen: 5,
		},
		{
			name:    "head tail with marker",
			input:   strings.Repeat("a", 50) + strings.Repeat("z", 50),
			maxLen:  80,
			wantLen: 80,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncateString(tc.input, tc.maxLen)
			if len(got) != tc.wantLen {
				t.Fatalf("len(got)=%d want %d", len(got), tc.wantLen)
			}
			if len(tc.input) <= tc.maxLen && got != tc.input {
				t.Fatalf("expected unchanged input, got %q", got)
			}
			if len(tc.input) > tc.maxLen && !strings.Contains(got, truncationMarker) {
				t.Fatalf("expected truncation marker in %q", got)
			}
		})
	}
}
