package converter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
	"git.gasi.dev/robin/kiro-proxy/internal/model"
	"git.gasi.dev/robin/kiro-proxy/internal/payloadguard"
	"git.gasi.dev/robin/kiro-proxy/internal/util"
)

// ErrNoMessages is returned by [BuildKiroPayload] when, after normalization,
// there are no messages to send.
var ErrNoMessages = errors.New("no messages to send")

// BuildKiroPayload assembles the Kiro / CodeWhisperer conversationState request
// body from a unified [Request] and serializes it to JSON.
//
// It folds the system prompt (plus tool documentation and thinking/truncation
// legitimization) into the conversation, sanitizes tool schemas, extracts and
// converts images, tool calls and tool results, normalizes roles, and enforces
// Kiro's alternation and first-message-is-user requirements. The conversationId
// is derived deterministically from the request messages via
// util.GenerateConversationID.
//
// internalModelID is the already-resolved Kiro model id; when empty it is derived
// from in.Model via model.GetModelIDForKiro. profileARN is the AWS CodeWhisperer
// profile ARN — it is always included when non-empty.
//
// Before serializing, the payload is checked with payloadguard.Guard using
// cfg.KiroMaxPayloadBytes and cfg.AutoTrimPayload: oversized payloads are either
// auto-trimmed or rejected with a *payloadguard.PayloadTooLargeError.
func BuildKiroPayload(ctx context.Context, cfg *config.Config, in Request, profileARN, internalModelID string) ([]byte, map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	modelID := internalModelID
	if modelID == "" {
		modelID = model.GetModelIDForKiro(in.Model, cfg.HiddenModels)
	}

	conversationID := util.GenerateConversationID(simplifyMessages(in.Messages))

	// Apply tool choice before documentation and aliasing so every downstream
	// representation sees the same selected definitions.
	selectedTools, toolChoiceInstruction := applyToolChoice(in.Tools, in.ToolChoice)
	processedTools, toolDocumentation := processToolsWithLongDescriptions(selectedTools, cfg.ToolDescriptionMaxLength)

	// Rewrite tool names that exceed Kiro's 64-character limit to a deterministic
	// ≤64-char alias (see ShortenToolName). toolNameReverse maps each alias back to
	// its original name; the caller uses it (via RestoreToolName) to restore names
	// in the model's tool_use blocks, so the aliasing is invisible to clients.
	processedTools, toolNameReverse := aliasTools(processedTools)

	// Prefer native reasoning and copy generation controls only when Kiro's live
	// schema advertises them. XML remains a reasoning-only fallback.
	nativeReasoning := buildNativeReasoningFields(in.ModelRequestSchema, in.Thinking)
	modelRequestFields := mergeRequestFields(nativeReasoning, buildGenerationFields(in.ModelRequestSchema, in.Generation))
	emulatedThinking := in.Thinking
	if nativeReasoning != nil {
		emulatedThinking.Enabled = false
	}
	fullSystem := in.System
	fullSystem = appendSystemAddition(fullSystem, toolDocumentation)
	fullSystem = appendSystemAddition(fullSystem, toolChoiceInstruction)
	fullSystem = appendSystemAddition(fullSystem, getThinkingSystemPromptAddition(cfg, emulatedThinking))
	fullSystem = appendSystemAddition(fullSystem, getTruncationRecoverySystemAddition(cfg))

	// When no tools are defined, strip all tool content (Kiro rejects toolResults
	// without tools); otherwise repair orphaned tool results.
	var messages []Message
	if len(processedTools) == 0 {
		messages, _ = stripAllToolContent(in.Messages)
	} else {
		messages, _ = ensureAssistantBeforeToolResults(in.Messages)
		// Alias history tool-use names so they match the aliased tool definitions.
		messages = aliasMessageToolNames(messages)
	}

	// Normalize the conversation shape for Kiro.
	messages = mergeAdjacentMessages(messages)
	messages = ensureFirstMessageIsUser(messages)
	messages = NormalizeMessageRoles(messages)
	messages = ensureAlternatingRoles(messages)

	if len(messages) == 0 {
		return nil, nil, ErrNoMessages
	}

	// History is every message except the last; the system prompt is folded into
	// the first user history message when present.
	var historyMsgs []Message
	if len(messages) > 1 {
		historyMsgs = messages[:len(messages)-1]
	}
	if fullSystem != "" && len(historyMsgs) > 0 && historyMsgs[0].Role == "user" {
		original := ExtractTextContent(historyMsgs[0].Content)
		historyMsgs[0].Content = textContent(fullSystem + "\n\n" + original)
	}
	history := buildKiroHistory(historyMsgs, modelID)

	// Current message (the last one).
	current := messages[len(messages)-1]
	currentContent := ExtractTextContent(current.Content)

	// If there is no history, fold the system prompt into the current message.
	if fullSystem != "" && len(history) == 0 {
		currentContent = fullSystem + "\n\n" + currentContent
	}

	// An assistant as the final message is pushed into history with a placeholder
	// user message taking its place.
	if current.Role == "assistant" {
		history = append(history, map[string]any{
			"assistantResponseMessage": map[string]any{"content": currentContent},
		})
		currentContent = "(empty placeholder)"
	}
	if currentContent == "" {
		currentContent = "(empty placeholder)"
	}

	// Images for the current message go directly on userInputMessage.
	images := current.Images
	if len(images) == 0 {
		images = extractImagesFromContent(current.Content)
	}
	kiroImages := convertImagesToKiroFormat(images)
	if len(kiroImages) > 0 {
		slog.Debug("added images to current message", "count", len(kiroImages))
	}

	// userInputMessageContext carries tools and toolResults (not images).
	userInputContext := map[string]any{}
	if kiroTools := convertToolsToKiroFormat(processedTools); len(kiroTools) > 0 {
		userInputContext["tools"] = kiroTools
	}
	if len(current.ToolResults) > 0 {
		if kr := convertToolResultsToKiroFormat(current.ToolResults); len(kr) > 0 {
			userInputContext["toolResults"] = kr
		}
	} else if tr := extractToolResultsFromContent(current.Content); len(tr) > 0 {
		userInputContext["toolResults"] = tr
	}

	// Inject XML thinking tags only for legacy models without native fields.
	if current.Role == "user" {
		currentContent = injectThinkingTags(currentContent, cfg, emulatedThinking)
	}

	userInputMessage := map[string]any{
		"content": currentContent,
		"modelId": modelID,
		"origin":  "AI_EDITOR",
	}
	if len(kiroImages) > 0 {
		userInputMessage["images"] = kiroImages
	}
	if len(userInputContext) > 0 {
		userInputMessage["userInputMessageContext"] = userInputContext
	}

	conversationState := map[string]any{
		"chatTriggerType": "MANUAL",
		"conversationId":  conversationID,
		"currentMessage": map[string]any{
			"userInputMessage": userInputMessage,
		},
	}
	if len(history) > 0 {
		conversationState["history"] = history
	}

	payload := map[string]any{
		"conversationState": conversationState,
	}
	if modelRequestFields != nil {
		payload["additionalModelRequestFields"] = modelRequestFields
	}
	if profileARN != "" {
		payload["profileArn"] = profileARN
	}

	// Enforce the Kiro payload-size limit (auto-trim or reject).
	stats, err := payloadguard.Guard(payload, cfg.KiroMaxPayloadBytes, cfg.AutoTrimPayload)
	if err != nil {
		return nil, nil, err
	}
	if stats.Trimmed || stats.ImagesShrunk {
		slog.Info("reduced payload for Kiro API limit",
			"original_entries", stats.OriginalEntries, "final_entries", stats.FinalEntries,
			"original_bytes", stats.OriginalBytes, "final_bytes", stats.FinalBytes,
			"history_trimmed", stats.Trimmed,
			"images_shrunk", stats.ImagesShrunk)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("serializing kiro payload: %w", err)
	}
	return body, toolNameReverse, nil
}

// appendSystemAddition appends a system-prompt addition to base. When base is
// empty the addition is trimmed of surrounding whitespace.
func appendSystemAddition(base, addition string) string {
	if addition == "" {
		return base
	}
	if base != "" {
		return base + addition
	}
	return strings.TrimSpace(addition)
}

// simplifyMessages reduces unified messages to the {role, content} shape consumed
// by util.GenerateConversationID for deterministic conversation-id derivation.
func simplifyMessages(messages []Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, map[string]any{
			"role":    m.Role,
			"content": ExtractTextContent(m.Content),
		})
	}
	return out
}
