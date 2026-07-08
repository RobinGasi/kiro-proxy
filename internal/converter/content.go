package converter

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// ExtractTextContent concatenates the text of a unified content block list.
//
// Image, image_url, tool_use and tool_result blocks are skipped; text blocks
// (and unknown blocks that carry text) are joined with no separator. A bare-string
// message is represented here as a single text block.
func ExtractTextContent(parts []ContentPart) string {
	if len(parts) == 0 {
		return ""
	}
	var b []byte
	for _, p := range parts {
		switch p.Type {
		case "image", "image_url", "tool_reference", "tool_use", "tool_result":
			continue
		case "text":
			b = append(b, p.Text...)
		default:
			// Unknown block: include it if it carries text.
			if p.Text != "" {
				b = append(b, p.Text...)
			}
		}
	}
	return string(b)
}

// textContent wraps a plain string as a single-block content list. It is used
// wherever a bare string is assigned to a message's content.
func textContent(s string) []ContentPart {
	return []ContentPart{{Type: "text", Text: s}}
}

// extractImagesFromContent collects image blocks (Type == "image") from a content
// list into unified [Image] form. Blocks with empty data are skipped. API-specific
// data-URL parsing is handled later by convertImagesToKiroFormat.
func extractImagesFromContent(parts []ContentPart) []Image {
	var images []Image
	for _, p := range parts {
		if p.Type != "image" {
			continue
		}
		if p.Data == "" {
			continue
		}
		images = append(images, Image{MediaType: p.MediaType, Data: p.Data})
	}
	if len(images) > 0 {
		slog.Debug("extracted images from content", "count", len(images))
	}
	return images
}

// toolResultText renders a tool-result content value (string or []ContentPart) as
// plain text: a string is used as-is, otherwise its text content is extracted.
func toolResultText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []ContentPart:
		return ExtractTextContent(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// argsToInput converts tool-call arguments (a JSON string or a map) into a
// map[string]any suitable for Kiro's toolUse.input. Handles both string (OpenAI)
// and dict (Anthropic unified) argument forms; malformed JSON yields an empty map
// rather than an error.
func argsToInput(args any) map[string]any {
	switch v := args.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		if v == nil {
			return map[string]any{}
		}
		return v
	case string:
		if v == "" {
			return map[string]any{}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			slog.Debug("failed to parse tool-call arguments JSON, using empty input", "error", err)
			return map[string]any{}
		}
		return m
	case json.RawMessage:
		if len(v) == 0 {
			return map[string]any{}
		}
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			slog.Debug("failed to parse tool-call arguments JSON, using empty input", "error", err)
			return map[string]any{}
		}
		return m
	default:
		return map[string]any{}
	}
}

// argsToText renders tool-call arguments as a string for the human-readable
// fallback used when no tools are defined. The default is "{}".
func argsToText(args any) string {
	switch v := args.(type) {
	case nil:
		return "{}"
	case string:
		if v == "" {
			return "{}"
		}
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}
