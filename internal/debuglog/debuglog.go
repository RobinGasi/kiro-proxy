// Package debuglog writes per-request debug artifacts (the client request body,
// the transformed Kiro request body, the raw and modified response streams, an
// error_info.json, and captured application logs) into config.DebugDir.
//
// Behavior is driven by config.DebugMode:
//
//   - off:     nothing is written.
//   - errors:  data is buffered in memory and flushed to files only when the
//     request fails (see Request.FlushOnError).
//   - all:     the debug directory is reset at the start of every request and
//     data is written to files immediately as it is recorded.
//
// Rather than a process-global singleton with shared buffers (which is only
// correct for one in-flight request at a time), this keeps simple on-disk
// semantics (a single shared debug directory) but isolates per-request state in
// a Request value so concurrent requests cannot race; filesystem operations are
// serialized through the Logger mutex.
//
// Application-log capture replaces loguru's temporary sink with a slog.Handler
// (see NewHandler). Wrap the program's base handler once at startup, e.g.:
//
//	slog.SetDefault(slog.New(debuglog.NewHandler(baseHandler)))
//
// Begin then attaches a per-request capture buffer to the returned context;
// any slog records emitted with that context (via the *Context methods, or a
// logger carrying it) are mirrored into app_logs.txt when the request is
// finalized.
package debuglog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
)

// Debug modes, mirroring config.DebugMode.
const (
	ModeOff    = "off"
	ModeErrors = "errors"
	ModeAll    = "all"
)

// Debug artifact filenames.
const (
	fileRequestBody     = "request_body.json"
	fileKiroRequestBody = "kiro_request_body.json"
	fileRawStream       = "response_stream_raw.txt"
	fileModifiedStream  = "response_stream_modified.txt"
	fileErrorInfo       = "error_info.json"
	fileAppLogs         = "app_logs.txt"
)

const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// Logger manages debug request logs for a single debug directory. It is safe
// for concurrent use. Construct it with New.
type Logger struct {
	mode string
	dir  string

	// mu serializes all filesystem operations against the shared dir so that
	// concurrent requests cannot corrupt files or interleave directory resets.
	mu sync.Mutex
}

// New returns a Logger configured from cfg.DebugMode and cfg.DebugDir.
func New(cfg *config.Config) *Logger {
	return &Logger{mode: cfg.DebugMode, dir: cfg.DebugDir}
}

func (l *Logger) enabled() bool   { return l.mode == ModeErrors || l.mode == ModeAll }
func (l *Logger) immediate() bool { return l.mode == ModeAll }

// Begin starts debug capture for a new request. It returns a context that
// carries the per-request application-log capture buffer (used by handlers
// created via NewHandler) and a Request handle the middleware drives.
//
// In "all" mode the debug directory is reset here so that only the current
// request's artifacts remain. When debugging is disabled the returned Request
// is an inert no-op and the context is returned unchanged.
func (l *Logger) Begin(ctx context.Context) (context.Context, *Request) {
	r := &Request{logger: l}
	if !l.enabled() {
		return ctx, r
	}

	// Set up application-log capture for this request.
	r.target = &captureTarget{buf: &bytes.Buffer{}}
	ctx = context.WithValue(ctx, captureKey{}, r.target)

	if l.immediate() {
		if err := l.resetDir(); err != nil {
			slog.Error("debuglog: failed to reset debug directory", "dir", l.dir, "err", err)
		} else {
			slog.Debug("debuglog: debug directory cleared for new request", "dir", l.dir)
		}
	}
	return ctx, r
}

// Request holds the debug state for a single in-flight request. Obtain one from
// Logger.Begin. All methods are safe for concurrent use and are no-ops when
// debugging is disabled.
type Request struct {
	logger *Logger
	target *captureTarget // app-log capture buffer; nil when disabled

	mu              sync.Mutex
	requestBody     []byte
	kiroRequestBody []byte
	rawChunks       bytes.Buffer
	modifiedChunks  bytes.Buffer
}

// LogRequestBody records the client request body (OpenAI/Anthropic format).
func (r *Request) LogRequestBody(body []byte) {
	if !r.logger.enabled() {
		return
	}
	if r.logger.immediate() {
		r.logger.writeJSONOrRaw(fileRequestBody, body)
		return
	}
	r.mu.Lock()
	r.requestBody = cloneBytes(body)
	r.mu.Unlock()
}

// LogKiroRequestBody records the transformed request body sent to the Kiro API.
func (r *Request) LogKiroRequestBody(body []byte) {
	if !r.logger.enabled() {
		return
	}
	if r.logger.immediate() {
		r.logger.writeJSONOrRaw(fileKiroRequestBody, body)
		return
	}
	r.mu.Lock()
	r.kiroRequestBody = cloneBytes(body)
	r.mu.Unlock()
}

// LogRawChunk appends a raw response chunk received from the provider.
func (r *Request) LogRawChunk(chunk []byte) {
	if !r.logger.enabled() {
		return
	}
	if r.logger.immediate() {
		r.logger.appendFile(fileRawStream, chunk)
		return
	}
	r.mu.Lock()
	r.rawChunks.Write(chunk)
	r.mu.Unlock()
}

// LogModifiedChunk appends a chunk after transformation, as sent to the client.
func (r *Request) LogModifiedChunk(chunk []byte) {
	if !r.logger.enabled() {
		return
	}
	if r.logger.immediate() {
		r.logger.appendFile(fileModifiedStream, chunk)
		return
	}
	r.mu.Lock()
	r.modifiedChunks.Write(chunk)
	r.mu.Unlock()
}

// FlushOnError finalizes a failed request. In "all" mode the artifacts were
// already written, so it only adds error_info.json and the captured app logs.
// In "errors" mode it resets the directory and flushes every buffered artifact.
func (r *Request) FlushOnError(statusCode int, errMsg string) {
	l := r.logger
	if !l.enabled() {
		return
	}

	if l.immediate() {
		l.writeErrorInfo(statusCode, errMsg)
		r.flushAppLogs()
		return
	}

	// "errors" mode: snapshot the buffers under the lock.
	r.mu.Lock()
	reqBody := r.requestBody
	kiroBody := r.kiroRequestBody
	raw := cloneBytes(r.rawChunks.Bytes())
	modified := cloneBytes(r.modifiedChunks.Bytes())
	r.mu.Unlock()

	// If nothing was buffered, write nothing at all (not even error_info or app
	// logs).
	if len(reqBody) == 0 && len(kiroBody) == 0 && len(raw) == 0 && len(modified) == 0 {
		return
	}

	defer r.clearBuffers()

	if err := l.resetDir(); err != nil {
		slog.Error("debuglog: failed to reset debug directory for flush", "dir", l.dir, "err", err)
		return
	}

	if len(reqBody) > 0 {
		l.writeJSONOrRaw(fileRequestBody, reqBody)
	}
	if len(kiroBody) > 0 {
		l.writeJSONOrRaw(fileKiroRequestBody, kiroBody)
	}
	if len(raw) > 0 {
		l.writeFile(fileRawStream, raw)
	}
	if len(modified) > 0 {
		l.writeFile(fileModifiedStream, modified)
	}

	l.writeErrorInfo(statusCode, errMsg)
	r.flushAppLogs()

	slog.Info("debuglog: error logs flushed", "dir", l.dir, "status", statusCode)
}

// Discard finalizes a successful request. In "errors" mode the buffers are
// dropped without touching the disk. In "all" mode the captured app logs are
// written so a successful request's logs are preserved alongside its artifacts.
func (r *Request) Discard() {
	switch r.logger.mode {
	case ModeErrors:
		r.clearBuffers()
	case ModeAll:
		r.flushAppLogs()
	}
}

func (r *Request) clearBuffers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requestBody = nil
	r.kiroRequestBody = nil
	r.rawChunks.Reset()
	r.modifiedChunks.Reset()
}

// flushAppLogs writes the captured application logs to app_logs.txt, if any.
func (r *Request) flushAppLogs() {
	if r.target == nil {
		return
	}
	r.target.mu.Lock()
	content := r.target.buf.String()
	r.target.mu.Unlock()

	if strings.TrimSpace(content) == "" {
		return
	}
	r.logger.writeAppLogs(content)
}

// ==================== filesystem helpers (Logger) ====================

// resetDir removes and recreates the debug directory.
func (l *Logger) resetDir() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.RemoveAll(l.dir); err != nil {
		return err
	}
	return os.MkdirAll(l.dir, dirPerm)
}

// writeJSONOrRaw pretty-prints body as JSON when it is valid JSON, otherwise
// writes the raw bytes verbatim.
func (l *Logger) writeJSONOrRaw(name string, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, dirPerm); err != nil {
		slog.Error("debuglog: failed to create debug directory", "dir", l.dir, "err", err)
		return
	}
	path := filepath.Join(l.dir, name)

	if json.Valid(body) {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err == nil {
			if err := os.WriteFile(path, pretty.Bytes(), filePerm); err != nil {
				slog.Error("debuglog: failed to write debug file", "file", name, "err", err)
			}
			return
		}
	}
	if err := os.WriteFile(path, body, filePerm); err != nil {
		slog.Error("debuglog: failed to write debug file", "file", name, "err", err)
	}
}

// writeFile overwrites name with the given bytes.
func (l *Logger) writeFile(name string, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, dirPerm); err != nil {
		slog.Error("debuglog: failed to create debug directory", "dir", l.dir, "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(l.dir, name), data, filePerm); err != nil {
		slog.Error("debuglog: failed to write debug file", "file", name, "err", err)
	}
}

// appendFile appends data to name, creating it if necessary. Errors are
// swallowed to keep streaming writes best-effort.
func (l *Logger) appendFile(name string, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, dirPerm); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(data)
}

// errorInfo is the JSON shape written to error_info.json.
type errorInfo struct {
	StatusCode   int    `json:"status_code"`
	ErrorMessage string `json:"error_message"`
}

// writeErrorInfo writes error_info.json. JSON is emitted without HTML escaping
// so literal characters are preserved.
func (l *Logger) writeErrorInfo(statusCode int, errMsg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, dirPerm); err != nil {
		slog.Error("debuglog: failed to create debug directory", "dir", l.dir, "err", err)
		return
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(errorInfo{StatusCode: statusCode, ErrorMessage: errMsg}); err != nil {
		slog.Error("debuglog: failed to encode error_info", "err", err)
		return
	}
	// json.Encoder appends a trailing newline; drop it to match json.dump.
	data := bytes.TrimRight(buf.Bytes(), "\n")
	if err := os.WriteFile(filepath.Join(l.dir, fileErrorInfo), data, filePerm); err != nil {
		slog.Error("debuglog: failed to write error_info", "err", err)
	}
}

// writeAppLogs writes captured application logs. It deliberately avoids slog to
// prevent the capture handler from feeding logs back into the buffer.
func (l *Logger) writeAppLogs(content string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, dirPerm); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(l.dir, fileAppLogs), []byte(content), filePerm)
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

// ==================== application-log capture (slog) ====================

// captureKey is the unexported context key under which a request's app-log
// capture target is stored.
type captureKey struct{}

// captureTarget is a synchronized in-memory sink for captured app logs.
type captureTarget struct {
	mu  sync.Mutex
	buf *bytes.Buffer
	// sinks memoizes the composed text handler per captureHandler instance, so
	// the per-record sink (NewTextHandler + replayed WithAttrs/WithGroup) is
	// built once per (request, handler) rather than on every log line. The
	// target is per-request and short-lived, so the cache cannot leak.
	sinks map[*captureHandler]slog.Handler
}

func captureFrom(ctx context.Context) *captureTarget {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(captureKey{}).(*captureTarget)
	return t
}

// captureHandler is an slog.Handler that forwards every record to a base
// handler and, when the record's context carries a capture target (installed by
// Logger.Begin), additionally writes a formatted line to that target's buffer.
type captureHandler struct {
	base slog.Handler
	// mods replays WithAttrs/WithGroup onto the per-record capture sink so that
	// captured lines carry the same attributes as the base output.
	mods []func(slog.Handler) slog.Handler
}

// NewHandler wraps base so that records emitted with a context produced by
// Logger.Begin are mirrored into that request's app_logs.txt. base must be
// non-nil and continues to receive every record it would normally handle.
func NewHandler(base slog.Handler) slog.Handler {
	return &captureHandler{base: base}
}

// Enabled reports whether a record at level should be processed. When a capture
// target is present, DEBUG-and-above records are processed so all logs can be
// captured even if the base handler's level is higher; otherwise it defers to
// the base handler.
func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= slog.LevelDebug && captureFrom(ctx) != nil {
		return true
	}
	return h.base.Enabled(ctx, level)
}

// Handle forwards the record to the base handler (only when the base would
// actually emit it) and mirrors it into the capture target when present.
func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	if h.base.Enabled(ctx, r.Level) {
		if err := h.base.Handle(ctx, r); err != nil {
			firstErr = err
		}
	}

	if t := captureFrom(ctx); t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		sink := t.sinks[h] // nil-map read is safe
		if sink == nil {
			sink = slog.NewTextHandler(t.buf, &slog.HandlerOptions{
				Level:     slog.LevelDebug,
				AddSource: true,
			})
			for _, m := range h.mods {
				sink = m(sink)
			}
			if t.sinks == nil {
				t.sinks = make(map[*captureHandler]slog.Handler)
			}
			t.sinks[h] = sink
		}
		if err := sink.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WithAttrs returns a handler that applies attrs to both the base handler and
// the capture sink.
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	mods := make([]func(slog.Handler) slog.Handler, len(h.mods), len(h.mods)+1)
	copy(mods, h.mods)
	mods = append(mods, func(s slog.Handler) slog.Handler { return s.WithAttrs(attrs) })
	return &captureHandler{base: h.base.WithAttrs(attrs), mods: mods}
}

// WithGroup returns a handler that applies the group to both the base handler
// and the capture sink.
func (h *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	mods := make([]func(slog.Handler) slog.Handler, len(h.mods), len(h.mods)+1)
	copy(mods, h.mods)
	mods = append(mods, func(s slog.Handler) slog.Handler { return s.WithGroup(name) })
	return &captureHandler{base: h.base.WithGroup(name), mods: mods}
}
