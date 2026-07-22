package payloadguard

import (
	"encoding/json"
)

const (
	truncationMarker     = "\n\n[... truncated by kiro-proxy to fit Kiro API limit ...]"
	minTruncatedTextSize = 128 + len(truncationMarker)
)

// textField is a mutable string slot inside the Kiro payload tree.
type textField struct {
	get func() string
	set func(string)
}

// enforceLimit applies progressively stronger reductions until the payload fits
// maxBytes or no further reduction is possible.
func enforceLimit(payload map[string]any, maxBytes int) (TrimStats, error) {
	originalBytes, err := CheckPayloadSize(payload)
	if err != nil {
		return TrimStats{}, err
	}
	entries := historyEntries(payload)
	stats := TrimStats{
		OriginalBytes:   originalBytes,
		FinalBytes:      originalBytes,
		OriginalEntries: entries,
		FinalEntries:    entries,
	}

	if originalBytes <= maxBytes {
		return stats, nil
	}

	// Phase 1: shrink any single image that is larger than the entire payload
	// budget before sacrificing conversation history. Image-reading tools commonly
	// attach multi-megabyte PNGs that can be re-encoded below the limit while all
	// surrounding context remains intact.
	for range 32 {
		size, err := CheckPayloadSize(payload)
		if err != nil {
			return TrimStats{}, err
		}
		if size <= maxBytes {
			stats.FinalBytes = size
			stats.FinalEntries = historyEntries(payload)
			return stats, nil
		}
		_, source := largestImage(payload)
		if source == nil {
			break
		}
		imageData, _ := source["bytes"].(string)
		if len(imageData) <= maxBytes || !shrinkLargestImage(payload, size-maxBytes) {
			break
		}
		stats.ImagesShrunk = true
	}

	// Phase 2: drop oldest history pairs.
	trimStats, err := TrimPayloadToLimit(payload, maxBytes)
	if err != nil {
		return TrimStats{}, err
	}
	mergeTrimStats(&stats, trimStats)
	if size, err := CheckPayloadSize(payload); err != nil {
		return TrimStats{}, err
	} else if size <= maxBytes {
		stats.FinalBytes = size
		stats.FinalEntries = historyEntries(payload)
		return stats, nil
	}

	// Phase 3–6: shrink inline content and images before clearing history, then
	// strip images only as the final fallback.
	for range 32 {
		size, err := CheckPayloadSize(payload)
		if err != nil {
			return TrimStats{}, err
		}
		if size <= maxBytes {
			stats.FinalBytes = size
			stats.FinalEntries = historyEntries(payload)
			return stats, nil
		}

		excess := size - maxBytes
		if truncated := truncateLargestText(payload, excess); truncated {
			stats.ContentTruncated = true
			continue
		}
		if shrunk := shrinkLargestImage(payload, excess); shrunk {
			stats.ImagesShrunk = true
			continue
		}
		if cleared := clearAllHistory(payload); cleared {
			stats.HistoryCleared = true
			stats.FinalEntries = historyEntries(payload)
			continue
		}
		if stripped := stripAllImages(payload); stripped {
			stats.ImagesStripped = true
			continue
		}
		break
	}

	finalBytes, err := CheckPayloadSize(payload)
	if err != nil {
		return TrimStats{}, err
	}
	stats.FinalBytes = finalBytes
	stats.FinalEntries = historyEntries(payload)
	if finalBytes > maxBytes {
		return stats, &PayloadTooLargeError{Size: finalBytes, MaxBytes: maxBytes}
	}
	return stats, nil
}

func mergeTrimStats(dst *TrimStats, src TrimStats) {
	dst.Trimmed = dst.Trimmed || src.Trimmed
	dst.ContentTruncated = dst.ContentTruncated || src.ContentTruncated
	dst.HistoryCleared = dst.HistoryCleared || src.HistoryCleared
	dst.ImagesShrunk = dst.ImagesShrunk || src.ImagesShrunk
	dst.ImagesStripped = dst.ImagesStripped || src.ImagesStripped
	dst.FinalBytes = src.FinalBytes
	dst.FinalEntries = src.FinalEntries
	if dst.OriginalBytes == 0 {
		dst.OriginalBytes = src.OriginalBytes
	}
	if dst.OriginalEntries == 0 {
		dst.OriginalEntries = src.OriginalEntries
	}
}

// truncateLargestText shortens the single largest text field enough to recover
// at least excess bytes (plus a small safety margin). Returns false when there
// is nothing left to truncate.
func truncateLargestText(payload map[string]any, excess int) bool {
	fields := collectTextFields(payload)
	if len(fields) == 0 {
		return false
	}

	largest := -1
	largestLen := 0
	for i, f := range fields {
		curLen := len(f.get())
		if curLen <= minTruncatedTextSize {
			continue
		}
		if curLen > largestLen {
			largest = i
			largestLen = curLen
		}
	}
	if largest < 0 {
		return false
	}

	cur := fields[largest].get()

	// Target a reduction slightly larger than excess to account for JSON overhead.
	targetLen := len(cur) - excess - 64
	if targetLen < 128 {
		targetLen = 128
	}
	if targetLen >= len(cur) {
		return false
	}

	fields[largest].set(truncateString(cur, targetLen))
	return true
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= len(truncationMarker) {
		return s[:maxLen]
	}
	budget := maxLen - len(truncationMarker)
	head := budget / 2
	tail := budget - head
	if head+tail >= len(s) {
		return s
	}
	return s[:head] + truncationMarker + s[len(s)-tail:]
}

func collectTextFields(payload map[string]any) []textField {
	var fields []textField
	cs, _ := payload["conversationState"].(map[string]any)
	if cs == nil {
		return fields
	}

	if hist, ok := cs["history"].([]any); ok {
		for _, e := range hist {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			fields = append(fields, textFieldsFromUserMessage(entry["userInputMessage"])...)
			fields = append(fields, textFieldsFromAssistantMessage(entry["assistantResponseMessage"])...)
		}
	}

	if cur, ok := cs["currentMessage"].(map[string]any); ok {
		fields = append(fields, textFieldsFromUserMessage(cur["userInputMessage"])...)
	}
	return fields
}

func textFieldsFromUserMessage(raw any) []textField {
	msg, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var fields []textField
	if content, ok := msg["content"].(string); ok && content != "" {
		fields = append(fields, textField{
			get: func() string { c, _ := msg["content"].(string); return c },
			set: func(s string) { msg["content"] = s },
		})
	}
	ctx, _ := msg["userInputMessageContext"].(map[string]any)
	if ctx != nil {
		if trs, ok := ctx["toolResults"].([]any); ok {
			for _, tr := range trs {
				trMap, ok := tr.(map[string]any)
				if !ok {
					continue
				}
				fields = append(fields, textFieldsFromToolResult(trMap)...)
			}
		}
	}
	return fields
}

func textFieldsFromAssistantMessage(raw any) []textField {
	msg, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var fields []textField
	if content, ok := msg["content"].(string); ok && content != "" {
		fields = append(fields, textField{
			get: func() string { c, _ := msg["content"].(string); return c },
			set: func(s string) { msg["content"] = s },
		})
	}
	if tus, ok := msg["toolUses"].([]any); ok {
		for _, tu := range tus {
			tuMap, ok := tu.(map[string]any)
			if !ok {
				continue
			}
			m := tuMap
			fields = append(fields, textField{
				get: func() string {
					b, err := json.Marshal(m["input"])
					if err != nil {
						return ""
					}
					return string(b)
				},
				set: func(s string) {
					var input map[string]any
					if json.Unmarshal([]byte(s), &input) == nil {
						m["input"] = input
						return
					}
					m["input"] = map[string]any{"_truncated": s}
				},
			})
		}
	}
	return fields
}

func textFieldsFromToolResult(tr map[string]any) []textField {
	content, ok := tr["content"].([]any)
	if !ok {
		return nil
	}
	var fields []textField
	for _, part := range content {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		text, ok := pm["text"].(string)
		if !ok || text == "" {
			continue
		}
		partMap := pm
		fields = append(fields, textField{
			get: func() string { t, _ := partMap["text"].(string); return t },
			set: func(s string) { partMap["text"] = s },
		})
	}
	return fields
}

func clearAllHistory(payload map[string]any) bool {
	cs, ok := payload["conversationState"].(map[string]any)
	if !ok {
		return false
	}
	hist, ok := cs["history"].([]any)
	if !ok || len(hist) == 0 {
		return false
	}
	delete(cs, "history")
	repairOrphanedCurrentToolResults(payload)
	return true
}

func stripAllImages(payload map[string]any) bool {
	cs, ok := payload["conversationState"].(map[string]any)
	if !ok {
		return false
	}
	stripped := false
	if hist, ok := cs["history"].([]any); ok {
		for _, e := range hist {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if user, ok := entry["userInputMessage"].(map[string]any); ok {
				if _, has := user["images"]; has {
					delete(user, "images")
					stripped = true
				}
			}
		}
	}
	if cur, ok := cs["currentMessage"].(map[string]any); ok {
		if user, ok := cur["userInputMessage"].(map[string]any); ok {
			if _, has := user["images"]; has {
				delete(user, "images")
				stripped = true
			}
		}
	}
	return stripped
}
