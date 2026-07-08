// Package tokenizer provides fast, approximate token counting for requests and
// responses. It is used purely for usage reporting and estimation — it must
// never gate, validate, or reject a request.
//
// Token counts use a character-based heuristic: roughly one token per four
// characters. A Claude correction factor of 1.15 is applied by default because
// Claude tokenizes text about 15% more densely than GPT-4 (cl100k_base).
//
// CalculateTokensFromContextUsage is also provided here, since it is a token
// estimation helper.
package tokenizer

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ClaudeCorrectionFactor scales the GPT-4-oriented estimate up for Claude
// models. Claude tokenizes text roughly 15% more than cl100k_base; this is an
// empirical value derived from comparison with the API's context_usage signal.
const ClaudeCorrectionFactor = 1.15

// imageBlockTokens is the fixed estimate charged for an image content block, to
// avoid badly undercounting multimodal input.
const imageBlockTokens = 100

// CountTokens returns the approximate token count of text, with the Claude
// correction factor applied.
func CountTokens(text string) int {
	return CountTokensWithCorrection(text, true)
}

// CountTokensWithCorrection returns the approximate token count of text. When
// applyClaudeCorrection is true the result is scaled by ClaudeCorrectionFactor.
func CountTokensWithCorrection(text string, applyClaudeCorrection bool) int {
	return countTokens(text, applyClaudeCorrection)
}

// countTokens is the character-based heuristic: ~4 characters per token, with a
// minimum of one token for any non-empty string. "characters" means Unicode
// code points, so multi-byte text is counted by code point rather than by byte.
func countTokens(text string, applyClaudeCorrection bool) int {
	if text == "" {
		return 0
	}
	base := utf8.RuneCountInString(text)/4 + 1
	return withCorrection(base, applyClaudeCorrection)
}

// withCorrection optionally scales n by ClaudeCorrectionFactor, truncating
// toward zero.
func withCorrection(n int, applyClaudeCorrection bool) int {
	if applyClaudeCorrection {
		return int(float64(n) * ClaudeCorrectionFactor)
	}
	return n
}

// CountMessageTokens estimates the tokens in a list of chat messages, accounting
// for OpenAI/Anthropic message structure (roles, delimiters, multi-type content
// blocks, tool calls, and tool results). Per-piece counts are computed without
// correction; the factor is applied once to the total when requested.
func CountMessageTokens(messages []map[string]any, applyClaudeCorrection bool) int {
	if len(messages) == 0 {
		return 0
	}

	total := 0
	for _, message := range messages {
		// Service tokens (role, delimiters) per message.
		total += 4
		total += countTokens(asString(message["role"]), false)

		switch content := message["content"].(type) {
		case string:
			total += countTokens(content, false)
		case []any:
			for _, raw := range content {
				total += countContentBlock(raw)
			}
		}

		if toolCalls, ok := message["tool_calls"].([]any); ok {
			for _, raw := range toolCalls {
				tc, _ := raw.(map[string]any)
				total += 4 // service tokens
				fn := asMap(tc["function"])
				total += countTokens(asString(fn["name"]), false)
				total += countTokens(asString(fn["arguments"]), false)
			}
		}

		if id := asString(message["tool_call_id"]); id != "" {
			total += countTokens(id, false)
		}
	}

	// Final service tokens.
	total += 3
	return withCorrection(total, applyClaudeCorrection)
}

// countContentBlock estimates the (uncorrected) tokens of a single content block
// from a message's content list.
func countContentBlock(raw any) int {
	item, ok := raw.(map[string]any)
	if !ok {
		return countTokens(pyStr(raw), false)
	}

	switch asString(item["type"]) {
	case "text":
		return countTokens(asString(item["text"]), false)
	case "image_url", "image":
		return imageBlockTokens
	case "tool_use":
		total := countTokens(asString(item["id"]), false)
		total += countTokens(asString(item["name"]), false)
		input, present := item["input"]
		if !present {
			input = map[string]any{} // missing input defaults to {}
		}
		total += pyJSONTokens(input)
		return total
	case "tool_result":
		total := countTokens(asString(item["tool_use_id"]), false)
		if isErr := item["is_error"]; isErr != nil {
			total += countTokens(pyStr(isErr), false)
		}
		switch trc := item["content"].(type) {
		case string:
			total += countTokens(trc, false)
		case []any:
			for _, rb := range trc {
				block, ok := rb.(map[string]any)
				if !ok {
					total += countTokens(pyStr(rb), false)
					continue
				}
				switch asString(block["type"]) {
				case "text":
					total += countTokens(asString(block["text"]), false)
				case "image_url", "image":
					total += imageBlockTokens
				}
			}
		case nil:
			// No tool_result content; nothing to add.
		default:
			total += countTokens(pyStr(trc), false)
		}
		return total
	default:
		// Unknown block: estimate via JSON to avoid undercounting.
		return pyJSONTokens(item)
	}
}

// CountToolsTokens estimates the tokens in a list of tool definitions. It
// supports both OpenAI standard tools ({"type":"function","function":{...}}) and
// Anthropic/flat tools, and both the input_schema and parameters schema keys.
func CountToolsTokens(tools []map[string]any, applyClaudeCorrection bool) int {
	if len(tools) == 0 {
		return 0
	}

	total := 0
	for _, tool := range tools {
		total += 4 // service tokens

		payload := tool
		if asString(tool["type"]) == "function" {
			if fn := asMap(tool["function"]); fn != nil {
				payload = fn
			}
		}

		total += countTokens(asString(payload["name"]), false)
		total += countTokens(asString(payload["description"]), false)

		params := payload["input_schema"] // Anthropic
		if params == nil {
			params = payload["parameters"] // OpenAI
		}
		if params != nil {
			total += pyJSONTokens(params)
		}
	}

	return withCorrection(total, applyClaudeCorrection)
}

// CountSystemTokens estimates the tokens in a system prompt, which may be a plain
// string or an Anthropic list of content blocks (with optional cache_control).
func CountSystemTokens(systemPrompt any, applyClaudeCorrection bool) int {
	total := 0
	switch sp := systemPrompt.(type) {
	case nil:
		return 0
	case string:
		if sp == "" {
			return 0
		}
		total += countTokens(sp, false)
	case []any:
		if len(sp) == 0 {
			return 0
		}
		for _, raw := range sp {
			if block, ok := raw.(map[string]any); ok {
				total += countTokens(asString(block["text"]), false)
				if cc := block["cache_control"]; cc != nil {
					total += pyJSONTokens(cc)
				}
			} else {
				total += countTokens(pyStr(raw), false)
			}
		}
	default:
		total += countTokens(pyStr(sp), false)
	}

	return withCorrection(total, applyClaudeCorrection)
}

// RequestTokens is the breakdown returned by EstimateRequestTokens.
type RequestTokens struct {
	MessagesTokens int
	ToolsTokens    int
	SystemTokens   int
	TotalTokens    int
}

// EstimateRequestTokens estimates the total prompt tokens of a request, broken
// down into messages, tools, and system prompt. tools and systemPrompt may be
// nil.
func EstimateRequestTokens(messages []map[string]any, tools []map[string]any, systemPrompt any, applyClaudeCorrection bool) RequestTokens {
	messagesTokens := CountMessageTokens(messages, applyClaudeCorrection)
	toolsTokens := CountToolsTokens(tools, applyClaudeCorrection)
	systemTokens := CountSystemTokens(systemPrompt, applyClaudeCorrection)
	return RequestTokens{
		MessagesTokens: messagesTokens,
		ToolsTokens:    toolsTokens,
		SystemTokens:   systemTokens,
		TotalTokens:    messagesTokens + toolsTokens + systemTokens,
	}
}

// CalculateTokensFromContextUsage derives prompt and total token counts from the
// context-usage percentage reported by the Kiro API.
//
// When contextUsagePct is positive, the total is (pct/100)*contextLimit and the
// prompt is total minus completionTokens (clamped at zero); the sources are
// "subtraction" and "API Kiro". Otherwise it returns (0, completionTokens) with
// sources "unknown" and "tiktoken", signalling the caller to fall back to
// heuristic estimation.
func CalculateTokensFromContextUsage(contextUsagePct float64, completionTokens, contextLimit int) (promptTokens, totalTokens int, promptSource, totalSource string) {
	if contextUsagePct > 0 {
		totalTokens = int((contextUsagePct / 100) * float64(contextLimit))
		promptTokens = totalTokens - completionTokens
		if promptTokens < 0 {
			promptTokens = 0
		}
		return promptTokens, totalTokens, "subtraction", "API Kiro"
	}
	return 0, completionTokens, "unknown", "tiktoken"
}

// --- helpers ---

// asString returns v as a string, or "" if v is missing or not a string.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asMap returns v as a map, or nil if v is missing or not a map.
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// pyStr stringifies the rare non-standard values that need a text form. The
// exact representation only feeds the character-count heuristic, so close-enough
// lengths are sufficient.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	default:
		return pyJSON(v)
	}
}

// pyJSON serializes v with non-ASCII left as UTF-8 (not \u-escaped), HTML
// metacharacters unescaped, and ", " / ": " separators. This separator spacing
// keeps character counts — and therefore token estimates — stable. Map keys are
// sorted, but that does not change the length.
func pyJSON(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return spaceSeparators(strings.TrimRight(sb.String(), "\n"))
}

// pyJSONTokens returns countTokens(pyJSON(v), false) without materializing the
// space-separated string. pyJSON would insert exactly one space after every
// structural ','/':' outside a string literal, so the counted length is the
// compact JSON's rune count plus one per such separator. This is the hot path
// (called per content block / per tool), so it avoids spaceSeparators' second
// full-string allocation.
func pyJSONTokens(v any) int {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0
	}
	s := strings.TrimRight(sb.String(), "\n")
	runes := utf8.RuneCountInString(s)
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ',', ':':
			runes++ // pyJSON inserts a single space here
		}
	}
	if runes == 0 {
		return 0
	}
	return runes/4 + 1
}

// spaceSeparators rewrites compact JSON to use ", " and ": " separators,
// inserting a space after every structural ',' and ':' that lies outside a
// string literal.
func spaceSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		b.WriteByte(c)
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ',', ':':
			b.WriteByte(' ')
		}
	}
	return b.String()
}
