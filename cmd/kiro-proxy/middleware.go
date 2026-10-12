package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/debuglog"
	"github.com/RobinGasi/kiro-proxy/internal/util"
)

// errMsgCaptureBytes caps how much of a failed response body is copied into
// error_info.json. Error envelopes are small; the full body is always available
// in response_stream_modified.txt.
const errMsgCaptureBytes = 8 << 10

// requestLogger logs two INFO lines per request: "request start" the moment it
// arrives, and "request" when the handler returns (with status and duration).
// Neither touches the request/response body, so no prompt, message or
// completion content is ever recorded. Both lines carry the same short req_id,
// so a long-lived SSE stream can be matched back to its arrival. Health probes
// are skipped to keep the log quiet.
//
// When dbg is enabled (DEBUG_MODE=errors|all) the same req_id names the
// request's debug directory, and this middleware is where the full artifacts are
// captured: request metadata, the verbatim client request body, and every byte
// written back to the client. The deeper layers add their own artifacts through
// debuglog.FromContext (the transformed Kiro payload, the raw upstream stream).
// On a 4xx/5xx the whole set is flushed via FlushOnError.
func requestLogger(next http.Handler, log *slog.Logger, dbg *debuglog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		reqID := util.UUIDHex()[:8]
		w.Header().Set("request-id", reqID)
		w.Header().Set("x-request-id", reqID)
		log.Info("request start",
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)

		ctx := util.WithRequestID(r.Context(), reqID)
		ctx, dreq := dbg.Begin(ctx, reqID)
		r = r.WithContext(ctx)

		var body *bodyCapture
		if dreq != nil {
			dreq.LogMeta(requestMeta(r, reqID))
			if r.Body != nil {
				body = &bodyCapture{ReadCloser: r.Body, dbg: dreq}
				r.Body = body
			}
			log.Debug("debug capture started", "req_id", reqID, "dir", dreq.Dir())
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK, dbg: dreq}
		next.ServeHTTP(sw, r)
		body.flush()

		if sw.status >= http.StatusBadRequest {
			dreq.FlushOnError(sw.status, sw.errSnippet())
		} else {
			dreq.Discard()
		}

		log.Info("request",
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// requestMeta describes the incoming request for request_meta.json. Authorization
// and API-key headers are redacted to their presence, never their value.
func requestMeta(r *http.Request, reqID string) map[string]any {
	headers := make(map[string]string, len(r.Header))
	for name, values := range r.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "X-Api-Key", "Api-Key", "Cookie":
			headers[name] = "[redacted]"
		default:
			headers[name] = values[0]
		}
	}
	return map[string]any{
		"req_id":      reqID,
		"received_at": time.Now().UTC().Format(time.RFC3339Nano),
		"method":      r.Method,
		"path":        r.URL.Path,
		"query":       r.URL.RawQuery,
		"proto":       r.Proto,
		"remote_addr": r.RemoteAddr,
		"headers":     headers,
	}
}

type bodyCapture struct {
	io.ReadCloser
	dbg *debuglog.Request
	buf bytes.Buffer
}

// Read records everything the handler reads. Recording on read (rather than
// buffering the body up front) keeps the streaming read path intact and costs
// nothing for a body the handler abandons early.
func (b *bodyCapture) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.buf.Write(p[:n])
	}
	if err != nil {
		// Terminal read: the body is as complete as it will get. Flushing here
		// (rather than only at the end of the request) is what makes the body
		// available while a long-lived SSE stream is still running.
		b.flush()
	}
	return n, err
}

func (b *bodyCapture) Close() error {
	b.flush()
	return b.ReadCloser.Close()
}

// flush records the buffered body, if any. It is idempotent and nil-safe, and is
// called both on the terminal read and once the handler has returned: a decoder
// that stops at the closing brace never sees a terminal read, and net/http
// closes its own body reference rather than this wrapper.
func (b *bodyCapture) flush() {
	if b == nil || b.buf.Len() == 0 {
		return
	}
	b.dbg.LogRequestBody(b.buf.Bytes())
	b.buf.Reset()
}

// statusWriter wraps http.ResponseWriter to capture the response status code
// while transparently forwarding Flush so SSE streaming still works. The status
// defaults to 200 (set in requestLogger) for handlers that write a body without
// an explicit WriteHeader. When dbg is non-nil every written byte is also
// recorded as the client-visible stream.
type statusWriter struct {
	http.ResponseWriter
	status int
	dbg    *debuglog.Request

	// errBuf holds the first errMsgCaptureBytes of the body, used as the
	// error_info.json message when the response turns out to be a failure.
	errBuf bytes.Buffer
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.dbg != nil {
		w.dbg.LogModifiedChunk(p[:n])
		if room := errMsgCaptureBytes - w.errBuf.Len(); room > 0 {
			w.errBuf.Write(p[:min(n, room)])
		}
	}
	return n, err
}

// errSnippet returns the captured head of the response body, used as the
// error message for a failed request.
func (w *statusWriter) errSnippet() string {
	return w.errBuf.String()
}

// Flush forwards to the underlying writer when it supports flushing (required
// for Server-Sent Events).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
