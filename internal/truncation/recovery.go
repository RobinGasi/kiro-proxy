// Package truncation implements the truncation-recovery system that works around
// upstream Kiro API limitations (issue #56). Kiro truncates large tool-call
// payloads and content mid-stream; because this is an upstream limitation that
// cannot be prevented, the gateway injects synthetic recovery messages so the
// model is told about the truncation and can adapt its approach.
//
// This package does NOT detect truncation itself — detection happens in the
// stream parser. Here we only record per-conversation truncation state (see
// Store) and generate the synthetic messages that inform the model.
package truncation

import (
	"log/slog"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
)

// TruncationInfo holds diagnostic information about a single truncation event,
// as produced by the stream parser.
type TruncationInfo struct {
	// SizeBytes is the size, in bytes, of the truncated payload.
	SizeBytes int `json:"size_bytes"`
	// Reason is a human-readable description of how the truncation was
	// detected (for example, "missing 2 closing braces").
	Reason string `json:"reason"`
}

// truncationToolResultContent is the body of the synthetic tool_result injected
// for a truncated tool call. It is worded to acknowledge the API limitation
// (not the model's fault), warn against repeating the same operation, and avoid
// prescriptive step-by-step instructions.
const truncationToolResultContent = "[API Limitation] Your tool call was truncated by the upstream API due to output size limits.\n\n" +
	"If the tool result below shows an error or unexpected behavior, this is likely a CONSEQUENCE of the truncation, " +
	"not the root cause. The tool call itself was cut off before it could be fully transmitted.\n\n" +
	"Repeating the exact same operation will be truncated again. Consider adapting your approach."

// truncationUserMessage is the synthetic user message injected when non-tool
// content is truncated. It avoids blaming the model and suggests adapting the
// approach without prescribing micro-steps.
const truncationUserMessage = "[System Notice] Your previous response was truncated by the API due to " +
	"output size limitations. This is not an error on your part. " +
	"If you need to continue, please adapt your approach rather than repeating the same output."

// ShouldInjectRecovery reports whether truncation recovery is enabled.
func ShouldInjectRecovery(cfg *config.Config) bool {
	return cfg != nil && cfg.TruncationRecovery
}

// GenerateTruncationToolResult builds a synthetic tool_result for a tool call
// that was truncated mid-stream, in the gateway's unified message format:
//
//	{"type": "tool_result", "tool_use_id": ..., "content": ..., "is_error": true}
//
// The info argument carries diagnostic details used only for logging.
func GenerateTruncationToolResult(toolName, toolUseID string, info TruncationInfo) map[string]any {
	slog.Debug("generated synthetic tool_result for truncated tool",
		"tool", toolName,
		"tool_use_id", toolUseID,
		"size_bytes", info.SizeBytes,
		"reason", info.Reason,
	)
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
		"content":     truncationToolResultContent,
		"is_error":    true,
	}
}

// GenerateTruncationUserMessage returns the synthetic user-message text injected
// when non-tool content is truncated.
func GenerateTruncationUserMessage() string {
	return truncationUserMessage
}
