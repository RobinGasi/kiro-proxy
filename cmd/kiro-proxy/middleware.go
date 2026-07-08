package main

import (
	"log/slog"
	"net/http"
	"time"
)

// requestLogger logs one INFO line per request — method, path, status and
// duration — WITHOUT touching the request/response body, so no prompt, message
// or completion content is ever recorded. Health probes are skipped to keep the
// log quiet. Set DEBUG_MODE=all for full request/response artifacts instead.
func requestLogger(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusWriter wraps http.ResponseWriter to capture the response status code
// while transparently forwarding Flush so SSE streaming still works.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when it supports flushing (required
// for Server-Sent Events).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
