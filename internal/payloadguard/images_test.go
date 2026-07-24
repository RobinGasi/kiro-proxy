package payloadguard

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// makeNoisyPNG builds a w×h image of pseudo-random noise (which PNG cannot
// compress away, so the encoding is reliably large) and returns its PNG bytes.
func makeNoisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func imagePayload(t *testing.T, pngBytes []byte) map[string]any {
	t.Helper()
	return map[string]any{
		"conversationState": map[string]any{
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": "look at this",
					"images": []any{
						map[string]any{
							"format": "png",
							"source": map[string]any{"bytes": base64.StdEncoding.EncodeToString(pngBytes)},
						},
					},
				},
			},
		},
	}
}

func TestShrinkImageBytesProducesSmallerJPEG(t *testing.T) {
	raw := makeNoisyPNG(t, 800, 800)
	target := len(raw) / 4

	out, format, ok := shrinkImageBytes(raw, target)
	if !ok {
		t.Fatal("expected shrink to succeed")
	}
	if format != "jpeg" {
		t.Fatalf("format=%q want jpeg", format)
	}
	if len(out) >= len(raw) {
		t.Fatalf("expected smaller output: got %d, raw %d", len(out), len(raw))
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("output not decodable as JPEG: %v", err)
	}
}

func TestShrinkImageBytesUndecodable(t *testing.T) {
	if _, _, ok := shrinkImageBytes([]byte("not an image"), 100); ok {
		t.Fatal("expected undecodable input to fail")
	}
}

func TestGuardShrinksImageToFit(t *testing.T) {
	raw := makeNoisyPNG(t, 800, 800) // base64 is well over the limit
	p := imagePayload(t, raw)
	limit := 600_000
	if got := mustSize(t, p); got <= limit {
		t.Fatalf("test image not large enough: %d", got)
	}

	// autoTrim=false: the image must still be shrunk (non-destructive tier).
	stats, err := Guard(p, limit, false)
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if !stats.ImagesShrunk {
		t.Fatalf("expected ImagesShrunk, stats=%+v", stats)
	}
	if got := mustSize(t, p); got > limit {
		t.Fatalf("payload still %d bytes, limit %d", got, limit)
	}

	// The image must still be present, now re-encoded as JPEG.
	cur := p["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	imgs, ok := cur["images"].([]any)
	if !ok || len(imgs) == 0 {
		t.Fatal("expected image to survive shrinking")
	}
	im := imgs[0].(map[string]any)
	if im["format"] != "jpeg" {
		t.Fatalf("format=%v want jpeg", im["format"])
	}
	decoded, err := base64.StdEncoding.DecodeString(im["source"].(map[string]any)["bytes"].(string))
	if err != nil {
		t.Fatalf("shrunk image not valid base64: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(decoded)); err != nil {
		t.Fatalf("shrunk image not decodable JPEG: %v", err)
	}
}
