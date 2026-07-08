// Package apiauth holds the shared proxy-key authentication used by both the
// OpenAI and Anthropic route handlers.
package apiauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// CheckKey reports whether the request carries a valid proxy API key. Clients
// may present it either OpenAI-style (Authorization: Bearer <key>) or
// Anthropic-style (x-api-key: <key>). Comparison is constant-time. An empty
// keys slice denies all requests.
func CheckKey(r *http.Request, keys []string) bool {
	presented := extractKey(r)
	if presented == "" {
		return false
	}
	for _, k := range keys {
		if k != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

func extractKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		return v
	}
	return ""
}
