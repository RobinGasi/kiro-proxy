package main

import (
	"log/slog"
	"net/http"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/util"
)

// requestLogger logs two INFO lines per request: "request start" the moment it
// arrives, and "request" when the handler returns (with status and duration).
// Neither touches the request/response body, so no prompt, message or
// completion content is ever recorded. Both lines carry the same short req_id,
// so a long-lived SSE stream can be matched back to its arrival. Health probes
// are skipped to keep the log quiet. Set DEBUG_MODE=all for full
// request/response artifacts instead.
func requestLogger(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		reqID := util.UUIDHex()[:8]
		log.Info("request start",
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Info("request",
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusWriter wraps http.ResponseWriter to capture the response status code
// while transparently forwarding Flush so SSE streaming still works. The status
// defaults to 200 (set in requestLogger) for handlers that write a body without
// an explicit WriteHeader.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer when it supports flushing (required
// for Server-Sent Events).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
