// Package util holds common helpers: machine fingerprint, ID generation, and
// Kiro request header construction.
package util

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
)

type requestIDKey struct{}

// WithRequestID attaches the proxy's safe, short request correlation ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the proxy request correlation ID, or an empty string when
// the code is running outside an HTTP request.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// UpstreamRequestID extracts AWS's safe response correlation ID.
func UpstreamRequestID(h http.Header) string {
	for _, name := range []string{"x-amzn-requestid", "x-amz-request-id", "x-amzn-request-id"} {
		if id := h.Get(name); id != "" {
			return id
		}
	}
	return ""
}

// MachineFingerprint returns a stable per-installation fingerprint, the SHA256
// of "{hostname}-{username}-kiro-proxy".
func MachineFingerprint() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	username := "unknown-user"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}
	sum := sha256.Sum256([]byte(hostname + "-" + username + "-kiro-proxy"))
	return hex.EncodeToString(sum[:])
}

// UUID4 returns a random RFC-4122 v4 UUID string.
func UUID4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UUIDHex returns 32 hex chars (a UUID4 with dashes removed).
func UUIDHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// GenerateCompletionID returns an OpenAI-style "chatcmpl-..." id.
func GenerateCompletionID() string { return "chatcmpl-" + UUIDHex() }

// GenerateToolCallID returns a "call_xxxxxxxx" id.
func GenerateToolCallID() string { return "call_" + UUIDHex()[:8] }

// GenerateMessageID returns an Anthropic-style "msg_..." id.
func GenerateMessageID() string { return "msg_" + UUIDHex() }

// KiroHeaders builds the headers for a Kiro GenerateAssistantResponse request.
// clientVersion is the (configurable) spoofed Kiro IDE version.
func KiroHeaders(token, fingerprint, clientVersion string) map[string]string {
	ua := fmt.Sprintf(
		"aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-%s-%s",
		clientVersion, fingerprint,
	)
	return map[string]string{
		"Authorization":               "Bearer " + token,
		"Content-Type":                "application/x-amz-json-1.0",
		"x-amz-target":                "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		"User-Agent":                  ua,
		"x-amz-user-agent":            fmt.Sprintf("aws-sdk-js/1.0.27 KiroIDE-%s-%s", clientVersion, fingerprint),
		"x-amzn-codewhisperer-optout": "true",
		"x-amzn-kiro-agent-mode":      "vibe",
		"amz-sdk-invocation-id":       UUID4(),
		"amz-sdk-request":             "attempt=1; max=3",
	}
}

// GenerateConversationID returns a stable 16-char hex id derived from the first
// three and last messages, or a random UUID when no messages are given.
func GenerateConversationID(messages []map[string]any) string {
	if len(messages) == 0 {
		return UUID4()
	}
	var key []map[string]any
	if len(messages) <= 3 {
		key = messages
	} else {
		key = append(append([]map[string]any{}, messages[:3]...), messages[len(messages)-1])
	}
	simplified := make([]map[string]string, 0, len(key))
	for _, m := range key {
		role, _ := m["role"].(string)
		if role == "" {
			role = "unknown"
		}
		var contentStr string
		switch c := m["content"].(type) {
		case string:
			contentStr = Truncate(c, 100)
		case nil:
			contentStr = ""
		default:
			b, _ := json.Marshal(c)
			contentStr = Truncate(string(b), 100)
		}
		simplified = append(simplified, map[string]string{"role": role, "content": contentStr})
	}
	b, _ := json.Marshal(simplified)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Truncate shortens s to at most n bytes (useful for logging/hashing previews).
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
