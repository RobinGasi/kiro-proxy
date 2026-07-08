package truncation

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
)

// ToolTruncationInfo records a truncated tool call. It is tracked by its stable
// tool-call ID, which survives across requests.
type ToolTruncationInfo struct {
	// ToolCallID is the stable ID of the truncated tool call.
	ToolCallID string
	// ToolName is the name of the tool that was called.
	ToolName string
	// TruncationInfo holds diagnostic details from the stream parser.
	TruncationInfo TruncationInfo
	// Timestamp is when the truncation was detected.
	Timestamp time.Time
}

// ContentTruncationInfo records truncated non-tool content. It is tracked by a
// hash of the truncated assistant message, which is stable across requests.
type ContentTruncationInfo struct {
	// MessageHash is the stable identifier derived from the content.
	MessageHash string
	// ContentPreview holds the first 200 runes of the content, for debugging.
	ContentPreview string
	// Timestamp is when the truncation was detected.
	Timestamp time.Time
}

// CacheStats reports the current sizes of the truncation caches.
type CacheStats struct {
	ToolTruncations    int
	ContentTruncations int
	Total              int
}

// Store is a thread-safe in-memory cache of truncation-recovery state.
//
// Entries persist until either retrieved (a successful Get* call removes the
// entry) or the process restarts. There is intentionally no TTL: if the user
// takes a long break, the truncation info must still be available on their next
// request.
type Store struct {
	mu      sync.Mutex
	tool    map[string]ToolTruncationInfo
	content map[string]ContentTruncationInfo
}

// NewStore returns an empty, ready-to-use Store.
func NewStore() *Store {
	return &Store{
		tool:    make(map[string]ToolTruncationInfo),
		content: make(map[string]ContentTruncationInfo),
	}
}

// SaveToolTruncation records truncation info for a specific tool call.
func (s *Store) SaveToolTruncation(toolCallID, toolName string, info TruncationInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tool[toolCallID] = ToolTruncationInfo{
		ToolCallID:     toolCallID,
		ToolName:       toolName,
		TruncationInfo: info,
		Timestamp:      time.Now(),
	}
	slog.Debug("saved tool truncation", "tool_call_id", toolCallID, "tool", toolName)
}

// GetToolTruncation returns and removes the truncation info for a tool call.
// This is a one-time retrieval: a present entry is deleted. The boolean result
// reports whether an entry was found.
func (s *Store) GetToolTruncation(toolCallID string) (ToolTruncationInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.tool[toolCallID]
	if ok {
		delete(s.tool, toolCallID)
		slog.Debug("retrieved tool truncation", "tool_call_id", toolCallID)
	}
	return info, ok
}

// SaveContentTruncation records truncation info for non-tool content and returns
// the stable hash used to track it.
func (s *Store) SaveContentTruncation(content string) string {
	hash := contentHash(content)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content[hash] = ContentTruncationInfo{
		MessageHash:    hash,
		ContentPreview: firstRunes(content, 200),
		Timestamp:      time.Now(),
	}
	slog.Debug("saved content truncation", "hash", hash)
	return hash
}

// GetContentTruncation returns and removes the truncation info for the given
// content, looked up by its hash. This is a one-time retrieval: a present entry
// is deleted. The boolean result reports whether an entry was found.
func (s *Store) GetContentTruncation(content string) (ContentTruncationInfo, bool) {
	hash := contentHash(content)
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.content[hash]
	if ok {
		delete(s.content, hash)
		slog.Debug("retrieved content truncation", "hash", hash)
	}
	return info, ok
}

// CacheStats returns the current cache sizes.
func (s *Store) CacheStats() CacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := len(s.tool)
	c := len(s.content)
	return CacheStats{ToolTruncations: t, ContentTruncations: c, Total: t + c}
}

// contentHash returns the first 16 hex characters of the SHA-256 of the first
// 500 runes of content (slicing by code point before UTF-8 encoding).
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(firstRunes(content, 500)))
	return hex.EncodeToString(sum[:])[:16]
}

// firstRunes returns the first n runes of s, using code-point slicing rather
// than Go's default byte slicing.
func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
