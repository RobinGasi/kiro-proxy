package converter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

func noisyPNGBase64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(2))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestBuildKiroPayloadShrinksOversizedImage exercises the full production path:
// an oversized screenshot must be downscaled to fit the Kiro limit and still
// ship in the serialized body (as JPEG) rather than being silently dropped.
func TestBuildKiroPayloadShrinksOversizedImage(t *testing.T) {
	imgData := noisyPNGBase64(t, 800, 800) // base64 is well over the limit
	req := Request{
		Model: "claude-sonnet-4",
		Messages: []Message{{
			Role:    "user",
			Content: []ContentPart{{Type: "text", Text: "what is in this screenshot?"}},
			Images:  []Image{{MediaType: "image/png", Data: imgData}},
		}},
	}

	cfg := &config.Config{KiroMaxPayloadBytes: 600_000, AutoTrimPayload: true}
	body, _, err := BuildKiroPayload(context.Background(), cfg, req, "", "kiro-model")
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}
	if len(body) > cfg.KiroMaxPayloadBytes {
		t.Fatalf("body %d bytes exceeds limit %d", len(body), cfg.KiroMaxPayloadBytes)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cur := payload["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	imgs, ok := cur["images"].([]any)
	if !ok || len(imgs) == 0 {
		t.Fatal("image was dropped instead of shrunk")
	}
	im := imgs[0].(map[string]any)
	if im["format"] != "jpeg" {
		t.Fatalf("format=%v, want jpeg", im["format"])
	}
	raw, err := base64.StdEncoding.DecodeString(im["source"].(map[string]any)["bytes"].(string))
	if err != nil {
		t.Fatalf("bad base64: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("shrunk image not valid JPEG: %v", err)
	}
	// The image survived, so the "image removed" note must NOT be present.
	if strings.Contains(cur["content"].(string), "was removed") {
		t.Fatal("unexpected image-removed note when the image survived")
	}
}

// TestBuildKiroPayloadPreservesToolHistoryWhenShrinkingOversizedImage guards
// the Claude Code image-read path: shrinking a large tool-result image must not
// discard the preceding tool_use and the rest of the conversation history.
func TestBuildKiroPayloadPreservesToolHistoryWhenShrinkingOversizedImage(t *testing.T) {
	imgData := noisyPNGBase64(t, 800, 800)
	req := Request{
		Model: "claude-sonnet-4",
		Messages: []Message{
			{
				Role:    "user",
				Content: []ContentPart{{Type: "text", Text: "Render and review the wallpaper"}},
			},
			{
				Role:    "assistant",
				Content: []ContentPart{{Type: "text", Text: "The wallpaper has been generated"}},
			},
			{
				Role:    "user",
				Content: []ContentPart{{Type: "text", Text: "Check that it is dark and low contrast"}},
			},
			{
				Role:    "assistant",
				Content: []ContentPart{{Type: "text", Text: "Let me view it"}},
				ToolCalls: []ToolCall{{
					ID:       "toolu_read",
					Function: FunctionCall{Name: "Read", Arguments: map[string]any{"file_path": "wallpaper.png"}},
				}},
			},
			{
				Role:        "user",
				ToolResults: []ToolResult{{ToolUseID: "toolu_read", Content: "image contents"}},
				Images:      []Image{{MediaType: "image/png", Data: imgData}},
			},
		},
		Tools: []Tool{{
			Name:        "Read",
			Description: "Read a file",
			InputSchema: map[string]any{"type": "object"},
		}},
	}

	cfg := &config.Config{KiroMaxPayloadBytes: 600_000, AutoTrimPayload: true}
	body, _, err := BuildKiroPayload(context.Background(), cfg, req, "", "kiro-model")
	if err != nil {
		t.Fatalf("BuildKiroPayload: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	conversation := payload["conversationState"].(map[string]any)
	history, ok := conversation["history"].([]any)
	if !ok || len(history) != 4 {
		t.Fatalf("conversation history was cleared while shrinking the image: %+v", history)
	}
	assistant := history[3].(map[string]any)["assistantResponseMessage"].(map[string]any)
	toolUses, ok := assistant["toolUses"].([]any)
	if !ok || len(toolUses) != 1 || toolUses[0].(map[string]any)["toolUseId"] != "toolu_read" {
		t.Fatalf("preceding tool_use was not preserved: %+v", assistant)
	}
}
