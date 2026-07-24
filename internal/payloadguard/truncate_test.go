package payloadguard

import (
	"errors"
	"strings"
	"testing"
)

// TestShrinkImagesToFitReducesImagePayload verifies the non-destructive tier:
// an oversized image is shrunk until the payload fits, with no history touched.
func TestShrinkImagesToFitReducesImagePayload(t *testing.T) {
	raw := makeNoisyPNG(t, 800, 800) // base64 is well over the limit
	p := imagePayload(t, raw)
	limit := 600_000
	if got := mustSize(t, p); got <= limit {
		t.Fatalf("test image not large enough: %d", got)
	}

	shrunk, size, err := shrinkImagesToFit(p, limit)
	if err != nil {
		t.Fatalf("shrinkImagesToFit: %v", err)
	}
	if !shrunk {
		t.Fatal("expected an image to be shrunk")
	}
	if size > limit {
		t.Fatalf("reported size %d exceeds limit %d", size, limit)
	}
	if got := mustSize(t, p); got != size {
		t.Fatalf("reported size %d != actual serialized size %d", size, got)
	}
}

// TestGuardShrinksImagesWithoutAutoTrim verifies that image shrinking is applied
// even when auto-trim is disabled — an oversized screenshot must not cost the
// model its conversation history.
func TestGuardShrinksImagesWithoutAutoTrim(t *testing.T) {
	raw := makeNoisyPNG(t, 800, 800)
	p := imagePayload(t, raw)
	limit := 600_000
	if got := mustSize(t, p); got <= limit {
		t.Fatalf("test image not large enough: %d", got)
	}

	stats, err := Guard(p, limit, false)
	if err != nil {
		t.Fatalf("Guard should succeed by shrinking the image, got: %v", err)
	}
	if !stats.ImagesShrunk {
		t.Fatalf("expected ImagesShrunk, stats=%+v", stats)
	}
	if got := mustSize(t, p); got > limit {
		t.Fatalf("payload still %d bytes, limit %d", got, limit)
	}
}

// TestGuardRejectsOversizedTextWithoutAutoTrim is the core regression guard:
// when a text-heavy conversation exceeds the limit and auto-trim is OFF, Guard
// must return a PayloadTooLargeError and leave history INTACT rather than
// silently dropping context.
func TestGuardRejectsOversizedTextWithoutAutoTrim(t *testing.T) {
	p := map[string]any{
		"conversationState": map[string]any{
			"history": []any{
				map[string]any{"userInputMessage": map[string]any{"content": strings.Repeat("a", 400_000)}},
				map[string]any{"assistantResponseMessage": map[string]any{"content": strings.Repeat("b", 400_000)}},
			},
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{"content": "now"},
			},
		},
	}
	limit := 600_000
	before := historyEntries(p)

	stats, err := Guard(p, limit, false)
	var tooLarge *PayloadTooLargeError
	if err == nil {
		t.Fatal("expected PayloadTooLargeError with auto-trim disabled")
	}
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected *PayloadTooLargeError, got %T: %v", err, err)
	}
	if stats.Trimmed {
		t.Error("history must not be trimmed when auto-trim is disabled")
	}
	if after := historyEntries(p); after != before {
		t.Errorf("history was mutated: before=%d after=%d", before, after)
	}
}

// TestGuardTrimsOversizedTextWithAutoTrim verifies the opt-in destructive tier:
// with auto-trim ON, oldest history pairs are dropped until the payload fits.
func TestGuardTrimsOversizedTextWithAutoTrim(t *testing.T) {
	p := makePayload(40)
	full := mustSize(t, p)
	limit := full / 2

	stats, err := Guard(p, limit, true)
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if !stats.Trimmed {
		t.Fatalf("expected trimming, stats=%+v", stats)
	}
	if got := mustSize(t, p); got > limit {
		t.Fatalf("payload still %d bytes, limit %d", got, limit)
	}
	if stats.FinalEntries >= stats.OriginalEntries {
		t.Fatalf("expected fewer entries: orig=%d final=%d", stats.OriginalEntries, stats.FinalEntries)
	}
}
