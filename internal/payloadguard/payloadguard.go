// Package payloadguard enforces the Kiro API payload-size limit.
//
// The Kiro API rejects request bodies larger than ~615KB with a misleading
// "Improperly formed request." (reason: null) error. To avoid that, this
// package measures the serialized size of an outgoing Kiro payload and, when it
// exceeds the configured limit (config.KiroMaxPayloadBytes), reduces it in two
// tiers:
//
//   - Always: shrink oversized images by downscaling/re-encoding them. This is
//     non-destructive to conversation context, so it runs regardless of the
//     auto-trim setting.
//   - Only when auto-trimming is enabled (config.AutoTrimPayload): drop the
//     oldest user/assistant history pairs until the payload fits. When
//     auto-trim is disabled the payload is left intact and a clear
//     *PayloadTooLargeError is returned so the client (and the model) stay in
//     sync — the caller can surface "context too large, use /compact" rather
//     than silently losing history.
//
// It operates on the same in-memory representation used elsewhere for the Kiro
// payload: a map[string]any decoded from / destined for JSON. Maps are mutated
// in place, so any reduction is reflected in the caller's payload.
package payloadguard

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TrimStats summarizes a trim operation.
type TrimStats struct {
	// OriginalBytes is the serialized size before trimming.
	OriginalBytes int
	// FinalBytes is the serialized size after trimming.
	FinalBytes int
	// OriginalEntries is the number of history entries before trimming.
	OriginalEntries int
	// FinalEntries is the number of history entries after trimming.
	FinalEntries int
	// Trimmed reports whether any history entries were removed.
	Trimmed bool
	// ImagesShrunk reports whether any image attachment was downscaled/re-encoded
	// to fit the limit. This is non-destructive to conversation context and runs
	// regardless of the auto-trim setting.
	ImagesShrunk bool
}

// PayloadTooLargeError is returned by [Guard] when a payload exceeds the
// configured limit and could not (or may not) be brought under it.
type PayloadTooLargeError struct {
	// Size is the serialized payload size in bytes.
	Size int
	// MaxBytes is the configured limit (config.KiroMaxPayloadBytes).
	MaxBytes int
}

// Error implements the error interface.
func (e *PayloadTooLargeError) Error() string {
	return fmt.Sprintf(
		"conversation context is too large for the Kiro API (%d bytes, limit %d). "+
			"Use /compact in your client or reduce tool output size; "+
			"enable AUTO_TRIM_PAYLOAD=true to let the proxy trim automatically",
		e.Size, e.MaxBytes,
	)
}

// CheckPayloadSize returns the serialized byte size of the payload.
//
// It measures the compact JSON encoding produced by encoding/json — the same
// encoding used to transmit the request — so the result reflects the actual
// on-the-wire size. Non-ASCII is emitted as raw UTF-8 (not \u-escaped), matching
// what this proxy actually sends, so the count is not inflated for non-ASCII
// text.
func CheckPayloadSize(payload map[string]any) (int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("serializing kiro payload: %w", err)
	}
	return len(b), nil
}

// Guard checks payload against maxBytes and enforces the limit.
//
//   - If the payload is within the limit, it is left untouched and stats with
//     Trimmed=false are returned.
//   - Otherwise it first shrinks oversized images (non-destructive; always
//     applied). If that brings the payload under the limit, the shrunk payload
//     is returned with ImagesShrunk=true.
//   - If it still exceeds the limit and autoTrim is true, the oldest
//     user/assistant history pairs are dropped until it fits (or only two
//     entries remain).
//   - If it still exceeds the limit and autoTrim is false, the payload is left
//     as-is (images already shrunk) and a *PayloadTooLargeError is returned so
//     the caller can tell the client the context is too large.
//
// Callers pass config.KiroMaxPayloadBytes and config.AutoTrimPayload.
func Guard(payload map[string]any, maxBytes int, autoTrim bool) (TrimStats, error) {
	originalBytes, err := CheckPayloadSize(payload)
	if err != nil {
		return TrimStats{}, err
	}

	originalEntries := historyEntries(payload)
	stats := TrimStats{
		OriginalBytes:   originalBytes,
		FinalBytes:      originalBytes,
		OriginalEntries: originalEntries,
		FinalEntries:    originalEntries,
	}

	if originalBytes <= maxBytes {
		return stats, nil
	}

	// Tier 1 (always): shrink oversized images. Keeps all conversation context.
	// shrinkImagesToFit reports the final serialized size, so no re-measure here.
	shrunk, size, err := shrinkImagesToFit(payload, maxBytes)
	if err != nil {
		return TrimStats{}, err
	}
	stats.ImagesShrunk = shrunk
	stats.FinalBytes = size
	stats.FinalEntries = historyEntries(payload)
	if size <= maxBytes {
		return stats, nil
	}

	// Tier 2 (opt-in): drop oldest history pairs. Destructive to context, so
	// gated behind autoTrim; otherwise report the payload as too large.
	if !autoTrim {
		return stats, &PayloadTooLargeError{Size: size, MaxBytes: maxBytes}
	}

	trimStats, err := TrimPayloadToLimit(payload, maxBytes)
	if err != nil {
		return TrimStats{}, err
	}
	stats.Trimmed = trimStats.Trimmed
	stats.FinalBytes = trimStats.FinalBytes
	stats.FinalEntries = trimStats.FinalEntries

	// TrimPayloadToLimit already reported the post-trim size; no re-measure.
	if stats.FinalBytes > maxBytes {
		return stats, &PayloadTooLargeError{Size: stats.FinalBytes, MaxBytes: maxBytes}
	}
	return stats, nil
}

// TrimPayloadToLimit trims the oldest conversation-history entries so the
// serialized payload fits under maxBytes.
//
// History is removed in user/assistant pairs (two entries at a time), the start
// is realigned to a userInputMessage boundary, empty toolUses arrays are
// stripped, and toolResults orphaned by the trim are repaired (dropped, with any
// text content preserved inline). The payload is mutated in place. Trimming
// never drops below two history entries, so the result may still exceed maxBytes.
func TrimPayloadToLimit(payload map[string]any, maxBytes int) (TrimStats, error) {
	originalBytes, err := CheckPayloadSize(payload)
	if err != nil {
		return TrimStats{}, err
	}

	cs, _ := payload["conversationState"].(map[string]any)
	var history []any
	if cs != nil {
		history, _ = cs["history"].([]any)
	}

	if len(history) == 0 {
		return TrimStats{
			OriginalBytes:   originalBytes,
			FinalBytes:      originalBytes,
			OriginalEntries: 0,
			FinalEntries:    0,
			Trimmed:         false,
		}, nil
	}

	originalEntries := len(history)

	// Strip empty toolUses before measuring (a Kiro quirk).
	stripEmptyToolUses(history)

	// Trim pairs from the front until under the limit, keeping > 2 entries.
	//
	// Rather than re-serializing the whole payload on every iteration
	// (O(N × payload size)), measure once and decrement by each removed entry's
	// own serialized size. In compact JSON, removing a non-final array element
	// drops exactly its bytes plus one separating comma, so this tracks the true
	// size exactly while marshaling each entry only once (O(payload size) total).
	curSize, err := CheckPayloadSize(payload)
	if err != nil {
		return TrimStats{}, err
	}
	entrySize := make([]int, len(history))
	for i, e := range history {
		b, err := json.Marshal(e)
		if err != nil {
			return TrimStats{}, fmt.Errorf("serializing kiro history entry: %w", err)
		}
		entrySize[i] = len(b)
	}
	off := 0
	for len(history)-off > 2 && curSize > maxBytes {
		curSize -= entrySize[off] + 1 + entrySize[off+1] + 1 // two entries + their commas
		off += 2
	}
	if off > 0 {
		history = history[off:]
		cs["history"] = history
	}

	// Align the start to a userInputMessage entry.
	history = alignToUserMessage(history)
	cs["history"] = history

	// Repair toolResults that lost their originating assistant message.
	repairOrphanedToolResults(history)

	finalBytes, err := CheckPayloadSize(payload)
	if err != nil {
		return TrimStats{}, err
	}
	return TrimStats{
		OriginalBytes:   originalBytes,
		FinalBytes:      finalBytes,
		OriginalEntries: originalEntries,
		FinalEntries:    len(history),
		Trimmed:         originalEntries != len(history),
	}, nil
}

// historyEntries returns the number of conversation-history entries in payload.
func historyEntries(payload map[string]any) int {
	cs, ok := payload["conversationState"].(map[string]any)
	if !ok {
		return 0
	}
	h, _ := cs["history"].([]any)
	return len(h)
}

// stripEmptyToolUses removes empty "toolUses": [] arrays from assistant messages
// in place.
func stripEmptyToolUses(history []any) {
	for _, e := range history {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		assistant, ok := entry["assistantResponseMessage"].(map[string]any)
		if !ok {
			continue
		}
		if tu, ok := assistant["toolUses"]; ok {
			if arr, ok := tu.([]any); ok && len(arr) == 0 {
				delete(assistant, "toolUses")
			}
		}
	}
}

// alignToUserMessage drops leading entries until history starts with a
// userInputMessage entry, returning the resliced history.
func alignToUserMessage(history []any) []any {
	for len(history) > 0 {
		if entry, ok := history[0].(map[string]any); ok {
			if _, has := entry["userInputMessage"]; has {
				break
			}
		}
		history = history[1:]
	}
	return history
}

// repairOrphanedToolResults removes toolResults that reference toolUseIds not
// present in the preceding assistant message. Any text content from an orphaned
// result is preserved inline in the user message content, marked as trimmed.
func repairOrphanedToolResults(history []any) {
	for i, e := range history {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		userMsg, ok := entry["userInputMessage"].(map[string]any)
		if !ok {
			continue
		}
		repairToolResultsInUserMessage(userMsg, toolUseIDsAt(history, i-1))
	}
}

func toolUseIDsAt(history []any, index int) map[string]struct{} {
	validIDs := map[string]struct{}{}
	if index < 0 || index >= len(history) {
		return validIDs
	}
	entry, ok := history[index].(map[string]any)
	if !ok {
		return validIDs
	}
	assistant, ok := entry["assistantResponseMessage"].(map[string]any)
	if !ok {
		return validIDs
	}
	toolUses, ok := assistant["toolUses"].([]any)
	if !ok {
		return validIDs
	}
	for _, toolUse := range toolUses {
		toolUseMap, ok := toolUse.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := toolUseMap["toolUseId"].(string); ok && id != "" {
			validIDs[id] = struct{}{}
		}
	}
	return validIDs
}

func repairToolResultsInUserMessage(userMsg map[string]any, validIDs map[string]struct{}) {
	ctx, ok := userMsg["userInputMessageContext"].(map[string]any)
	if !ok {
		return
	}
	toolResults, ok := ctx["toolResults"].([]any)
	if !ok {
		return
	}

	kept := make([]any, 0, len(toolResults))
	var orphanedText []string
	for _, toolResult := range toolResults {
		toolResultMap, ok := toolResult.(map[string]any)
		if !ok {
			continue
		}
		id, _ := toolResultMap["toolUseId"].(string)
		if _, valid := validIDs[id]; valid {
			kept = append(kept, toolResult)
			continue
		}

		// Orphaned: preserve any text content.
		switch content := toolResultMap["content"].(type) {
		case []any:
			for _, part := range content {
				if partMap, ok := part.(map[string]any); ok {
					if text, ok := partMap["text"].(string); ok && text != "" {
						orphanedText = append(orphanedText, text)
					}
				}
			}
		case string:
			if content != "" {
				orphanedText = append(orphanedText, content)
			}
		}
	}

	if len(kept) == len(toolResults) {
		return
	}
	if len(kept) > 0 {
		ctx["toolResults"] = kept
	} else {
		delete(ctx, "toolResults")
		if len(ctx) == 0 {
			delete(userMsg, "userInputMessageContext")
		}
	}
	if len(orphanedText) > 0 {
		marker := "\n[trimmed tool result] " + strings.Join(orphanedText, "; ")
		current, _ := userMsg["content"].(string)
		userMsg["content"] = current + marker
	}
}
