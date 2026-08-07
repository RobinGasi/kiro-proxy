// Package debuglog writes per-request debug artifacts (request metadata, the
// client request body, the transformed Kiro request body, the raw upstream
// stream, the bytes sent back to the client, an error_info.json, and captured
// application logs) into config.DebugDir.
//
// Behavior is driven by config.DebugMode:
//
//   - off:     nothing is written.
//   - errors:  data is buffered in memory and flushed to files only when the
//     request fails (see Request.FlushOnError).
//   - all:     every request gets a directory and data is written to files
//     immediately as it is recorded.
//
// Each request writes into its own directory beneath config.DebugDir, named
// "<UTC timestamp>_<request id>", so concurrent requests cannot interleave and a
// failure can be compared against the requests that preceded it. Nothing is ever
// deleted; the directory grows until the operator prunes it, which is the point
// of a debug mode that is off by default.
//
// Application-log capture wraps the program's base slog handler (see NewHandler)
// once at startup:
//
//	slog.SetDefault(slog.New(debuglog.NewHandler(baseHandler)))
//
// Begin then attaches a per-request capture buffer to the returned context; any
// slog records emitted with that context are mirrored into app_logs.txt when the
// request is finalized. Begin also puts the *Request itself on the context, so
// layers below the middleware (the API handlers, the streaming driver) can
// record artifacts via FromContext without threading a parameter through every
// signature.
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
	"time"

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
	fileRequestMeta     = "request_meta.json"
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

type Logger struct {
	mode string
	root string

	// mu serializes directory creation and file writes so concurrent requests
	// cannot race on the shared root.
	mu sync.Mutex

	// globalMu guards lazy initialization of the process-wide app-log sink.
	globalMu     sync.Mutex
	globalFile   *os.File
	globalSink   slog.Handler
	globalFailed bool
}

// globalHandler returns the handler that appends every log record to
// <root>/app_logs.txt, opening the file on first use. It returns nil when
// debugging is off or the file cannot be opened (recorded once, never retried,
// so a broken debug dir does not slow the hot path down).
//
// This sink is what makes DEBUG_MODE a complete log: almost every call site in
// the codebase logs without a context, so per-request mirroring alone would miss
// them. Records land here regardless, at DEBUG level, correlated to a request by
// the req_id on the surrounding "request start"/"request" lines.
func (l *Logger) globalHandler() slog.Handler {
	if !l.enabled() {
		return nil
	}
	l.globalMu.Lock()
	defer l.globalMu.Unlock()
	if l.globalSink != nil || l.globalFailed {
		return l.globalSink
	}
	if err := os.MkdirAll(l.root, dirPerm); err != nil {
		l.globalFailed = true
		return nil
	}
	f, err := os.OpenFile(filepath.Join(l.root, fileAppLogs), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		l.globalFailed = true
		return nil
	}
	l.globalFile = f
	// AddSource pays for itself here: this file is read when something has
	// already gone wrong. Errors are swallowed rather than logged — logging a
	// log-sink failure would recurse.
	l.globalSink = slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug, AddSource: true})
	return l.globalSink
}

// Close releases the process-wide app-log file. It is safe to call on a nil
// Logger and safe to call more than once.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.globalMu.Lock()
	defer l.globalMu.Unlock()
	f := l.globalFile
	l.globalFile, l.globalSink = nil, nil
	if f == nil {
		return nil
	}
	return f.Close()
}

// New returns a Logger configured from cfg.DebugMode and cfg.DebugDir.
func New(cfg *config.Config) *Logger {
	return &Logger{mode: cfg.DebugMode, root: cfg.DebugDir}
}

// Mode reports the configured debug mode ("off", "errors" or "all").
func (l *Logger) Mode() string {
	if l == nil {
		return ModeOff
	}
	return l.mode
}

// Dir reports the debug root directory.
func (l *Logger) Dir() string {
	if l == nil {
		return ""
	}
	return l.root
}

func (l *Logger) enabled() bool   { return l != nil && (l.mode == ModeErrors || l.mode == ModeAll) }
func (l *Logger) immediate() bool { return l != nil && l.mode == ModeAll }

// requestKey is the unexported context key under which the in-flight *Request is
// stored.
type requestKey struct{}

// FromContext returns the *Request attached by Begin, or nil when the context
// carries none. Every *Request method is nil-safe, so callers can record
// artifacts unconditionally:
//
//	debuglog.FromContext(ctx).LogKiroRequestBody(payload)
func FromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(requestKey{}).(*Request)
	return r
}

// Begin starts debug capture for the request identified by reqID. It returns a
// context carrying both the per-request application-log capture buffer and the
// Request handle, plus the Request the middleware drives.
//
// In "all" mode the request's directory is created eagerly so that even a
// request that produces no artifacts leaves a trace. When debugging is disabled
// the returned Request is nil (an inert no-op receiver) and the context is
// returned unchanged.
func (l *Logger) Begin(ctx context.Context, reqID string) (context.Context, *Request) {
	if !l.enabled() {
		return ctx, nil
	}

	r := &Request{
		logger: l,
		dir:    filepath.Join(l.root, dirName(time.Now().UTC(), reqID)),
		target: &captureTarget{buf: &bytes.Buffer{}},
	}
	ctx = context.WithValue(ctx, captureKey{}, r.target)
	ctx = context.WithValue(ctx, requestKey{}, r)

	if l.immediate() {
		if err := l.mkdir(r.dir); err != nil {
			slog.Error("debuglog: failed to create request directory", "dir", r.dir, "err", err)
		}
	}
	return ctx, r
}

// dirName builds the per-request directory name: a sortable UTC timestamp plus
// the request id, e.g. "20260807T101530.123Z_ab12cd34".
func dirName(t time.Time, reqID string) string {
	name := t.Format("20060102T150405.000Z")
	if reqID == "" {
		return name
	}
	return name + "_" + reqID
}

// Request holds the debug state for a single in-flight request. Obtain one from
// Logger.Begin or FromContext. All methods are safe for concurrent use and are
// no-ops on a nil receiver.
type Request struct {
	logger *Logger
	dir    string
	target *captureTarget // app-log capture buffer

	mu              sync.Mutex
	meta            []byte
	requestBody     []byte
	kiroRequestBody []byte
	rawChunks       bytes.Buffer
	modifiedChunks  bytes.Buffer
}

// Dir reports the directory this request's artifacts are written to.
func (r *Request) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// LogMeta records request metadata (method, path, headers, client) as JSON.
func (r *Request) LogMeta(meta any) {
	if r == nil {
		return
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return
	}
	if r.logger.immediate() {
		r.logger.writeJSONOrRaw(r.dir, fileRequestMeta, body)
		return
	}
	r.mu.Lock()
	r.meta = body
	r.mu.Unlock()
}

// LogRequestBody records the client request body (OpenAI/Anthropic format).
func (r *Request) LogRequestBody(body []byte) {
	if r == nil {
		return
	}
	if r.logger.immediate() {
		r.logger.writeJSONOrRaw(r.dir, fileRequestBody, body)
		return
	}
	r.mu.Lock()
	r.requestBody = cloneBytes(body)
	r.mu.Unlock()
}

// LogKiroRequestBody records the transformed request body sent to the Kiro API.
func (r *Request) LogKiroRequestBody(body []byte) {
	if r == nil {
		return
	}
	if r.logger.immediate() {
		r.logger.writeJSONOrRaw(r.dir, fileKiroRequestBody, body)
		return
	}
	r.mu.Lock()
	r.kiroRequestBody = cloneBytes(body)
	r.mu.Unlock()
}

// LogRawChunk appends a raw response chunk received from the provider.
func (r *Request) LogRawChunk(chunk []byte) {
	if r == nil {
		return
	}
	if r.logger.immediate() {
		r.logger.appendFile(r.dir, fileRawStream, chunk)
		return
	}
	r.mu.Lock()
	r.rawChunks.Write(chunk)
	r.mu.Unlock()
}

// LogModifiedChunk appends a chunk after transformation, as sent to the client.
func (r *Request) LogModifiedChunk(chunk []byte) {
	if r == nil {
		return
	}
	if r.logger.immediate() {
		r.logger.appendFile(r.dir, fileModifiedStream, chunk)
		return
	}
	r.mu.Lock()
	r.modifiedChunks.Write(chunk)
	r.mu.Unlock()
}

// FlushOnError finalizes a failed request. In "all" mode the artifacts were
// already written, so it only adds error_info.json and the captured app logs.
// In "errors" mode it creates the request directory and flushes every buffered
// artifact.
func (r *Request) FlushOnError(statusCode int, errMsg string) {
	if r == nil {
		return
	}
	l := r.logger

	if l.immediate() {
		l.writeErrorInfo(r.dir, statusCode, errMsg)
		r.flushAppLogs()
		slog.Info("debuglog: request failed; artifacts written", "dir", r.dir, "status", statusCode)
		return
	}

	// "errors" mode: snapshot the buffers under the lock.
	r.mu.Lock()
	meta := r.meta
	reqBody := r.requestBody
	kiroBody := r.kiroRequestBody
	raw := cloneBytes(r.rawChunks.Bytes())
	modified := cloneBytes(r.modifiedChunks.Bytes())
	r.mu.Unlock()

	defer r.clearBuffers()

	if err := l.mkdir(r.dir); err != nil {
		slog.Error("debuglog: failed to create request directory for flush", "dir", r.dir, "err", err)
		return
	}

	if len(meta) > 0 {
		l.writeJSONOrRaw(r.dir, fileRequestMeta, meta)
	}
	if len(reqBody) > 0 {
		l.writeJSONOrRaw(r.dir, fileRequestBody, reqBody)
	}
	if len(kiroBody) > 0 {
		l.writeJSONOrRaw(r.dir, fileKiroRequestBody, kiroBody)
	}
	if len(raw) > 0 {
		l.writeFile(r.dir, fileRawStream, raw)
	}
	if len(modified) > 0 {
		l.writeFile(r.dir, fileModifiedStream, modified)
	}

	l.writeErrorInfo(r.dir, statusCode, errMsg)
	r.flushAppLogs()

	slog.Info("debuglog: error logs flushed", "dir", r.dir, "status", statusCode)
}

// Discard finalizes a successful request. In "errors" mode the buffers are
// dropped without touching the disk. In "all" mode the captured app logs are
// written so a successful request's logs are preserved alongside its artifacts.
func (r *Request) Discard() {
	if r == nil {
		return
	}
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
	r.meta = nil
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
	r.logger.writeAppLogs(r.dir, content)
}

// ==================== filesystem helpers (Logger) ====================

// mkdir creates a request directory (and the debug root) if it does not exist.
func (l *Logger) mkdir(dir string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return os.MkdirAll(dir, dirPerm)
}

// writeJSONOrRaw pretty-prints body as JSON when it is valid JSON, otherwise
// writes the raw bytes verbatim.
func (l *Logger) writeJSONOrRaw(dir, name string, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		slog.Error("debuglog: failed to create request directory", "dir", dir, "err", err)
		return
	}
	path := filepath.Join(dir, name)

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
func (l *Logger) writeFile(dir, name string, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		slog.Error("debuglog: failed to create request directory", "dir", dir, "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, filePerm); err != nil {
		slog.Error("debuglog: failed to write debug file", "file", name, "err", err)
	}
}

// appendFile appends data to name, creating it if necessary. Errors are
// swallowed to keep streaming writes best-effort.
func (l *Logger) appendFile(dir, name string, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
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
func (l *Logger) writeErrorInfo(dir string, statusCode int, errMsg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		slog.Error("debuglog: failed to create request directory", "dir", dir, "err", err)
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
	if err := os.WriteFile(filepath.Join(dir, fileErrorInfo), data, filePerm); err != nil {
		slog.Error("debuglog: failed to write error_info", "err", err)
	}
}

// writeAppLogs writes captured application logs. It deliberately avoids slog to
// prevent the capture handler from feeding logs back into the buffer.
func (l *Logger) writeAppLogs(dir, content string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, fileAppLogs), []byte(content), filePerm)
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
// handler and, when debugging is enabled, additionally mirrors it into
//
//   - the process-wide <root>/app_logs.txt (every record, whether or not it
//     carries a request context), and
//   - the capture buffer of the request whose context the record carries, when
//     it carries one (installed by Logger.Begin).
//
// The two are complementary: almost every call site in this codebase logs
// without a context, so the process-wide file is what makes DEBUG_MODE a
// complete log, while the per-request file isolates the records a handler
// deliberately attributed to its request.
type captureHandler struct {
	base  slog.Handler
	owner *Logger
	// mods replays WithAttrs/WithGroup onto the derived sinks so that captured
	// lines carry the same attributes as the base output.
	mods []func(slog.Handler) slog.Handler

	// globalOnce memoizes the composed process-wide sink for this handler
	// instance; instances are created at wiring time, not per record.
	globalOnce sync.Once
	globalSink slog.Handler
}

// NewHandler wraps base so that records are mirrored into the debug log files
// described on captureHandler. base must be non-nil and continues to receive
// every record it would normally handle. On a nil or disabled Logger the result
// is a pass-through, so callers can wire it unconditionally.
func (l *Logger) NewHandler(base slog.Handler) slog.Handler {
	if !l.enabled() {
		return base
	}
	return &captureHandler{base: base, owner: l}
}

// global returns this handler's view of the process-wide sink, with any
// WithAttrs/WithGroup modifications replayed onto it.
func (h *captureHandler) global() slog.Handler {
	h.globalOnce.Do(func() {
		sink := h.owner.globalHandler()
		if sink == nil {
			return
		}
		for _, m := range h.mods {
			sink = m(sink)
		}
		h.globalSink = sink
	})
	return h.globalSink
}

// Enabled reports whether a record at level should be processed. With debugging
// enabled, DEBUG-and-above records are always processed so the debug log is
// complete even when the base handler's level is higher; otherwise it defers to
// the base handler.
func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= slog.LevelDebug {
		return true
	}
	return h.base.Enabled(ctx, level)
}

// Handle forwards the record to the base handler (only when the base would
// actually emit it) and mirrors it into the process-wide sink and the
// per-request capture target when present.
func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	if h.base.Enabled(ctx, r.Level) {
		if err := h.base.Handle(ctx, r); err != nil {
			firstErr = err
		}
	}

	if g := h.global(); g != nil {
		if err := g.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
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
	return &captureHandler{base: h.base.WithAttrs(attrs), owner: h.owner, mods: mods}
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
	return &captureHandler{base: h.base.WithGroup(name), owner: h.owner, mods: mods}
}
