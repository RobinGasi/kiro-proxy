package converter

import (
	"fmt"
	"hash/fnv"
	"log/slog"
)

// KiroMaxToolUseIDLen is the maximum toolUseId length the Kiro / CodeWhisperer
// API accepts.
const KiroMaxToolUseIDLen = 64

// toolUseIDHashLen is the number of hex characters of the id digest appended to
// a sanitized toolUseId so that distinct originals do not collide.
const toolUseIDHashLen = 8

// SanitizeToolUseID returns id unchanged when it already satisfies Kiro's
// toolUseId contract — 1 to 64 bytes drawn from [A-Za-z0-9_-] — and otherwise a
// deterministic, conforming alias of the form "<cleaned-prefix>_<hash>".
//
// Kiro rejects a non-conforming toolUseId with a flat HTTP 400 "Invalid tool use
// format.", which names neither the offending id nor the rule it broke. Clients
// do produce such ids: a resumed or repaired conversation can carry ids that
// embed a newline (two provider ids spliced together) or run past 64 bytes, and
// the proxy previously forwarded them verbatim, so a single poisoned history
// entry failed every subsequent request.
//
// Every invalid byte becomes '_' and a digest of the *original* id is appended,
// so two ids that differ only in invalid bytes do not collapse onto each other.
// The result is a pure function of id, which is what keeps a tool result paired
// with its tool use: both sides are sanitized independently at the Kiro
// boundary and land on the same value.
//
// An empty id is returned unchanged; absence is the caller's concern, and
// minting an id here would invent a pairing that the client never expressed.
func SanitizeToolUseID(id string) string {
	if id == "" || validToolUseID(id) {
		return id
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	hash := fmt.Sprintf("%016x", h.Sum64())[:toolUseIDHashLen]

	// Byte-wise substitution keeps the result pure ASCII, so (unlike a
	// truncated tool *name*) no partial-rune repair is needed.
	cleaned := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		if allowedToolUseIDByte(id[i]) {
			cleaned = append(cleaned, id[i])
			continue
		}
		cleaned = append(cleaned, '_')
	}

	// Reserve room for the '_' separator plus the hash suffix.
	if maxPrefix := KiroMaxToolUseIDLen - toolUseIDHashLen - 1; len(cleaned) > maxPrefix {
		cleaned = cleaned[:maxPrefix]
	}
	alias := string(cleaned) + "_" + hash

	slog.Debug("sanitized non-conforming toolUseId for Kiro",
		"original", id, "alias", alias, "original_len", len(id))
	return alias
}

// validToolUseID reports whether id already satisfies Kiro's toolUseId contract.
func validToolUseID(id string) bool {
	if len(id) == 0 || len(id) > KiroMaxToolUseIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !allowedToolUseIDByte(id[i]) {
			return false
		}
	}
	return true
}

func allowedToolUseIDByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '-':
		return true
	default:
		return false
	}
}
