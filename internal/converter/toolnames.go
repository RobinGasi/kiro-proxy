package converter

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"unicode/utf8"
)

// KiroMaxToolNameLen is the maximum tool-name length the Kiro / CodeWhisperer
// API accepts. Names longer than this are rejected upstream (HTTP 400), so the
// proxy transparently rewrites them to a deterministic alias before sending and
// restores the original name in the response. This most commonly bites MCP
// tools, whose auto-generated names ("mcp__<server>__<tool>") easily exceed 64
// characters.
const KiroMaxToolNameLen = 64

// toolNameHashLen is the number of hex characters of the name digest appended to
// a shortened tool name so that distinct originals do not collide.
const toolNameHashLen = 8

// ShortenToolName returns name unchanged when it fits within Kiro's tool-name
// limit, and otherwise a deterministic, ≤64-character alias of the form
// "<truncated-prefix>_<hash>", where <hash> is derived from the full original
// name. The alias is a pure function of name (independent of the rest of the
// tool list), so the same original always yields the same alias.
func ShortenToolName(name string) string {
	if len(name) <= KiroMaxToolNameLen {
		return name
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	hash := fmt.Sprintf("%016x", h.Sum64())[:toolNameHashLen]

	// Reserve room for the '_' separator plus the hash suffix, then back the prefix
	// off any partial trailing rune so a multi-byte name never leaves an invalid
	// UTF-8 fragment in the alias (JSON would rewrite it to U+FFFD, which would then
	// fail to match the reverse-map key on the response path).
	prefixLen := KiroMaxToolNameLen - toolNameHashLen - 1
	prefix := name[:prefixLen]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	alias := prefix + "_" + hash

	slog.Debug("shortened over-long tool name for Kiro",
		"original", name, "alias", alias, "original_len", len(name))
	return alias
}

// RestoreToolName maps a Kiro tool alias back to its original name using a
// reverse map produced by [aliasTools] (and returned from [BuildKiroPayload]).
// Names absent from the map (including when the map is nil) are returned
// unchanged.
func RestoreToolName(reverse map[string]string, name string) string {
	if orig, ok := reverse[name]; ok {
		return orig
	}
	return name
}

// aliasTools renames every over-long tool name to its Kiro alias and returns the
// aliased tools together with an alias→original reverse map for restoring names
// on the response path. The input slice and its elements are not mutated: the
// original slice is returned (with a nil map) when no name needs shortening.
func aliasTools(tools []Tool) ([]Tool, map[string]string) {
	var out []Tool
	var reverse map[string]string
	for i, t := range tools {
		alias := ShortenToolName(t.Name)
		if alias == t.Name {
			continue
		}
		if out == nil {
			out = make([]Tool, len(tools))
			copy(out, tools)
			reverse = make(map[string]string)
		}
		out[i].Name = alias
		reverse[alias] = t.Name
	}
	if out == nil {
		return tools, nil
	}
	return out, reverse
}

// aliasMessageToolNames returns messages with every assistant tool-call name and
// tool_use content-part name replaced by its Kiro alias, so that history tool
// uses match the (also aliased) tool definitions. Caller data is not mutated:
// slices are cloned lazily on first change, and the input slice is returned
// unchanged when nothing needs aliasing.
func aliasMessageToolNames(messages []Message) []Message {
	var out []Message
	for i, m := range messages {
		newTCs := aliasToolCalls(m.ToolCalls)
		newContent := aliasToolUseParts(m.Content)
		if newTCs == nil && newContent == nil {
			continue
		}
		if out == nil {
			out = make([]Message, len(messages))
			copy(out, messages)
		}
		if newTCs != nil {
			out[i].ToolCalls = newTCs
		}
		if newContent != nil {
			out[i].Content = newContent
		}
	}
	if out == nil {
		return messages
	}
	return out
}

// aliasToolCalls returns a copy of calls with over-long names aliased, or nil
// when no name changes (so callers can keep the original slice).
func aliasToolCalls(calls []ToolCall) []ToolCall {
	var out []ToolCall
	for i, tc := range calls {
		alias := ShortenToolName(tc.Function.Name)
		if alias == tc.Function.Name {
			continue
		}
		if out == nil {
			out = make([]ToolCall, len(calls))
			copy(out, calls)
		}
		out[i].Function.Name = alias
	}
	return out
}

// aliasToolUseParts returns a copy of parts with over-long tool_use names
// aliased, or nil when no name changes.
func aliasToolUseParts(parts []ContentPart) []ContentPart {
	var out []ContentPart
	for i, p := range parts {
		if p.Type != "tool_use" {
			continue
		}
		alias := ShortenToolName(p.Name)
		if alias == p.Name {
			continue
		}
		if out == nil {
			out = make([]ContentPart, len(parts))
			copy(out, parts)
		}
		out[i].Name = alias
	}
	return out
}
