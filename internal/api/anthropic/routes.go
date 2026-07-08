package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/RobinGasi/kiro-proxy/internal/account"
	"github.com/RobinGasi/kiro-proxy/internal/apiauth"
	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/converter"
	"github.com/RobinGasi/kiro-proxy/internal/mcp"
	"github.com/RobinGasi/kiro-proxy/internal/model"
	"github.com/RobinGasi/kiro-proxy/internal/payloadguard"
	"github.com/RobinGasi/kiro-proxy/internal/streaming"
	"github.com/RobinGasi/kiro-proxy/internal/tokenizer"
	"github.com/RobinGasi/kiro-proxy/internal/upstream"
)

// Deps holds the collaborators a Handler needs. All fields are required except
// MCP and Logger; a nil Logger falls back to slog.Default.
type Deps struct {
	Cfg      *config.Config
	Accounts *account.Manager
	Upstream *upstream.Client
	MCP      *mcp.Client
	Logger   *slog.Logger
}

// Handler implements the Anthropic Messages API endpoints.
type Handler struct {
	cfg      *config.Config
	accounts *account.Manager
	upstream *upstream.Client
	mcp      *mcp.Client
	log      *slog.Logger
}

// NewHandler constructs a Handler from its dependencies.
func NewHandler(d Deps) *Handler {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		cfg:      d.Cfg,
		accounts: d.Accounts,
		upstream: d.Upstream,
		mcp:      d.MCP,
		log:      log,
	}
}

// RegisterRoutes registers the Anthropic endpoints on mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", h.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", h.handleCountTokens)
}

// handleMessages implements POST /v1/messages for both streaming and
// non-streaming requests.
func (h *Handler) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !apiauth.CheckKey(r, h.cfg.ProxyAPIKeys) {
		writeError(w, http.StatusUnauthorized, "authentication_error",
			"Invalid or missing API key. Use x-api-key header or Authorization: Bearer.")
		return
	}

	var req MessagesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body: "+err.Error())
		return
	}

	acct, err := h.accounts.Pick(ctx)
	if err != nil {
		h.log.Error("no account available", "error", err)
		writeError(w, http.StatusServiceUnavailable, "api_error", "No available accounts.")
		return
	}
	am := acct.Auth()
	res := acct.Resolver().Resolve(req.Model)

	// Web-search wiring. Decode the raw
	// tools into maps so the mcp helpers — which operate on raw JSON — can detect a
	// native server-side tool (Path A) and inject the emulation tool (Path B).
	var rawTools []map[string]any
	if len(req.Tools) > 0 {
		_ = json.Unmarshal(req.Tools, &rawTools)
	}

	// Path A — native, client-supplied server-side web_search tool: fulfill it
	// directly via the Kiro MCP API and return, bypassing the model. This works
	// regardless of cfg.WebSearchEnabled (native tools always work).
	if h.mcp != nil && mcp.HasNativeWebSearch(rawTools) {
		h.log.Info("anthropic: native web_search (Path A)")
		query := mcp.ExtractQueryFromText(lastUserText(&req))
		if err := h.mcp.HandleNativeWebSearch(ctx, w, am, mcp.NativeWebSearchParams{
			Model:     req.Model,
			Query:     query,
			Messages:  messagesAsMaps(&req),
			Stream:    req.Stream,
			APIFormat: mcp.APIFormatAnthropic,
		}); err != nil {
			h.log.Error("anthropic: native web_search response write failed", "error", err)
		}
		return
	}

	// Path B — MCP emulation: inject the web_search tool so the model can call it,
	// and rewrite req.Tools so the injected tool reaches Kiro. The model's
	// web_search tool_use is then intercepted in the streaming layer (see
	// handleToolUse / interceptWebSearch).
	if h.cfg.WebSearchEnabled {
		if injected := mcp.InjectAnthropicWebSearchTool(rawTools, true); len(injected) != len(rawTools) {
			rawTools = injected
			if b, err := json.Marshal(rawTools); err == nil {
				req.Tools = b
			}
		}
	}

	convReq := buildConverterRequest(&req)

	// toolNameReverse restores any tool names that BuildKiroPayload aliases to
	// satisfy Kiro's 64-character limit (nil when nothing was shortened).
	payload, toolNameReverse, err := converter.BuildKiroPayload(ctx, h.cfg, convReq, am.ProfileARN(), res.InternalID)
	if err != nil {
		var tooLarge *payloadguard.PayloadTooLargeError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", tooLarge.Error())
		case errors.Is(err, converter.ErrNoMessages):
			writeError(w, http.StatusBadRequest, "invalid_request_error", "No messages to send.")
		default:
			h.log.Error("failed to build kiro payload", "error", err)
			writeError(w, http.StatusInternalServerError, "api_error", "Failed to build upstream request.")
		}
		return
	}

	start := func(ctx context.Context) (*http.Response, error) {
		return h.upstream.SendConversation(ctx, am, payload, req.Stream)
	}

	contextLimit := model.ContextLimitFor(res.Normalized, h.cfg.DefaultMaxInputTokens)
	asReasoning := asReasoningContent(h.cfg.FakeReasoningHandling)

	if req.Stream {
		h.serveStreaming(ctx, w, &req, acct, am, contextLimit, asReasoning, toolNameReverse, start)
		return
	}
	h.serveNonStreaming(ctx, w, &req, acct, contextLimit, asReasoning, toolNameReverse, start)
}

// serveStreaming drives the SSE response path.
func (h *Handler) serveStreaming(ctx context.Context, w http.ResponseWriter, req *MessagesRequest, acct *account.Account, am mcp.AuthManager, contextLimit int, asReasoning bool, toolNameReverse map[string]string, start func(context.Context) (*http.Response, error)) {
	inputTokens := h.estimateInputTokens(req, false)

	evCh, errCh, err := streaming.StreamWithFirstTokenRetry(ctx, h.cfg, start)
	if err != nil {
		h.log.Error("failed to start stream", "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", "Failed to start stream.")
		return
	}

	committed, streamErr := h.streamMessages(ctx, w, am, req.Model, inputTokens, contextLimit, asReasoning, toolNameReverse, evCh, errCh)
	if streamErr == nil {
		h.accounts.RecordSuccess(acct)
		return
	}

	status, reason, message := classifyUpstreamError(streamErr)
	h.accounts.RecordFailure(acct, status, reason)
	if !committed {
		writeError(w, status, "api_error", message)
		return
	}
	// The response is already a committed event stream; the error event has been
	// emitted by streamMessages. Nothing more can be written.
	h.log.Warn("stream failed after commit", "status", status, "reason", reason, "error", streamErr)
}

// serveNonStreaming drives the single-JSON-response path.
func (h *Handler) serveNonStreaming(ctx context.Context, w http.ResponseWriter, req *MessagesRequest, acct *account.Account, contextLimit int, asReasoning bool, toolNameReverse map[string]string, start func(context.Context) (*http.Response, error)) {
	inputTokens := h.estimateInputTokens(req, false)

	evCh, errCh, err := streaming.StreamWithFirstTokenRetry(ctx, h.cfg, start)
	if err != nil {
		h.log.Error("failed to start stream", "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", "Failed to start upstream request.")
		return
	}

	result, err := streaming.CollectStreamResult(evCh, errCh)
	if err != nil {
		status, reason, message := classifyUpstreamError(err)
		h.accounts.RecordFailure(acct, status, reason)
		writeError(w, status, "api_error", message)
		return
	}

	resp := buildMessagesResponse(result, req.Model, inputTokens, contextLimit, asReasoning, toolNameReverse)
	h.accounts.RecordSuccess(acct)
	// Mirror the OpenAI surface: surface remaining credits as a response header
	// when the upstream usage payload carries them (see openai serveNonStream).
	if rem, ok := creditsRemaining(result.Usage); ok {
		w.Header().Set("x-kiro-credits-remaining", rem)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCountTokens implements POST /v1/messages/count_tokens, returning an
// estimated input token count without contacting the upstream API.
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !apiauth.CheckKey(r, h.cfg.ProxyAPIKeys) {
		writeError(w, http.StatusUnauthorized, "authentication_error",
			"Invalid or missing API key. Use x-api-key header or Authorization: Bearer.")
		return
	}

	var req MessagesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body: "+err.Error())
		return
	}

	// Mirror the Anthropic streaming message_start estimation with the Claude
	// correction enabled (Claude Code uses this to decide on compaction).
	inputTokens := h.estimateInputTokens(&req, true)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": inputTokens})
}

// estimateInputTokens approximates the prompt token count of a request,
// accounting for messages, tools, and the (role-normalized) system prompt.
func (h *Handler) estimateInputTokens(req *MessagesRequest, applyCorrection bool) int {
	norm := req.NormalizeRoles()

	var msgs []map[string]any
	if b, err := json.Marshal(req.Messages); err == nil {
		_ = json.Unmarshal(b, &msgs)
	}
	var tools []map[string]any
	if len(req.Tools) > 0 {
		_ = json.Unmarshal(req.Tools, &tools)
	}

	est := tokenizer.EstimateRequestTokens(msgs, tools, norm.System, applyCorrection)
	return est.TotalTokens
}

// lastUserText returns the text of the last user message in the request, used to
// derive the web_search query for Path A. It falls back to the last message of
// any role when no user message is present.
func lastUserText(req *MessagesRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content.Text()
		}
	}
	if n := len(req.Messages); n > 0 {
		return req.Messages[n-1].Content.Text()
	}
	return ""
}

// messagesAsMaps renders the request messages as plain maps for the MCP
// token-counting path (HandleNativeWebSearch). A marshal/unmarshal round-trip
// keeps this faithful to the wire shape without re-implementing the conversion.
func messagesAsMaps(req *MessagesRequest) []map[string]any {
	var msgs []map[string]any
	if b, err := json.Marshal(req.Messages); err == nil {
		_ = json.Unmarshal(b, &msgs)
	}
	return msgs
}

// classifyUpstreamError maps a terminal stream/collection error to an HTTP
// status, a Kiro error reason (for circuit-breaker classification), and a
// client-facing message.
func classifyUpstreamError(err error) (status int, reason string, message string) {
	var statusErr *streaming.UpstreamStatusError
	if errors.As(err, &statusErr) {
		msg, rsn := parseKiroError(statusErr.Body)
		if msg == "" {
			msg = "Upstream API error."
		}
		return statusErr.StatusCode, rsn, msg
	}

	var tooLarge *payloadguard.PayloadTooLargeError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge, "", tooLarge.Error()
	}

	return http.StatusInternalServerError, "", err.Error()
}

// parseKiroError extracts the message and reason from a Kiro error body, which is
// typically {"message": "...", "reason": "..."}.
func parseKiroError(body string) (message, reason string) {
	if body == "" {
		return "", ""
	}
	var k struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal([]byte(body), &k) == nil {
		return k.Message, k.Reason
	}
	return body, ""
}

// decodeJSON leniently decodes the request body into v, tolerating unknown
// fields. Any trailing bytes are drained so the connection can be reused.
func decodeJSON(r *http.Request, v any) error {
	err := json.NewDecoder(r.Body).Decode(v)
	_, _ = io.Copy(io.Discard, r.Body)
	return err
}

// writeJSON writes v as a JSON body with the given status, without HTML escaping
// so '<'/'>' in content survive intact.
func writeJSON(w http.ResponseWriter, status int, v any) {
	payload, err := marshalNoEscape(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// writeError writes an Anthropic-format error envelope with the given status.
func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, errorResponse{
		Type:  "error",
		Error: errorBody{Type: errType, Message: message},
	})
}
